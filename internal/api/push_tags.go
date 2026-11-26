package api

// THE PUSH TAG LEDGER (docs/push.md "When a notification is withdrawn").
//
// A notification's tag names what it is about: a later push with the same tag
// replaces it on a device, and a `resolved` push with the same tag withdraws
// it (push.TypeResolved; web/static/sw.js closeByTag, the Android app's
// Withdraw). 🔴 A NOTIFICATION THAT OPENS SOMETHING WHICH CAN STOP WAITING —
// an ask, a draft, a proposal, a request — MUST BE WITHDRAWN WHEN IT DOES:
// otherwise its tap opens "that is no longer waiting". One that only reports
// something (a page published, the morning report) is informational, and
// nothing withdraws it.
//
// pushTagFamilies declares every tag family this package sends, one or the
// other. TestEveryPushTagFamilyIsDeclaredWithItsClearPath reads every tag
// from the source and holds it here: a family sent and not declared fails, a
// family declared and no longer sent fails, a family declared cleared must
// have AT LEAST ONE withdrawal of that family somewhere in the source, and one
// declared informational must have none. The key is the family as the test
// reduces a tag expression: its fixed parts, "*" for each part that varies.
//
// ⚠ WHAT IT DOES NOT CHECK: that EVERY way a family's thing stops waiting
// sends that withdrawal. Cleared is a sentence a person wrote, listing the
// ways; each way is held by that family's own behavioural tests (the fleet's,
// the connectors', pairing's: *_push_retract_pg_test.go), not by this one.

// pushTagFamily is one family's declaration: exactly one of the two is set.
type pushTagFamily struct {
	// Cleared is when its notification is withdrawn, and by what.
	Cleared string
	// Informational is why nothing withdraws it.
	Informational string
}

var pushTagFamilies = map[string]pushTagFamily{
	// --- withdrawn when their thing stops waiting -------------------------------
	"attention-*":            {Cleared: "an Attention item (an ask, a waiting session) leaving the queue: attentionClear, from askSettled and the session-state pass"},
	"plugin-draft-*":         {Cleared: "a reply draft decided or expired: clearDraftNote, and the draft decision's own withdrawal"},
	"plugin-outreach-*":      {Cleared: "an outreach batch decided: outreach_batch.go's withdrawal"},
	"task-*-done":            {Cleared: "a task leaving review: push_task.go's resolved push"},
	"app-*-*":                {Cleared: "an app's knock withdrawn by the app, or its app uninstalled (plugin_notify.go, plugin_server.go)"},
	"officer-cap-hold-*-*":   {Cleared: "an officer's cap hold ending (officerCapHoldNotice)"},
	"officer-restart-held-*": {Cleared: "an officer instance leaving its held error (officerAutoRestartNotice)"},
	fleetPushTag:             {Cleared: "no fleet proposal waiting any more — expired, moved on, approved, rejected, suppressed or fleet proposals turned off: fleetPushSync / fleetPushHide (ordered, re-checked); a proposal leaving while others wait replaces it quietly instead"},
	"connector-*":            {Cleared: "a connector request decided, approved by a shared approval or by a standing grant on a re-ask, revoked, or gone with its connector or its agent: connectorRequestsWithdraw (ordered, re-checked: push_order.go)"},
	"pair-*":                 {Cleared: "a plugin pairing request decided, or expired (withdrawExpiredPairs, from the Attention sweep): pairSettled (ordered, re-checked: push_order.go)"},
	// team_withdraw.go re-sends the withdrawal of OTHER families' open items to
	// a person leaving a team: its tags are theirs, read from the items.
	"w.tag": {Cleared: "a member leaving a team: withdrawTeamItemsNow re-sends the withdrawal of each team item still open"},

	// --- informational: they report, nothing withdraws them --------------------
	"agent-*-outcome":               {Informational: "an agent the owner started finished, failed or went quiet"},
	"dispatch-approved-*":           {Informational: "a dispatch the owner asked for was approved"},
	"host-dispatch-*-outcome":       {Informational: "a host dispatch's outcome"},
	"morning.PushTag":               {Informational: "the morning report: counts, once a day"},
	"muster-security":               {Informational: "a security notice about the owner's account"},
	"muster-test":                   {Informational: "the test notification the owner asked for"},
	"notifyTag()":                   {Informational: "an agent's own notification to its owner (notify_owner)"},
	"officer-budget-*-*":            {Informational: "an officer's daily budget reached: one per UTC day, and the cap holds until midnight"},
	"officer-budget-unreadable-*-*": {Informational: "an officer's spend could not be read: one per UTC day"},
	"page-*-ready":                  {Informational: "an agent published a page"},
	"page-*-response":               {Informational: "someone answered through one of the owner's pages"},
	"page-*-shared":                 {Informational: "an agent made one of the owner's pages public"},
	"page-*-updated":                {Informational: "an agent published a new version of a page"},
	"proposalEventID()":             {Informational: "a new session started in place of one that ended (succession)"},
	"route-*":                       {Informational: "the chief routed a message or a task to a session"},
	"task-*-comment":                {Informational: "a machine commented on a task"},
	"tasks-comments":                {Informational: "several machine comments, coalesced"},
	"tasks-new":                     {Informational: "a machine created tasks"},
	"task-*-provisioned":            {Informational: "a task's agent is up and working"},
	"task-*-gate-released":          {Informational: "the chief released a task's gate"},
	"team-dispatch-*":               {Informational: "a team dispatch was approved"},
}
