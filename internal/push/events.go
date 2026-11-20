package push

// The notification events a user can receive, each with a toggle on /account.
//
// 🔴 EVERY ONE IS ON BY DEFAULT (operator decision): push_preferences stores
// what a user MUTED, so a user who never opened the page gets everything. That
// is also why a per-owner rate cap and muster_push_delivery_total ship with
// it rather than later — "everything on" is the amplification risk.
const (
	EventReview    = "review"    // a task enters ready_for_review
	EventResolved  = "resolved"  // a task leaves review: clears its notification
	EventConnector = "connector" // an agent requests a connector
	EventAgent     = "agent"     // a task's agent is up and working
	EventCreated   = "created"   // a machine (producer) created tasks
	EventComment   = "comment"   // an external (machine) comment on a task
	EventPlugin    = "plugin"    // a plugin reply draft awaits its owner's approval, or the chief routed something to a session
	EventAttention = "attention" // something new in the attention queue: an agent's question, a session waiting
	// EventAgentOutcome: an agent started with a first turn finished, failed or
	// went quiet (internal/agentoutcome) — one push per change of its outcome.
	EventAgentOutcome = "agent_outcome"
	// EventPage: a viewer sent something through a shareable page's link
	// (internal/pages) — at most one push per page a minute.
	EventPage = "page"
	// EventPagePublished: an agent published a new page for its owner.
	EventPagePublished = "page_published"
	// EventPageShared: an agent made one of its owner's pages public without
	// asking (the owner's own opt-in). Its OWN toggle, apart from
	// page_published: muting "a page is ready" must never also hide the one
	// notice that a page went public.
	EventPageShared = "page_shared"
	// EventMorningReport: the morning report — once a day, only on a day
	// something happened (internal/morning) — counts only, tapping opens it.
	EventMorningReport = "morning_report"
	// EventPageUpdated: an agent published a NEW VERSION of one of its
	// owner's pages (page_update). Its OWN toggle, apart from page_published:
	// an agent iterating on a page publishes many versions, and an owner who
	// wants to hear of new pages may not want every revision — muting the
	// revisions must never also hide a new page (the page_shared precedent).
	EventPageUpdated = "page_updated"
	// EventAgentNotify: one of the owner's agents sent them a notification
	// of its own (the notify_owner tool, internal/api/notify_owner.go).
	EventAgentNotify = "agent_notify"
	// EventApp: one of the owner's `app` plugins asked muster to tell them
	// something from the fixed set whose text muster writes (POST
	// /plugin/v1/notify, internal/api/plugin_notify.go): v1's one kind is a
	// knock, someone waiting to join a meeting.
	EventApp = "app"
)

// Event is one toggle.
type Event struct {
	Name  string
	Label string
}

// Events is the closed vocabulary, in the order /account shows it.
var Events = []Event{
	{EventReview, "A task is ready for review"},
	{EventResolved, "Clear a task's notification when it leaves review"},
	{EventConnector, "An agent requests a connector"},
	{EventAgent, "A task's agent is up and working"},
	{EventCreated, "A machine created tasks"},
	{EventComment, "A machine commented on a task"},
	{EventPlugin, "A reply draft awaits your approval, or your chief routed something to a session"},
	{EventAttention, "An agent asks you something, or a session is waiting on you"},
	{EventAgentOutcome, "An agent you started finished, failed or went quiet"},
	{EventPage, "Someone sent a response through one of your pages' links"},
	{EventPagePublished, "An agent published a page for you"},
	{EventPageShared, "An agent shared one of your pages without asking (anyone with its link can open it)"},
	{EventMorningReport, "Your morning report: what happened while you were away"},
	{EventPageUpdated, "An agent published a new version of one of your pages"},
	{EventAgentNotify, "An agent sent you a notification"},
	{EventApp, "An app needs you"},
}

// KnownEvent reports whether name is in Events.
func KnownEvent(name string) bool {
	for _, e := range Events {
		if e.Name == name {
			return true
		}
	}
	return false
}

// TypeResolved is the payload type the service worker reads as "close the
// notification with this tag and show nothing".
const TypeResolved = "resolved"

// applePushHost is Safari's web push service.
const applePushHost = "web.push.apple.com"
