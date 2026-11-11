// Package metrics holds muster's Prometheus instrumentation. Everything here is
// registered on the default registry and exposed at /metrics.
//
// 🔴 THIS IS A SUBSET OF THE PACKAGE IT WAS CARVED FROM, AND THE OMISSIONS ARE
// THE POINT. The project muster was extracted from instrumented both products
// out of one file: permission decisions, Web Push delivery, auto-approve
// windows, the attention queue, suggestion generation and a transcript archive
// — none of which muster owns. Copying them would have shipped counters that
// can only ever read zero, and a metric that is structurally incapable of
// moving is worse than a missing one: it reads on a dashboard as "this
// subsystem is quiet" rather than "this subsystem is not here". Those metrics
// stayed with the routing service. If muster later grows one of those concerns,
// the counter arrives with it.
//
// 🔴 CARDINALITY RULE, APPLIED THROUGHOUT: every label below is drawn from a
// CLOSED Go vocabulary. No identifier that a producer can mint — a session id,
// a task id, an agent name, a free-form tag value, a raw request path — may
// ever become a label. Each one is an unbounded series and a leak of somebody
// else's identifier into a metrics store that is usually less protected than
// the database. Where that rule is easy to break by accident, the declaration
// says so at its own site.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// BuildInfo is a constant gauge carrying the running version as a label.
	BuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "muster_build_info",
		Help: "Build info; value is always 1, the version is a label.",
	}, []string{"version"})

	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_http_requests_total",
		Help: "HTTP requests by matched route pattern, method, and response status code.",
	}, []string{"route", "method", "code"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muster_http_request_duration_seconds",
		Help:    "HTTP request duration by matched route pattern and method.",
		Buckets: []float64{0.005, 0.025, 0.1, 0.5, 1, 2.5, 5, 10},
	}, []string{"route", "method"})

	// Panics counts recovered panics (HTTP handlers + background goroutines) — a
	// nonzero value is always worth alerting on.
	Panics = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_panics_total",
		Help: "Recovered panics by source.",
	}, []string{"source"})

	// PluginOriginRefusals counts requests refused 421 because their Host is
	// under MUSTER_PLUGIN_ORIGINS (api.refusePluginOriginHosts). The refusal is
	// outermost, so no access log line, span or http metric sees it; this is
	// what does. Nonzero means something routes a plugin's host to muster.
	PluginOriginRefusals = promauto.NewCounter(prometheus.CounterOpts{
		Name: "muster_plugin_origin_refusals_total",
		Help: "Requests refused 421 because their Host is under MUSTER_PLUGIN_ORIGINS.",
	})

	// PluginHandoffs counts viewer hand-off codes (docs/plugins.md#viewer-hand-off)
	// by purpose (`view`; `unknown` for a redeem that matched no code) and
	// outcome: minted, rate_limited and refused (POST /plugin-handoffs);
	// redeemed and not_found (POST /plugin/v1/handoffs/redeem).
	PluginHandoffs = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_plugin_handoffs_total",
		Help: "Viewer hand-off codes minted and redeemed, by purpose and outcome.",
	}, []string{"purpose", "outcome"})

	// PushDelivery counts Web Push delivery attempts, one per DEVICE per
	// notification, by result:
	//   ok      — the push service accepted it (2xx)
	//   expired — the push service said the subscription is gone (404/410);
	//             the device was pruned
	//   failed  — a transport error or any other status; the device's
	//             fail_count went up
	//   disallowed — the device's push service is no longer allowed (its host
	//             left MUSTER_PUSH_EXTRA_HOSTS); nothing was sent and the
	//             device was pruned
	//   oversize — a paired phone's payload v2 was refused before sending: it
	//             would not fit in ntfy's 4096-byte message (push.ErrNativeTooLarge)
	// A notification to an owner with no devices counts nothing. The owner is
	// deliberately NOT a label (an identifier — see the cardinality rule); the
	// per-owner line is in the log (`push: delivered … owner=<id>`).
	PushDelivery = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_push_delivery_total",
		Help: "Web Push delivery attempts by result (ok|expired|failed|disallowed|oversize).",
	}, []string{"result"})

	// PushSuppressed counts notifications NOT sent, one per notification (not
	// per device), by reason:
	//   muted     — the owner turned that event off on /account
	//   throttled — the owner's per-owner rate cap was reached
	// A rising `throttled` is the amplification signal: something is producing
	// notifications faster than a person can read them.
	PushSuppressed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_push_suppressed_total",
		Help: "Web Push notifications not sent, by reason (muted|throttled).",
	}, []string{"reason"})

	// MirrorAskTyping counts answers muster typed into a mirrored session's
	// AskUserQuestion picker, by outcome:
	//   typed        — every key landed (the CLI's record says whose answer won)
	//   not-sent     — refused before the host was asked (offline, step-up, …)
	//   no-picker    — the picker was gone (answered or dismissed in the terminal)
	//   touched      — someone was answering in the terminal; nothing typed
	//   unreadable   — the host could not read the question off the screen as
	//                  the one registered (NOT someone answering it): a parse
	//                  or registration mismatch, or a picker taller than the
	//                  pane — worth a look, like unrecognised
	//   busy         — another answer was being typed into that pane
	//   lost         — the picker changed mid-way; stopped, nothing submitted
	//   unrecognised — the host could not read the picker: Claude Code's layout
	//                  has probably changed — ANY of these is worth a look
	//                  (`make test-liveclaude`)
	//   unsupported  — the host cannot type answers
	//   timeout, error — the host's outcome never came back
	MirrorAskTyping = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_mirror_ask_typing_total",
		Help: "Answers typed into a mirrored session's AskUserQuestion picker, by outcome.",
	}, []string{"result"})

	// THE ASK LOG (internal/asklog; docs/asks.md). Every label is a closed
	// vocabulary: kind is one of asklog.Kinds, answered_by one of owner,
	// chief, auto, none, stage one of read, classify, write. 🔴 No text, host,
	// repository, session or rule id is ever a label (the design's privacy
	// rule: the metrics outlive the text and are less protected than the
	// table).
	//
	// AsksTotal counts asks recorded, one per new row (a capture that ran
	// twice counts once).
	AsksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_asks_total",
		Help: "Asks an agent put to its owner, recorded in the ask log, by kind.",
	}, []string{"kind"})
	// AskPlainTextPrefilter counts turn ends whose last assistant message was
	// read for a plain-text question, by whether the pre-filter kept it (a row
	// is written) or dropped it (none is). What the classifier at capture
	// would cost is read off kept before MUSTER_ASK_LOG_CLASSIFY is turned on.
	AskPlainTextPrefilter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_ask_plain_text_prefilter_total",
		Help: "Turn ends read for a plain-text question, by pre-filter result (kept|dropped).",
	}, []string{"result"})
	// AskAnswerSeconds is how long an ask waited, from asked to how it ended:
	// answered_by owner, chief (its answer, or a nudge it queued) or other (a
	// routed message) for an answer, none for an ask that ended unanswered
	// (expired, cancelled, moved on, superseded, gone). A row a restart closed
	// unknown is NOT observed (the close is one UPDATE across rows, and its
	// time says nothing about the ask). auto is the later phases' (an
	// auto-answer); nothing observes it yet.
	AskAnswerSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muster_ask_answer_seconds",
		Help:    "Time from an ask to its end, by kind and who answered (owner|chief|other|auto|none).",
		Buckets: []float64{5, 15, 30, 60, 120, 300, 900, 1800, 3600, 4 * 3600, 24 * 3600},
	}, []string{"kind", "answered_by"})
	// AskCaptureErrors counts captures that failed — the transcript could not
	// be read (read), the classifier failed (classify), or the row could not
	// be written or the capture queue was full (write). 🔴 A failure is
	// counted and logged, never surfaced: the ask itself went to the owner as
	// before. Any sustained rate means the log is missing asks.
	AskCaptureErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_ask_capture_errors_total",
		Help: "Ask log captures that failed, by kind and stage (read|classify|write).",
	}, []string{"kind", "stage"})

	// AgentProvision counts agent provisioning attempts.
	AgentProvision = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_agent_provision_total",
		Help: "Agent provision attempts by result (ok|failed).",
	}, []string{"result"})

	AgentProvisionDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "muster_agent_provision_duration_seconds",
		Help:    "Agent provision wall time, end to end.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300},
	})

	// AgentKickoffResend counts kickoff recoveries after the container that
	// received the message died (out-of-memory kill, eviction, node drain), by
	// outcome:
	//   resent    — a re-send was launched against the replacement gateway
	//   exhausted — the re-send budget was spent and the agent was errored
	//
	// `exhausted` is the alertable one: it means a workspace is dying repeatedly
	// before it can start work — a memory limit too low for a real repository
	// clone is the shape that motivated this counter. A nonzero `resent` with no
	// `exhausted` is the system self-healing as designed.
	AgentKickoffResend = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_agent_kickoff_resend_total",
		Help: "Kickoff re-deliveries after the receiving container died (resent|exhausted).",
	}, []string{"result"})

	// AgentKickoffClaim counts outcomes of the CROSS-PROCESS kickoff claim — the
	// guard that stops two replicas from both kicking off one agent during a
	// rolling deploy:
	//   won                     — this process took the claim and ran the kickoff
	//   lost                    — another process held it; this kickoff was skipped
	//   error                   — the claim query failed; the kickoff was skipped
	//                             (FAIL CLOSED — the reconciler retries)
	//   renew_lost              — a renewal found the claim no longer ours, i.e.
	//                             a peer took it while our turn was still running
	//   renew_error             — a renewal round-trip failed (the claim is still
	//                             ours until the TTL; three in a row loses it)
	//   shutdown_released       — a claim released on the SIGTERM path
	//   shutdown_release_failed — that release errored; the claim waits out the TTL
	//   stale_swept             — a claim from a previous incarnation of this host
	//                             cleared at startup
	//
	// 🔴 THIS COUNTER MAY BE THE ONLY OBSERVABILITY THIS MECHANISM HAS. The log
	// lines beside each Inc() live in the process's stdout, which rolls away with
	// the pod on every deploy unless something is shipping logs. A deployment
	// that scrapes metrics but does not collect logs — a common and entirely
	// reasonable shape — can observe neither a duplicate-kickoff bug nor its fix
	// without this.
	//
	// `renew_lost` is the alertable one: it means two kickoffs for one agent are
	// genuinely running concurrently. `lost` is the mechanism working as designed
	// and should be rare-but-nonzero around deploys; a `lost` of exactly zero
	// forever is weak evidence the claim is never contended, not that it works.
	AgentKickoffClaim = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_agent_kickoff_claim_total",
		Help: "Cross-process kickoff claim outcomes (won|lost|error|renew_lost|renew_error|shutdown_released|shutdown_release_failed|stale_swept).",
	}, []string{"result"})

	// TasksCreated counts tasks created through the HTTP API, by their derived
	// producer source.
	//
	// 🔴 THE `source` LABEL IS BOUNDED BY A CLOSED ALLOWLIST IN THE API LAYER, not
	// by whatever the request carried. Deriving it from an Origin header or a
	// user agent directly would let any caller mint a new series.
	TasksCreated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_tasks_created_total",
		Help: "Tasks created via the HTTP API, by derived source (a closed allowlist).",
	}, []string{"source"})

	// RoutingTags counts routing (reserved-namespace) tags written onto tasks.
	//
	// 🔴 CARDINALITY RULE: the ONLY label is the reserved NAMESPACE, drawn from
	// the closed Go allowlist notes.RoutingNamespaces(). A tag's VALUE and any
	// free-form tag must NEVER reach a Prometheus label: free-form tags are
	// user/producer text and can carry repository or project names, so they would
	// blow up the label space and leak identifiers into metrics.
	RoutingTags = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_task_routing_tags_total",
		Help: "Routing tags written onto tasks, by reserved namespace ONLY — never the tag value.",
	}, []string{"namespace"})

	// TaskSessionLinks counts task-thread links that CHANGED — a session joining
	// a task's thread, or its role being upgraded. A repeat touch that only moves
	// last_seen_at is NOT counted, so this measures thread growth rather than
	// hook traffic.
	//
	// 🔴 CARDINALITY RULE, same discipline as RoutingTags: the ONLY label is
	// `role`, drawn from the closed three-value vocabulary
	// notes.RoleRead|RoleWorked|RoleCreated. The SESSION ID must NEVER become a
	// label — it is unbounded producer-supplied text, one new series per session.
	TaskSessionLinks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_task_session_links_total",
		Help: "Task-thread links created or upgraded, by role ONLY (read|worked|created) — never the session id.",
	}, []string{"role"})

	// TaskSessionLinksSkipped counts task-thread links that were NOT recorded, by
	// a closed two-value reason: `thread_full` (the task already carried
	// notes.MaxTaskSessions created/worked links, so a `read` breadcrumb was
	// dropped) or `error` (a store failure).
	//
	// 🔴 IT EXISTS BECAUSE THE FAILURE IS DELIBERATELY SILENT TO THE CALLER. The
	// cap is advisory — a provenance breadcrumb must never break the operation it
	// annotates — so a skipped link produces a normal 200. Without this counter
	// the only trace would be a log line, and "the thread quietly stopped
	// growing" would be indistinguishable from "nothing touched this task".
	TaskSessionLinksSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_task_session_links_skipped_total",
		Help: "Task-thread links NOT recorded, by reason ONLY (thread_full|error) — never the session id.",
	}, []string{"reason"})

	// FlowJobs counts browser-flow job transitions (internal/flows), by flow and
	// outcome.
	//
	// 🔴 CARDINALITY RULE: `flow` is a name from the COMPILED-IN catalogue
	// (flows.Catalogue — two entries today, and an enqueue naming anything else
	// is refused before it is counted), and `outcome` is the closed vocabulary
	// enqueued|claimed|requeued|done|failed|needs_operator|expired|cancelled,
	// plus an agent request's requested|approved|denied (migration 0014).
	// A job id, host, instance or any parameter must NEVER become a label.
	FlowJobs = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_flow_jobs_total",
		Help: "Browser-flow job transitions, by catalogue flow name and outcome ONLY — never a job id, host or parameter.",
	}, []string{"flow", "outcome"})

	// The memo relay (internal/memorelay): what each agent's check, each listener
	// call and each delivery came to.
	//
	// 🔴 CARDINALITY RULE: `result` is a closed vocabulary compiled into
	// internal/memorelay (its Result* and Outcome* constants and upstreamResult).
	// An agent name, a scope, a memo id or anything a listener sent must NEVER
	// become a label.
	MemoRelayAgentChecks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_memo_relay_agent_checks_total",
		Help: "Memo relay: agents classified per tick, by result ONLY (eligible, or why an agent is sent nothing).",
	}, []string{"result"})
	MemoRelayUpstream = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_memo_relay_upstream_total",
		Help: "Memo relay: calls to the cairn memo listener, by result ONLY — including the ones NOT sent (not_yet, backed_off, scope_backed_off).",
	}, []string{"result"})
	MemoRelayDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_memo_relay_deliveries_total",
		Help: "Memo relay: delivery attempts to agents, by result ONLY — never an agent, scope or memo id.",
	}, []string{"result"})

	// CCUsagePushFailures counts the Claude account usage pusher's failed
	// pushes (MUSTER_CC_USAGE_PUSH_URL; internal/ccusage push.go), by reason:
	//   build     — the readings could not be assembled (the store did not answer)
	//   timeout   — no answer inside the push timeout
	//   transport — any other failure to send (DNS, refused connection, TLS)
	//   http_4xx  — the dashboard answered 4xx (a bad token is 401/403)
	//   http_5xx  — the dashboard answered 5xx
	//   http_other — any other status but 204 (a 200, a refused redirect)
	//   canceled  — muster was shutting down mid-push
	// 🔴 `reason` is that closed vocabulary (ccusage.PushReason*): never a URL,
	// a status code or an account name.
	CCUsagePushFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_cc_usage_push_failures_total",
		Help: "Claude account usage pushes that failed, by reason (build|timeout|transport|http_4xx|http_5xx|http_other|canceled).",
	}, []string{"reason"})
)

