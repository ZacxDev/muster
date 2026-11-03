package mirror

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Pending is WHAT a waiting Claude Code session is waiting on, as its host
// read it from the session's transcript (internal/mirrorhost's PendingOf):
// what the attention queue shows instead of a generic "waiting" line.
//
//   - Summary is the pending action in one short line: the last tool call that
//     has no result yet ("Bash: npm publish", "Edit internal/api/x.go", the
//     question an AskUserQuestion asks), "" when the transcript shows none.
//   - Tool is that call's tool as a NAME only ("Bash", "Edit", "MCP
//     server/tool"), "" when there is none: what a notification may say.
//   - Last is the last thing Claude said (its last text), whitespace-collapsed.
//
// 🔴 TEXT FROM THE SESSION, SHOWN ON A PAGE: every field is capped in RUNES
// (MaxPendingSummary, MaxPendingTool, MaxPendingLast), stripped of every
// control and format character and whitespace-collapsed (CleanPending) — by
// the host before sending and by muster again on receipt. The host also
// redacts what looks like a secret before it sends (internal/redact's
// Secrets, called by internal/mirrorhost). Only Tool ever reaches a lock screen: the summary and the
// last words are shown on the queue page, never in a push.
//
// 🔴 BOUNDED LIST-WIDE, LIKE OtherClaude: only a window that is Claude and
// StatusWaiting keeps one, and at most MaxPendingWindows windows of a list
// (the most recently waiting) carry one, so the window list's worst case grows
// by a fixed ~17 KiB of the frame's last few percent: 1,030,215 of 1,048,576
// bytes with it (TestTheLargestWindowListFitsOneFrame encodes it at its bound).
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster ignores
// the field (unknown JSON key); an older host never sends it, and muster then
// shows its own generic line.
type Pending struct {
	Summary string `json:"summary,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Last    string `json:"last,omitempty"`
	// THE HERMES ASK (Hermes Phase 4, docs/design/hermes.md, "Questions and
	// approvals"): a waiting HERMES window's Pending also says which ask it
	// waits on, and for an approval the one its host can answer. Never on a
	// Claude window (boundPending drops them there), so a Claude window's
	// bytes on the wire are unchanged; an older muster ignores them, and an
	// older host never sends them (muster then shows its generic line).
	//
	// Kind is PendingApproval, PendingClarify or PendingSudo; "" otherwise.
	Kind string `json:"kind,omitempty"`
	// Detail is an approval's description (why Hermes asks), capped at
	// MaxPendingDetail like the rest.
	Detail string `json:"detail,omitempty"`
	// PromptID is the approval's id in Hermes's gateway — what an answer
	// names, so muster can only answer the approval it SHOWED. "" when the
	// host could not read the approval (its gateway viewer could not attach):
	// then there is nothing to answer here, and the card says so.
	PromptID string `json:"promptId,omitempty"`
	// Choices are the answers muster may send for PromptID, space-separated:
	// a subset of HermesApprovalChoices, in that order ("once deny"). Empty
	// without a PromptID. (A string, not a slice, so a Pending stays
	// comparable with ==.)
	//
	// An OPENCODE ask (PendingPermission, PendingQuestion) names its request
	// or form the same way: PromptID is opencode's id for it ("per…",
	// "frm_…"), and a permission's Choices a subset of
	// OpenCodePermissionChoices.
	Choices string `json:"choices,omitempty"`
	// Options are an opencode QUESTION's option labels, one per line, when
	// muster can answer it (one single-select question of 1..MaxPendingOptions
	// options, each label within MaxPendingOption runes and unchanged by
	// cleaning — an answer is the label itself, so a cut one could not be
	// sent back). "" otherwise: the question is then the terminal's.
	Options string `json:"options,omitempty"`
}

// Offers reports whether p offers choice as an answer: one of its Choices,
// or — for an opencode question — one of its Options.
func (p Pending) Offers(choice string) bool {
	if choice == "" {
		return false
	}
	if p.Kind == PendingQuestion {
		return slices.Contains(p.OptionList(), choice)
	}
	return slices.Contains(strings.Fields(p.Choices), choice)
}

// OptionList is p's Options, one label each; nil for none.
func (p Pending) OptionList() []string {
	if p.Options == "" {
		return nil
	}
	return strings.Split(p.Options, "\n")
}

// The Hermes asks a Pending names (Pending.Kind), as Hermes's status hook
// records them (S2).
const (
	PendingApproval = "approval"
	PendingClarify  = "clarify"
	PendingSudo     = "sudo"
)

// The opencode asks a Pending names (docs/design/opencode.md, "Asks"): a
// permission request, and a form its question tool raised. Only an opencode
// window's (RuntimeSpec.AskKinds).
const (
	PendingPermission = "permission"
	PendingQuestion   = "question"
)

// OpenCodePermissionChoices are the answers muster ever sends an opencode
// permission request: this one call, or no. `always` widens the session's
// rules past this call, so muster never offers it (as HermesApprovalChoices
// leaves out `session` and `always`).
var OpenCodePermissionChoices = []string{"once", "reject"}

// openCodeIDPattern is an opencode permission request or form id as muster
// carries it: "per" or "frm_" and up to 125 more bytes of [0-9A-Za-z_-]
// (internal/opencodehost's ValidRequestID and ValidFormID, which a test pins
// to agree).
var openCodeIDPattern = regexp.MustCompile(`^(per[0-9A-Za-z_-]{1,125}|frm_[0-9A-Za-z_-]{1,124})$`)

// ValidOpenCodeAskID reports whether id is an opencode permission request or
// form id.
func ValidOpenCodeAskID(id string) bool { return openCodeIDPattern.MatchString(id) }

// HermesApprovalChoices are the answers muster ever sends a Hermes approval:
// the moment being approved, or no. `session` and `always` widen what the
// agent may do beyond it (docs/design/hermes.md, open question 1), so muster
// never offers them.
var HermesApprovalChoices = []string{"once", "deny"}

// promptIDPattern is a Hermes request id — an approval's prompt id, a status
// file's request id — as Hermes mints it (uuid4().hex): THE one rule for it
// (internal/hermeshost uses ValidPromptID too).
var promptIDPattern = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

// ValidPromptID reports whether id is a Hermes request or approval id.
func ValidPromptID(id string) bool { return promptIDPattern.MatchString(id) }

// The Pending bounds.
const (
	// MaxPendingSummary bounds Pending.Summary, in runes.
	MaxPendingSummary = 120
	// MaxPendingTool bounds Pending.Tool, in runes.
	MaxPendingTool = 40
	// MaxPendingLast bounds Pending.Last, in runes.
	MaxPendingLast = 200
	// MaxPendingWindows bounds how many windows of ONE list carry a Pending.
	MaxPendingWindows = 8
	// MaxPendingDetail bounds Pending.Detail, in runes.
	MaxPendingDetail = 120
	// MaxPendingOptions bounds how many options an opencode question offers
	// in muster, and MaxPendingOption each one's runes. With Last and Detail
	// left off a question, its Pending is never larger on the wire than a
	// Claude window's at its bounds (TestAnOpenCodePendingIsNoLargerThanAClaudeOne),
	// so the frame bound measured for those holds (internal/mirrorhost's
	// TestTheLargestWindowListFitsOneFrame).
	MaxPendingOptions = 4
	MaxPendingOption  = 40
)

// CleanPending is p as the wire carries it: each field through cleanLine and
// capped at its rune bound (a cut ends "…"). nil when nothing is left of any
// field.
func CleanPending(p *Pending) *Pending {
	if p == nil {
		return nil
	}
	out := Pending{Summary: cleanLine(p.Summary, MaxPendingSummary), Tool: cleanLine(p.Tool, MaxPendingTool), Last: cleanLine(p.Last, MaxPendingLast)}
	switch p.Kind {
	case PendingApproval, PendingClarify, PendingSudo:
		out.Kind = p.Kind
		out.Detail = cleanLine(p.Detail, MaxPendingDetail)
	case PendingPermission, PendingQuestion:
		// An opencode ask has no "last words" (Last is Claude Code's), and a
		// question no Detail: its bytes go to its options instead.
		out.Kind, out.Last = p.Kind, ""
		if p.Kind == PendingPermission {
			out.Detail = cleanLine(p.Detail, MaxPendingDetail)
		}
		if ValidOpenCodeAskID(p.PromptID) {
			switch {
			case p.Kind == PendingPermission && strings.HasPrefix(p.PromptID, "per"):
				var cs []string
				for _, c := range OpenCodePermissionChoices {
					if p.Offers(c) {
						cs = append(cs, c)
					}
				}
				if len(cs) > 0 {
					out.PromptID, out.Choices = p.PromptID, strings.Join(cs, " ")
				}
			case p.Kind == PendingQuestion && strings.HasPrefix(p.PromptID, "frm_"):
				if opts, ok := cleanOptions(p.OptionList()); ok {
					out.PromptID, out.Options = p.PromptID, strings.Join(opts, "\n")
				}
			}
		}
	}
	// An answer is offered only for an approval muster can name, and only
	// with a choice muster ever sends.
	if out.Kind == PendingApproval && ValidPromptID(p.PromptID) {
		var cs []string
		for _, c := range HermesApprovalChoices {
			if p.Offers(c) {
				cs = append(cs, c)
			}
		}
		if len(cs) > 0 {
			out.PromptID, out.Choices = p.PromptID, strings.Join(cs, " ")
		}
	}
	if out.Summary == "" && out.Tool == "" && out.Last == "" && out.Kind == "" {
		return nil
	}
	return &out
}

// cleanOptions is opts as an opencode question offers them in muster: all of
// them or none — 1..MaxPendingOptions labels, each unchanged by cleanLine at
// MaxPendingOption runes (an answer IS the label, so one cleaning changed
// could not be sent back) and unique.
func cleanOptions(opts []string) ([]string, bool) {
	if len(opts) == 0 || len(opts) > MaxPendingOptions {
		return nil, false
	}
	seen := map[string]bool{}
	for _, o := range opts {
		if o == "" || cleanLine(o, MaxPendingOption) != o || seen[o] {
			return nil, false
		}
		seen[o] = true
	}
	return opts, true
}

// CleanOption reports whether label is one an opencode question can offer
// in muster as it is (cleanOptions' per-label rule): the host checks each
// label with it before it offers any.
func CleanOption(label string) bool {
	return label != "" && cleanLine(label, MaxPendingOption) == label
}

// cleanLine is s as one phone-safe line: invalid UTF-8 dropped; every control
// character (C0, DEL, C1) and every Unicode space or line/paragraph separator
// turned into a space; every FORMAT character (Unicode category Cf: the bidi
// embeddings and isolates U+202A–202E and U+2066–2069, the zero-width
// characters, U+FEFF) removed, so none can reorder or hide what is shown;
// whitespace collapsed; at most max runes (a cut ends "…").
func cleanLine(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cf, r):
			return -1
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\u2028' || r == '\u2029' || unicode.IsSpace(r):
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = strings.TrimRight(string([]rune(s)[:max-1]), " ") + "…"
	}
	return s
}

// boundPending applies the Pending rules to a normalised list: none on a
// window that is not a waiting pane of a runtime with Prompts, each cleaned
// as its runtime keeps it (a Claude window's without the named ask's
// fields), and at most MaxPendingWindows kept — the windows that started waiting most recently,
// then list order — so which keep theirs does not depend on the host's order.
func boundPending(out []Window) {
	var with []int
	for i := range out {
		r, ok := out[i].Spec()
		if !ok || !r.Caps.Prompts || out[i].Status != StatusWaiting {
			out[i].Pending = nil
			continue
		}
		// A Claude window's Pending is what it always was (the named ask's
		// fields are another runtime's); a Hermes window's names its ask, or
		// it is none (RuntimeSpec.PendingNamesAsk).
		out[i].Pending = r.cleanPending(out[i].Pending)
		if out[i].Pending != nil {
			with = append(with, i)
		}
	}
	if len(with) <= MaxPendingWindows {
		return
	}
	sort.SliceStable(with, func(a, b int) bool { return out[with[a]].StatusSince > out[with[b]].StatusSince })
	for _, i := range with[MaxPendingWindows:] {
		out[i].Pending = nil
	}
}

// pendingEqual reports whether a and b carry the same Pending.
func pendingEqual(a, b *Pending) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// OneLine is s as one short, phone-safe line: control characters as spaces,
// whitespace collapsed, at most max runes (a cut ends "…"). The rule
// CleanPending applies to each field, for any other excerpt shown beside it.
func OneLine(s string, max int) string { return cleanLine(s, max) }
