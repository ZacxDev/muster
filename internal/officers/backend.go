package officers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/ZacxDev/muster/internal/askimage"
)

// Backend is the officer-backend contract (officer-backend/v1,
// docs/design/officers-first-class.md): the CORE verbs that run an officer —
// its queue, leases, brief and on/off switch — for ONE officer. Every caller
// in muster (the wake loop, the MCP tools, the ship gates, the transcript,
// usage and pools pushers) reaches an officer's backend through this
// interface and the optional ones below, never through a concrete client.
//
// *Client (HQ over HTTPS with the officer's token) is one implementation; a
// built-in one in muster's own Postgres is planned (muster#503 slice 4), and
// a conformance suite both must pass (slice 3).
//
// Errors are the contract's three kinds, whatever the implementation: a
// decision is *HQError (status + the backend's {code, …} JSON body: 404 no
// such item, 409 held / lease_lost / wrong_tier / blocked, 410 gone, 400 a
// refused argument); a failure the route says to retry is *RetryableError; an
// outage wraps ErrHQUnavailable. Callers branch on those, never on a message.
//
// Bodies ([]byte) are the backend's JSON, passed to the instance unchanged.
type Backend interface {
	// Officer is the officer this backend acts for.
	Officer() Officer
	// Capabilities is what this backend serves beyond the core verbs.
	Capabilities() Capabilities
	Items(ctx context.Context, q ItemsQuery) (ItemsPage, error)
	// AllItems reads up to maxItemPages pages of items in status.
	AllItems(ctx context.Context, status string) ([]Item, error)
	Brief(ctx context.Context) ([]byte, error)
	Claim(ctx context.Context, item ItemID, instance string, leaseSeconds int, tier string) ([]byte, error)
	ClaimNext(ctx context.Context, instance string, leaseSeconds int, tier string) ([]byte, error)
	Heartbeat(ctx context.Context, item ItemID, instance string, leaseSeconds int) ([]byte, error)
	Complete(ctx context.Context, item ItemID, instance string, done Completion) ([]byte, error)
	Release(ctx context.Context, item ItemID, instance string) ([]byte, error)
	// Enabled is the officer's on/off switch; an unreadable switch is an
	// error, never "enabled".
	Enabled(ctx context.Context) (bool, error)
	// Scrub is s with every credential this backend holds for the officer
	// removed (HQ: its token). A backend that holds none returns s.
	Scrub(s string) string
}

// The optional verbs. A backend serves one only when it implements the
// interface AND advertises its capability (Serves checks both).
type (
	// TranscriptBackend stores an instance's transcript of a held item.
	TranscriptBackend interface {
		PostTranscript(ctx context.Context, item ItemID, instance string, events []TranscriptEvent) (TranscriptAck, error)
	}
	// UsageBackend takes an instance's Claude usage per session and hour.
	UsageBackend interface {
		PutUsage(ctx context.Context, r UsageReport) error
	}
	// PoolsBackend takes the officer's declared pools.
	PoolsBackend interface {
		PutPools(ctx context.Context, r PoolsReport) error
	}
	// AsksBackend asks a named person about a held item (with images).
	AsksBackend interface {
		CreateAsk(ctx context.Context, item ItemID, instance string, a Ask) ([]byte, error)
		UploadAskImage(ctx context.Context, filename, contentType string, data []byte) (askimage.Record, error)
	}
	// TaskAsksBackend files a task-reconciler's derived ask.
	TaskAsksBackend interface {
		CreateTaskAsk(ctx context.Context, instance string, a TaskAskArgs) ([]byte, error)
	}
	// FloodBlocksBackend files a web-performance officer's flood-block proposal.
	FloodBlocksBackend interface {
		ProposeFloodBlock(ctx context.Context, instance string, p FloodBlockProposal) ([]byte, error)
	}
	// GapTasksBackend files a datastore officer's gap task.
	GapTasksBackend interface {
		FileGapTask(ctx context.Context, instance string, a GapTaskArgs) ([]byte, error)
	}
	// TasksBackend reads tasks (a task-reconciler's).
	TasksBackend interface {
		TaskList(ctx context.Context, query string) ([]byte, error)
		TaskGet(ctx context.Context, id string) ([]byte, error)
	}
	// PRContextBackend reads a pull request's context for attribution.
	PRContextBackend interface {
		PRContext(ctx context.Context, a PRContextArgs) ([]byte, error)
	}
	// ObsBackend runs the backend's NAMED observability queries.
	ObsBackend interface {
		ObsQueries(ctx context.Context) ([]byte, error)
		ObsQuery(ctx context.Context, args json.RawMessage) ([]byte, error)
	}
	// ObsRawBackend runs an instance's own PromQL / LogQL for a held item.
	ObsRawBackend interface {
		ObsRaw(ctx context.Context, body json.RawMessage) ([]byte, error)
	}
	// CaptainBackend takes a release officer's evaluation (extension x-captain).
	CaptainBackend interface {
		ReleaseCaptain(ctx context.Context, instance string, p CaptainPost) ([]byte, error)
	}
	// ShipBackend is what a shipping officer's gates need of the backend
	// beyond the switch: the sensitive-path notice and the backup preflight;
	// and for the PR review loop, the officer's shadow switch (the fallback
	// when an item carries no shadow of its own), the "PR opened" report
	// muster makes after a clean draft create, and a shepherd's merge ask.
	ShipBackend interface {
		PostNotice(ctx context.Context, path string, n Notice) error
		Preflight(ctx context.Context) (Preflight, error)
		Shadow(ctx context.Context) (bool, error)
		PROpened(ctx context.Context, p PROpened) (PROpenedAnswer, error)
		MergeAsk(ctx context.Context, item ItemID, body []byte) ([]byte, error)
	}
)