// ---- opening a session (docs/telemetry.md) --------------------------------
//
// 🔴 CARDINALITY RULE, AGAIN: `view`, `phase`, `op` and `reason` are each a
// closed vocabulary compiled into this module (the constants beside each
// Observe* below and mirror.Reason). A host, a session, a
// window, an agent name or a trace id must NEVER become a label — the per-open
// log line (`open view=…`) carries those, with the trace id that finds the
// trace.
var (
	// OpenPhase is the server's half of opening a session, by view
	// (mirror_view | mirror_terminal | agent) and phase:
	//   document     — the page's handler, request in to body written
	//   first_batch  — a rendered view's socket, upgrade to its first batch
	//                  written to the browser (resume or backfill)
	//   first_output — a terminal's socket, upgrade to its first bytes written
	OpenPhase = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muster_open_phase_seconds",
		Help:    "Server-side phases of opening a session, by view and phase (closed vocabularies).",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 40},
	}, []string{"view", "phase"})

	// MirrorHubRoundTrip is one request to a mirror host, frame written to
	// answer read, by op (watch: to the first batch; attach: to the first
	// output; paste, scroll, catalog, answer, cancel_ask, opencode_answer,
	// subagent_tail: to the host's answer) and reason (mirror.Reason).
	MirrorHubRoundTrip = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muster_mirror_hub_roundtrip_seconds",
		Help:    "A request to a mirror host, frame written to answer read, by op and reason.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 40},
	}, []string{"op", "reason"})

	// ViewDisconnects counts a session view's socket ENDING, by view and
	// reason (mirror.Reason's vocabulary, plus client_left when the browser
	// went away first). It is the server's half of "the session
	// disconnected"; the page reports its own half through the browser
	// telemetry.
	ViewDisconnects = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_view_disconnects_total",
		Help: "Session view sockets ended, by view and reason (closed vocabularies).",
	}, []string{"view", "reason"})
)

