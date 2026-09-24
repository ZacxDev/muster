// Package runbooks is the persistence domain for runbooks: user-defined,
// parameterized, privilege-aware dispatch templates. A runbook captures a
// reusable agent dispatch — its body template (with {{param}} placeholders), the
// parameters filled in at dispatch time, a default repo/model, and the privilege
// profiles to auto-grant — so a standard procedure can be run again with one
// click instead of being retyped into the dispatch modal each time.
//
// Phase 4.1 (this) is the registry + parameterized dispatch: rendering the body,
// creating the task note, dispatching the agent, and granting the profiles.
// Structured steps and approval checkpoints (Spec.Steps) are modelled here but
// rendered as a checklist into the body; hard enforcement is a later phase.
package runbooks

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ParamDef declares a parameter a runbook accepts, filled at dispatch time and
// substituted into the body template wherever {{name}} appears.
type ParamDef struct {
	Name        string   `json:"name"`
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"` // when set, the dispatch form renders a dropdown
}

// Step is one item in a runbook's procedure. In Phase 4.1 steps render into the
// body as a numbered checklist the agent works through; RequiresApproval is
// reserved for the later hard-checkpoint phase.
type Step struct {
	Title            string `json:"title"`
	Instructions     string `json:"instructions,omitempty"`
	RequiresApproval bool   `json:"requiresApproval,omitempty"`
}

// Spec is the body of a runbook: everything needed to render and dispatch it.
type Spec struct {
	DefaultRepo     string     `json:"defaultRepo,omitempty"`
	DefaultBranch   string     `json:"defaultBranch,omitempty"`
	Model           string     `json:"model,omitempty"`
	Params          []ParamDef `json:"params,omitempty"`
	BodyTemplate    string     `json:"bodyTemplate"`
	Steps           []Step     `json:"steps,omitempty"`
	GrantProfiles   []string   `json:"grantProfiles,omitempty"` // privilege profile names to auto-grant
	SuccessCriteria string     `json:"successCriteria,omitempty"`
}

// Runbook is a reusable, named dispatch template.
type Runbook struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Description string    `json:"description"`
	Spec        Spec      `json:"spec"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Run is an audit record of a runbook dispatch: which agent (+ task note) it
// created, with a snapshot of the params and rendered body at dispatch time.
type Run struct {
	ID           int64             `json:"id"`
	RunbookID    *int64            `json:"runbookId,omitempty"`
	RunbookName  string            `json:"runbookName"`
	AgentID      int64             `json:"agentId"`
	NoteID       *int64            `json:"noteId,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
	RenderedBody string            `json:"renderedBody"`
	CreatedAt    time.Time         `json:"createdAt"`
}

// Store is the runbooks persistence behaviour: the registry plus a dispatch
// audit trail. Postgres-backed (mirrors privilege.Store); no in-memory variant.
type Store interface {
	CreateRunbook(ctx context.Context, rb Runbook) (Runbook, error)
	ListRunbooks(ctx context.Context) ([]Runbook, error)
	GetRunbook(ctx context.Context, id int64) (Runbook, error)
	GetRunbookByName(ctx context.Context, name string) (Runbook, error)
	DeleteRunbook(ctx context.Context, id int64) error

	CreateRun(ctx context.Context, r Run) (Run, error)
	ListRunsForRunbook(ctx context.Context, runbookID int64) ([]Run, error)
	// LatestRunForAgent returns the most recent run that dispatched the given
	// agent (an agent is dispatched by at most one runbook run, but the newest
	// wins if an id were ever reused). The bool is false when the agent has no
	// recorded run — i.e. it was an ad-hoc/non-runbook dispatch. Used to attribute
	// a checkpoint decision back to its runbook for acceptance grouping.
	LatestRunForAgent(ctx context.Context, agentID int64) (Run, bool, error)
	// RunStatsForRunbooks returns, per runbook id, the run count and most-recent
	// run time, in one batched query — for the runbook cards' count/last-run badge.
	RunStatsForRunbooks(ctx context.Context, runbookIDs []int64) (map[int64]RunStats, error)
	// DeleteRunsOlderThan deletes runbook_runs created before cutoff and returns
	// the number removed (retention sweep).
	DeleteRunsOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// RunStats is a runbook's dispatch summary: how many runs it has and when the
// most recent one was recorded.
type RunStats struct {
	Count     int
	LastRunAt time.Time
}

// placeholderFor matches {{name}} with optional surrounding whitespace.
func placeholderFor(name string) *regexp.Regexp {
	return regexp.MustCompile(`\{\{\s*` + regexp.QuoteMeta(name) + `\s*\}\}`)
}

// Validate reports the first problem with a set of param values: a required
// parameter that is neither supplied nor defaulted. Unknown values are ignored
// (a caller may pass extras); undeclared {{placeholders}} are left literal.
func (s Spec) Validate(values map[string]string) error {
	var missing []string
	for _, p := range s.Params {
		if !p.Required {
			continue
		}
		if strings.TrimSpace(values[p.Name]) == "" && strings.TrimSpace(p.Default) == "" {
			missing = append(missing, p.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required parameter(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// Render substitutes the supplied param values into the body template and
// appends the steps checklist + success criteria, after validating that all
// required params are present. An empty value falls back to the param's Default.
func (rb Runbook) Render(values map[string]string) (string, error) {
	if err := rb.Spec.Validate(values); err != nil {
		return "", err
	}
	body := rb.Spec.BodyTemplate
	for _, p := range rb.Spec.Params {
		v := strings.TrimSpace(values[p.Name])
		if v == "" {
			v = p.Default
		}
		body = placeholderFor(p.Name).ReplaceAllLiteralString(body, v)
	}
	body = strings.TrimRight(body, "\n")

	if len(rb.Spec.Steps) > 0 {
		var b strings.Builder
		b.WriteString(body)
		b.WriteString("\n\n## Steps\n")
		for i, st := range rb.Spec.Steps {
			fmt.Fprintf(&b, "%d. %s", i+1, st.Title)
			if st.RequiresApproval {
				b.WriteString(" _(checkpoint: call agent_checkpoint with a summary and wait for approval before doing this step)_")
			}
			b.WriteString("\n")
			if strings.TrimSpace(st.Instructions) != "" {
				fmt.Fprintf(&b, "   %s\n", st.Instructions)
			}
		}
		body = strings.TrimRight(b.String(), "\n")
	}

	if sc := strings.TrimSpace(rb.Spec.SuccessCriteria); sc != "" {
		body += "\n\n## Done when\n" + sc
	}
	return body, nil
}