// ContractV1 is the contract version this muster speaks.
const ContractV1 = "officer-backend/v1"

// The capabilities: the optional verbs, plus the core-verb features a backend
// may lack. Each optional verb's capability names the interface above that
// serves it (capabilityInterface).
const (
	CapTiers         = "tiers"         // routine/judgement claims, ?tier=, needs_judgement
	CapInvestigation = "investigation" // the investigation block on complete
	CapCost          = "cost"          // cost_usd + cost_from on complete
	CapTranscript    = "transcript"
	CapUsage         = "usage"
	CapPools         = "pools"
	CapAsks          = "asks"
	CapTaskAsks      = "task-asks"
	CapFloodBlocks   = "flood-blocks"
	CapGapTasks      = "gap-tasks"
	CapTasksRead     = "tasks-read"
	CapPRContext     = "pr-context"
	CapObs           = "obs"
	CapObsRaw        = "obs.raw"
	CapCaptain       = "x-captain"
	CapShip          = "ship" // notices + preflight, for the ship gates
)

// capabilityInterface is THE map from an optional verb's capability to the
// interface that serves it. A capability not here (tiers, investigation,
// cost) is a feature of the core verbs: until the first narrower backend is
// wired (slice 5) nothing branches on those three — they are vocabulary. A
// conformance suite asserts a backend
// implements every interface it advertises.
var capabilityInterface = map[string]func(Backend) bool{
	CapTranscript:  func(b Backend) bool { _, ok := b.(TranscriptBackend); return ok },
	CapUsage:       func(b Backend) bool { _, ok := b.(UsageBackend); return ok },
	CapPools:       func(b Backend) bool { _, ok := b.(PoolsBackend); return ok },
	CapAsks:        func(b Backend) bool { _, ok := b.(AsksBackend); return ok },
	CapTaskAsks:    func(b Backend) bool { _, ok := b.(TaskAsksBackend); return ok },
	CapFloodBlocks: func(b Backend) bool { _, ok := b.(FloodBlocksBackend); return ok },
	CapGapTasks:    func(b Backend) bool { _, ok := b.(GapTasksBackend); return ok },
	CapTasksRead:   func(b Backend) bool { _, ok := b.(TasksBackend); return ok },
	CapPRContext:   func(b Backend) bool { _, ok := b.(PRContextBackend); return ok },
	CapObs:         func(b Backend) bool { _, ok := b.(ObsBackend); return ok },
	CapObsRaw:      func(b Backend) bool { _, ok := b.(ObsRawBackend); return ok },
	CapCaptain:     func(b Backend) bool { _, ok := b.(CaptainBackend); return ok },
	CapShip:        func(b Backend) bool { _, ok := b.(ShipBackend); return ok },
}