// The officer runtime (internal/officers). Deliberately UNLABELLED: an officer
// id is operator-configured, but a label per officer is still an identifier in
// a series name, and the operator needs "something is wrong", with the log
// line naming which officer. Each is the loop's whole-process total, set after
// every pass.
var (
	// OfficerErrorSlots counts officer instance slots held by an instance in
	// error: the officer runs below target until it recovers or an operator acts.
	OfficerErrorSlots = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "muster_officer_error_slots",
		Help: "Officer instance slots held by an instance in error (not restarted; the officer runs below target).",
	})
	// OfficerHQFaults counts officers whose HQ answers a PERMANENT refusal
	// (401/403/404: a revoked token, HQ without the officer API): a
	// configuration fault, not an outage. Their instances are scaled to zero.
	OfficerHQFaults = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "muster_officer_hq_faults",
		Help: "Officers whose HQ refuses them permanently (401/403/404); their instances are scaled to zero.",
	})
	// OfficerCapRefusalWithheld counts judgement instance numbers withheld
	// from their pool because HQ refuses the wall-time cap's escalation of the
	// item they held (tiers.judgement.max_item_minutes): muster keeps the item
	// held and never starts that number until the escalation lands (fail
	// closed). At the default max_instances 1 one is the whole pool, and every
	// other judgement item waits. Nonzero is worth an alert; the owner is
	// also notified once per episode, and the log line names the officer.
	OfficerCapRefusalWithheld = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "muster_officer_cap_refusal_withheld",
		Help: "Judgement instances withheld from their pool because HQ refuses the wall-time cap's escalation of the item they held.",
	})
	// OfficerJudgementCapReached counts officers whose judgement pool has
	// used its daily session cap (tiers.judgement.max_sessions_per_day) while
	// claimable judgement items wait: muster does not wake the pool for them
	// until the cap resets at 00:00 UTC. The log line (once per officer per
	// UTC day) names the officer, used/cap and the waiting count.
	OfficerJudgementCapReached = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "muster_officer_judgement_cap_reached",
		Help: "Officers whose judgement pool has used its daily session cap while claimable judgement items wait (not woken until 00:00 UTC).",
	})
	// OfficerTranscriptEvents counts officer transcript events by what became
	// of them (internal/officers/transcript): posted (HQ answered 2xx),
	// dropped_buffer (an instance's buffer was full: the oldest went),
	// dropped_retries (HQ kept failing past the retry cap), dropped_refused
	// (HQ answered 400), dropped_superseded (the instance claimed another
	// item before they were sent), dropped_unsequenced (no valid seq could be
	// derived from the transcript position).
	OfficerTranscriptEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_officer_transcript_events_total",
		Help: "Officer transcript events forwarded to HQ, by result (posted or a drop reason).",
	}, []string{"result"})
	// OfficerUsagePush counts the officer usage collector's per-instance
	// attempts (internal/officers/usage), by result: pushed (HQ answered
	// 2xx), ccd_unsupported (the instance's ccd predates GET /v1/usage),
	// ccd_failed (the read failed), hq_not_found (HQ answered 404: an HQ
	// without the route, or an unknown officer), hq_refused (any other 4xx),
	// hq_unavailable (5xx, a timeout, no answer), skipped (a pre-stop flush
	// that could not start within its budget).
	OfficerUsagePush = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_officer_usage_push_total",
		Help: "Officer instance Claude-usage pushes to HQ, by result (pushed or a failure class).",
	}, []string{"result"})
)

// The officer LLM proxy (internal/llmproxy, POST /llm/v1/messages on the
// connect listener).
//
// 🔴 THESE ARE LABELLED BY OFFICER AND INSTANCE, UNLIKE THE OFFICER RUNTIME'S
// GAUGES ABOVE, AND THAT IS A DECISION, NOT A SLIP. Per-officer spend is the
// whole point of the series (they are the usage counters a cost dashboard and
// an alert on a runaway officer read), and both labels are bounded by
// configuration rather than minted by a producer: `officer` is an id declared
// in MUSTER_OFFICERS (an officer with no `llm` block never reaches a label;
// a caller that is not an officer is "none"), and `instance` is the routine
// instance's label, `r<n>` for n in 1..tiers.routine.max_instances (at most
// 8), never the agent name. Nothing a pod sends becomes a label: the provider that served a
// request (an open set) and the model the client asked for are in the log
// line and the audit row only.
var (
	// OfficerLLMRequests counts proxied requests by result:
	//   ok             — the provider answered 2xx and the response was relayed
	//   upstream_auth  — the provider refused muster's key (401/403); also
	//                    counted in OfficerLLMUpstreamAuth
	//   upstream_status — any other non-2xx from the provider (relayed; a
	//                    402 to a caller with a KeyRemedy, the chief, is
	//                    answered with muster's 400 and that remedy instead)
	//   upstream_error — no answer, or the stream broke off
	//   refused_budget — the officer's daily USD budget was spent
	//   refused_item_cap — the instance used max_tokens_per_item on the item
	//                    it holds (never while it holds none)
	//   refused_bad_request — a body that is not a JSON object
	//   refused_store  — the budget could not be read (or this proxy keeps
	//                    none for the caller), so nothing was sent
	// The chief (an unbudgeted caller) is counted with officer and instance
	// both "chief"; it is never refused_budget or refused_item_cap.
	OfficerLLMRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_officer_llm_requests_total",
		Help: "Officer LLM proxy requests by officer, routine instance (r<n>) and result.",
	}, []string{"officer", "instance", "result"})
	// OfficerLLMRefused counts requests refused before any officer could be
	// charged, by reason: path (not POST /llm/v1/messages), not_routine (the
	// caller is not an allowed routine instance of an officer with `llm`),
	// unconfigured (no officer declares `llm`), agents_read (with the chief
	// LLM on, the agents list could not be read to tell whether a caller that
	// is not a routine instance is the chief). officer is "none" when the
	// caller is not an officer instance. For the chief (officer "chief"):
	// not_admin (a chief, but not the admin's), chief_off (its owner turned
	// it off), no_key (MUSTER_AGENT_OPENROUTER_KEY is empty).
	OfficerLLMRefused = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_officer_llm_refused_total",
		Help: "Officer LLM proxy requests refused before reaching the provider, by officer and reason.",
	}, []string{"officer", "reason"})
	// OfficerLLMTokens counts tokens the provider reported, by direction: in
	// (uncached input), cache_read, cache_write, out.
	OfficerLLMTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_officer_llm_tokens_total",
		Help: "Tokens the provider reported for officer LLM proxy requests, by officer, routine instance (r<n>) and direction.",
	}, []string{"officer", "instance", "dir"})
	// OfficerLLMCost is the USD booked against the officer's budget: the
	// provider's own usage.cost, or the proxy's pessimistic estimate when the
	// response carried none (OfficerLLMCostEstimated counts those).
	OfficerLLMCost = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_officer_llm_cost_usd_total",
		Help: "USD booked against officers' LLM budgets, by officer and routine instance (r<n>).",
	}, []string{"officer", "instance"})
	// OfficerLLMCostEstimated counts requests booked from an estimate
	// because the provider reported no cost (a broken-off stream, a client
	// that went away). A rising count means the budget is metered on guesses.
	OfficerLLMCostEstimated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_officer_llm_cost_estimated_total",
		Help: "Officer LLM proxy requests booked from an estimate (the provider reported no cost), by officer.",
	}, []string{"officer"})
	// OfficerLLMUpstreamAuth counts the provider refusing muster's key, by
	// status (401 or 403). 🔴 Claude Code retries a 401 about ten times with
	// backoff, so on an instance a bad key looks like a model stall; this,
	// OfficerLLMUpstreamAuthFailing and the log line are how it is told apart.
	OfficerLLMUpstreamAuth = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_officer_llm_upstream_auth_failures_total",
		Help: "Provider refusals of muster's LLM key (401/403), by officer and status.",
	}, []string{"officer", "status"})
	// OfficerLLMUpstreamAuthFailing is 1 while the provider's last answer for
	// the officer was a 401/403, and 0 once one succeeds again.
	OfficerLLMUpstreamAuthFailing = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "muster_officer_llm_upstream_auth_failing",
		Help: "1 while the provider refuses the officer's LLM key (its last answer was 401/403), else 0.",
	}, []string{"officer"})
	// OfficerLLMSpentToday and OfficerLLMBudget are one officer POOL's USD
	// spend this UTC day (as last read or booked) and its daily budget: the
	// routine and the judgement pool each have their own (pool label).
	OfficerLLMSpentToday = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "muster_officer_llm_spent_today_usd",
		Help: "USD an officer pool's LLM proxy requests have booked this UTC day, as last read or booked.",
	}, []string{"officer", "pool"})
	OfficerLLMBudget = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "muster_officer_llm_budget_usd",
		Help: "An officer pool's daily LLM budget in USD (MUSTER_OFFICERS llm / tiers.judgement.llm daily_budget_usd).",
	}, []string{"officer", "pool"})
	// OfficerLLMBookFailures counts usage that could not be written to the
	// database. The spend still counts in memory until the next read, but a
	// restart would forget it: alert on any.
	OfficerLLMBookFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_officer_llm_book_failures_total",
		Help: "Officer LLM usage rows that could not be written to the database, by officer.",
	}, []string{"officer"})
)

// ObserveHTTP records one HTTP request's matched route pattern, method, status,
// and duration.
//
// 🔴 `route` MUST BE THE ROUTE PATTERN, NEVER THE REQUEST PATH. The pattern
// ("GET /api/tasks/{id}") is drawn from the mux's own finite route table, so the
// label space is bounded by the size of that table. The raw path carries ids and
// sometimes tokens and is unbounded. Callers pass "unmatched" on the 404 path
// for exactly this reason.
func ObserveHTTP(route, method string, code int, d time.Duration) {
	httpRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	httpDuration.WithLabelValues(route, method).Observe(d.Seconds())
}