// AllCapabilities is every capability this muster knows, sorted.
func AllCapabilities() []string {
	out := []string{CapTiers, CapInvestigation, CapCost}
	for c := range capabilityInterface {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Capabilities is what a backend serves beyond the core verbs.
type Capabilities struct {
	Contract string
	// Verbs are the capability names, sorted.
	Verbs []string
}

// NewCapabilities is the v1 capabilities naming verbs (sorted, deduplicated).
func NewCapabilities(verbs ...string) Capabilities {
	v := append([]string(nil), verbs...)
	sort.Strings(v)
	return Capabilities{Contract: ContractV1, Verbs: slices.Compact(v)}
}

// Has reports whether c advertises capability v.
func (c Capabilities) Has(v string) bool {
	_, ok := slices.BinarySearch(c.Verbs, v)
	return ok
}

// Unimplemented is every capability b advertises without implementing the
// interface that serves it (none: nil): a backend that does is broken, and
// Unimplemented names it.
func Unimplemented(b Backend) []string {
	var out []string
	for _, v := range b.Capabilities().Verbs {
		if impl, optional := capabilityInterface[v]; optional && !impl(b) {
			out = append(out, v)
		}
	}
	return out
}

// ErrNotServed is a verb the officer's backend does not serve: it does not
// advertise the capability, or does not implement it. A decision (never
// retried), reported to an instance as a refusal.
var ErrNotServed = errors.New("officers: this officer's backend does not serve that verb")

// Serves is b as the optional interface T when b advertises capability and
// implements T; ErrNotServed (naming the capability) otherwise.
func Serves[T any](b Backend, capability string) (T, error) {
	var zero T
	t, ok := any(b).(T)
	if !ok || !b.Capabilities().Has(capability) {
		return zero, fmt.Errorf("%w (%s)", ErrNotServed, capability)
	}
	return t, nil
}

// ToolCapability is THE map from an officer MCP tool to the backend
// capability it needs: "" for a core-verb tool or one muster serves without
// the backend (kube reads; CI logs, though muster reads them, ride the ship capability like every ship tool). known is false for a
// name this map does not classify — a new tool must be added here
// deliberately (internal/api's ledger test fails on an unclassified one).
func ToolCapability(tool string) (capability string, known bool) {
	switch tool {
	case ToolBrief, ToolItems, ToolClaim, ToolHeartbeat, ToolComplete, ToolRelease, ToolKubeRead:
		return "", true
	case ToolAskPerson:
		return CapAsks, true
	case ToolObsQueries, ToolObsQuery:
		return CapObs, true
	case ToolObsRaw:
		return CapObsRaw, true
	case ToolReleaseEvaluation:
		return CapCaptain, true
	case ToolTaskList, ToolTaskGet:
		return CapTasksRead, true
	case ToolTaskAsk:
		return CapTaskAsks, true
	case ToolFloodBlockPropose:
		return CapFloodBlocks, true
	case ToolGapTask:
		return CapGapTasks, true
	case ToolPRContext:
		return CapPRContext, true
	case ToolMergePR, ToolTagRelease, ToolRolloutStatus, ToolMarkReady, ToolShepherdReady, ToolShepherdMergeAsk, ToolCILogs:
		return CapShip, true
	}
	return "", false
}

// ToolServed is THE rule for whether officer o's instances get tool when
// o's backend advertises caps: ToolOffered (the officer's shape) AND the
// backend serving the tool's capability. internal/api's listing, session
// allow-list and dispatch all ask it. An unclassified tool is never served.
func ToolServed(o Officer, caps Capabilities, tool string) bool {
	need, known := ToolCapability(tool)
	return known && ToolOffered(o, tool) && (need == "" || caps.Has(need))
}

// hqCapabilities is what HQ serves: every capability. HQ has no capabilities
// route yet, and muster has always offered HQ officers every tool their shape
// allows; a route HQ lacks still answers 404 at call time, as before.
var hqCapabilities = NewCapabilities(AllCapabilities()...)

// Capabilities implements Backend: HQ advertises everything (hqCapabilities).
func (c *Client) Capabilities() Capabilities { return hqCapabilities }

// CompleteDroppingCost is b.Complete, sent once more WITHOUT done.Cost when
// the backend refuses the complete that carried it with a 400 — an HQ from
// before item cost refuses the unknown fields, and one with it a cost it will
// not store — so a cost never fails a complete. refused is the refusal of the
// costed complete when the cost-free one then succeeded (the caller says the
// cost was dropped); when that one is refused too, its answer is returned, as
// a complete without a cost always was.
func CompleteDroppingCost(ctx context.Context, b Backend, item ItemID, instance string, done Completion) (body []byte, refused *HQError, err error) {
	body, err = b.Complete(ctx, item, instance, done)
	var hqErr *HQError
	if done.Cost == nil || !errors.As(err, &hqErr) || hqErr.Status != http.StatusBadRequest {
		return body, nil, err
	}
	done.Cost = nil
	body, err = b.Complete(ctx, item, instance, done)
	if err != nil {
		return nil, nil, err
	}
	return body, hqErr, nil
}

// *Client is the HQ implementation of the contract and every optional verb.
var (
	_ Backend            = (*Client)(nil)
	_ TranscriptBackend  = (*Client)(nil)
	_ UsageBackend       = (*Client)(nil)
	_ PoolsBackend       = (*Client)(nil)
	_ AsksBackend        = (*Client)(nil)
	_ TaskAsksBackend    = (*Client)(nil)
	_ FloodBlocksBackend = (*Client)(nil)
	_ GapTasksBackend    = (*Client)(nil)
	_ TasksBackend       = (*Client)(nil)
	_ PRContextBackend   = (*Client)(nil)
	_ ObsBackend         = (*Client)(nil)
	_ ObsRawBackend      = (*Client)(nil)
	_ CaptainBackend     = (*Client)(nil)
	_ ShipBackend        = (*Client)(nil)
)
