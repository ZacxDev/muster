package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/searchrank"
)

// ---------------------------------------------------------------------------
// THE CHIEF'S OPENROUTER SESSIONS ON THE LLM PROXY: THE SPEND DOOR
// (docs/chief.md "OpenRouter sessions, budget").
//
// A side chat of the admin's chief may run on any OpenRouter model. Its
// runtime in the chief's pod calls POST /llm/v1/messages with its chief's
// muster token (X-Muster-Token) and X-Muster-Chief-Session: <the session's
// key>; serveLLMMessages hands every request carrying that header here, and
// here alone. The request is proxied pinned to THAT session's stored model,
// on the chief sessions' own key, under the daily and per-session budget,
// and its provider-reported cost is booked to chief_session_llm_usage.
//
// Admitted exactly when: EnableChiefSessionsLLM was called (else 400); the
// calling agent is its owner's chief (agentIsChief); that owner is the admin
// (ChiefLLMOwner, the same predicate as the chief's own proxy route: the key
// is the admin's); the chief is on (chiefActing); and the header names a chat
// session that is this chief's — owned by the chief's owner and of this
// chief agent — is not its primary chat, and runs on OpenRouter
// (agents.SessionOnOpenRouter). Every other case is refused, nothing sent.
//
// 🔴 EVERY REFUSAL THIS DOOR WRITES ITSELF IS A 400 — its checks above and
// the budget's (chiefSessionLedger, through the proxy). opencode, the session
// runtime, was measured to retry a 503 forever and to fail a 400 at once; a
// 400 ends the turn with muster's message, and a transient fault (a read that
// failed) says "retry the turn shortly" in the text instead. NOT covered: the
// connector door in front (requireConnectorAgent) answers 401/503 before this
// door runs (no token, no store, an owner role it cannot read), and the
// provider's own error statuses are relayed (llmproxy).
//
// 🔴 THE KEY IS MUSTER_CHIEF_SESSIONS_OPENROUTER_KEY AND NOTHING ELSE. It is
// read by the server only, attached by the proxy, never put in a pod or a
// response. MUSTER_AGENT_OPENROUTER_KEY never stands in for it (its credit
// limit is production's), so a request with the header is NEVER handed to the
// chief's own route (serveChiefLLM), whatever else is on.
//
// ⚠ THE HEADER CAN BE FORGED FROM INSIDE THE POD. Anything in the chief's pod
// holds the chief's token and can send any session key. What that buys, and
// only that: spend booked to ANOTHER OpenRouter side chat of the SAME chief,
// pinned to that session's model. Another owner's session, the primary chat
// and a Claude session are refused above. The daily cap is per owner and is
// enforced by reservation (the budget section below), so a forged key admits
// nothing the owner's day has no room for — with that section's one limit: a
// request billed above its own estimate.
// ---------------------------------------------------------------------------

// ChiefSessionHeader is the header naming the chat session a chief's
// OpenRouter session runtime is calling for.
const ChiefSessionHeader = "X-Muster-Chief-Session"

// chiefSessionsLabel is the chief sessions' officer label in the proxy's
// metrics, log line and audit detail (one label: a session id is never a
// metric label).
const chiefSessionsLabel = "chief-sessions"

// chiefSessionMaxKeyLen bounds the header value read.
const chiefSessionMaxKeyLen = 200

// Default and environment names, named in refusals and docs.
const (
	// DefaultChiefSessionsDailyUSD is MUSTER_CHIEF_SESSIONS_DAILY_USD's
	// default: the operator's decision.
	DefaultChiefSessionsDailyUSD = 20.0
	envChiefSessionsKeyName      = "MUSTER_CHIEF_SESSIONS_OPENROUTER_KEY"
	envChiefSessionsDailyName    = "MUSTER_CHIEF_SESSIONS_DAILY_USD"
	envChiefSessionsSessionName  = "MUSTER_CHIEF_SESSIONS_SESSION_USD"
)

// chiefSessionsKeyRemedy is what a session is told when the provider refuses
// the key, or answers 402 (its credit limit is spent).
const chiefSessionsKeyRemedy = "it is the server's " + envChiefSessionsKeyName + ": check it, and its credit limit, then restart muster"

// ChiefSessionsOffRefusal is the 400 a request with the header gets when the
// server does not serve chief sessions on OpenRouter.
const ChiefSessionsOffRefusal = "chief sessions on OpenRouter are off on this server (MUSTER_CHIEF_SESSIONS_OPENROUTER); " +
	"nothing was sent"

// ChiefSessionsLLM is EnableChiefSessionsLLM's input.
type ChiefSessionsLLM struct {
	// Key is MUSTER_CHIEF_SESSIONS_OPENROUTER_KEY. Required.
	Key string
	// DailyUSD is the cap on an owner's chief-session spend per UTC day
	// (MUSTER_CHIEF_SESSIONS_DAILY_USD). Must be > 0.
	DailyUSD float64
	// SessionUSD is the optional cap on one session's lifetime spend
	// (MUSTER_CHIEF_SESSIONS_SESSION_USD); 0 is none.
	SessionUSD float64
	// Store is the spend book (chief_session_llm_usage). Required: a budget
	// that a restart resets is not a budget.
	Store chiefusage.SessionLLMStore
	// Upstream replaces the provider's API base, HTTP its transport, Now the
	// clock: tests.
	Upstream string
	HTTP     *http.Client
	Now      func() time.Time
}

// chiefSessionsRuntime is the door's proxy and settings; nil on the Server
// when EnableChiefSessionsLLM was never called.
type chiefSessionsRuntime struct {
	px         *llmproxy.Proxy
	key        string
	dailyUSD   float64
	sessionUSD float64
	store      chiefusage.SessionLLMStore
	now        func() time.Time
}

// EnableChiefSessionsLLM opens the spend door. Call it after UseExtensions.
// It refuses, for the caller to refuse to boot, without a key, a positive
// daily cap, a spend book, or the connector and agents stores (the proxy's
// door, requireConnectorAgent, answers 503 to every request without them).
func (s *Server) EnableChiefSessionsLLM(c ChiefSessionsLLM) error {
	if s.ext.Connectors == nil || s.ext.Agents == nil {
		return errors.New("chief sessions: this server has no connector store or agents store: the LLM proxy " +
			"authenticates the chief through the connector door and audits into its store")
	}
	key := strings.TrimSpace(c.Key)
	switch {
	case key == "":
		return fmt.Errorf("chief sessions: %s is empty", envChiefSessionsKeyName)
	case !(c.DailyUSD > 0):
		return fmt.Errorf("chief sessions: %s must be above 0", envChiefSessionsDailyName)
	case !(c.SessionUSD >= 0):
		return fmt.Errorf("chief sessions: %s must be 0 (none) or above", envChiefSessionsSessionName)
	case c.Store == nil:
		return errors.New("chief sessions: there is no usage store to keep the budget in")
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	px, err := llmproxy.New(llmproxy.Config{Upstream: c.Upstream, HTTP: c.HTTP, Audit: s.llmAudit(c.Upstream), Logger: s.logger, Now: now})
	if err != nil {
		return err
	}
	s.chiefSessions = &chiefSessionsRuntime{px: px, key: key, dailyUSD: c.DailyUSD, sessionUSD: c.SessionUSD, store: c.Store, now: now}
	per := "no per-session cap"
	if c.SessionUSD > 0 {
		per = fmt.Sprintf("$%.2f a session", c.SessionUSD)
	}
	s.logger.Printf("chief sessions: the admin's chief's OpenRouter side chats may reach their models through the LLM proxy "+
		"on %s, $%.2f a UTC day, %s", envChiefSessionsKeyName, c.DailyUSD, per)
	return nil
}

// chiefSessionRequest reports whether r is a chief-session request: it
// CARRIES the header, whatever its value (an empty one is refused, never read
// as "no header").
func chiefSessionRequest(r *http.Request) bool {
	_, ok := r.Header[http.CanonicalHeaderKey(ChiefSessionHeader)]
	return ok
}

// refuseChiefSession writes the door's refusal: always a 400 (the header).
func (s *Server) refuseChiefSession(w http.ResponseWriter, a agents.Agent, reason, msg string) {
	metrics.OfficerLLMRefused.WithLabelValues(chiefSessionsLabel, reason).Inc()
	s.logger.Printf("llm proxy: refused a chief-session request of agent %d (%q): %s (nothing sent)", a.ID, a.Name, reason)
	llmproxy.Refuse(w, http.StatusBadRequest, "invalid_request_error", msg)
}

// serveChiefSessionLLM is POST /llm/v1/messages for a request carrying
// ChiefSessionHeader (serveLLMMessages), checked in the header's order.
func (s *Server) serveChiefSessionLLM(w http.ResponseWriter, r *http.Request, a agents.Agent) {
	ctx := r.Context()
	if s.chiefSessions == nil {
		s.refuseChiefSession(w, a, "off", ChiefSessionsOffRefusal)
		return
	}
	key := strings.TrimSpace(r.Header.Get(ChiefSessionHeader))
	if key == "" || len(key) > chiefSessionMaxKeyLen {
		s.refuseChiefSession(w, a, "bad_session", "the "+ChiefSessionHeader+" header names no session; nothing was sent")
		return
	}
	_, chief, err := s.agentIsChief(ctx, a.ID)
	if err != nil {
		s.refuseChiefSession(w, a, "agents_read", "muster could not read its agents list to tell whether this agent is the "+
			"chief; nothing was sent. Retry the turn shortly.")
		return
	}
	if !chief {
		s.refuseChiefSession(w, a, "not_chief", "only a chief's sessions are served on "+ChiefSessionHeader+"; nothing was sent")
		return
	}
	owner := agentOwnerID(a)
	if !ChiefLLMOwner(owner) {
		s.refuseChiefSession(w, a, "not_admin", "only the admin's chief's sessions are served on the chief sessions' "+
			"OpenRouter key; this chief belongs to another user. Nothing was sent")
		return
	}
	if !s.chiefActing(ctx, a) {
		s.refuseChiefSession(w, a, "chief_off", "your owner turned the chief off; nothing was sent")
		return
	}
	sess, why, msg := s.chiefSessionFor(ctx, a, key)
	if why != "" {
		s.refuseChiefSession(w, a, why, msg)
		return
	}
	cs := s.chiefSessions
	cs.px.ServeMessages(w, r, llmproxy.Caller{Officer: chiefSessionsLabel, InstanceLabel: "session",
		AgentID: a.ID, AgentName: a.Name, Model: chiefSessionUpstreamModel(sess.Model), Key: cs.key,
		DailyBudgetUSD: cs.dailyUSD, Title: "muster chief session", KeyRemedy: chiefSessionsKeyRemedy,
		Ledger: chiefSessionLedger{s: s, session: sess}})
}

// chiefSessionFor reads the session the header names and checks it is one
// this door may spend on for chief a: owned by a's owner, of a itself, not
// a's primary chat, on OpenRouter. why is "" when it is; else the refusal's
// metric reason and message.
func (s *Server) chiefSessionFor(ctx context.Context, a agents.Agent, key string) (chiefusage.SessionRef, string, string) {
	sess, ok, err := s.chiefSessions.store.SessionRef(ctx, key)
	switch {
	case err != nil:
		s.logger.Printf("llm proxy: chief session %q of agent %d: read: %v", key, a.ID, err)
		return sess, "session_read", "the session could not be read just now; nothing was sent. Retry the turn shortly."
	case !ok:
		return sess, "unknown_session", "the session " + ChiefSessionHeader + " names is not one of this chief's; nothing was sent"
	case sess.OwnerID != agentOwnerID(a) || sess.AgentID != a.ID:
		// 🔴 THE OWNER CHECK. Row-level security already hides another
		// owner's session from this read; this is the check that does not
		// depend on it, and the one that refuses a session of another agent
		// of the same owner.
		return sess, "not_this_chief", "the session " + ChiefSessionHeader + " names is not one of this chief's; nothing was sent"
	case sess.IsDefault:
		return sess, "primary", "the chief's primary chat does not run on the chief sessions' OpenRouter key; nothing was sent"
	case !agents.SessionOnOpenRouter(sess.Model):
		return sess, "not_openrouter", "this session does not run on OpenRouter (it is a Claude session); nothing was sent"
	}
	return sess, "", ""
}

// chiefSessionUpstreamModel is the model id the provider is asked for, from a
// session's stored "openrouter/<catalogue id>": the catalogue id. An id
// OpenRouter itself owns ("openrouter/auto") is stored as itself and has no
// "/" once the prefix is off, so it is sent whole.
func chiefSessionUpstreamModel(stored string) string {
	id := strings.TrimPrefix(stored, agents.OpenRouterModelPrefix)
	if !strings.Contains(id, "/") {
		return stored
	}
	return id
}

// ---------------------------------------------------------------------------
// THE BUDGET, UNDER CONCURRENCY: RESERVE, THEN SEND
//
// Every request reserves a PESSIMISTIC ESTIMATE of its cost against both caps
// before it is sent (chiefusage Reserve: the check and the reservation are one
// transaction under a per-owner advisory lock, so they hold across muster
// processes). The estimate counts EVERY BODY BYTE as an input token and the
// request's max_tokens as output, each at the model's catalogue list price ×
// chiefSessionPriceMargin, never below the proxy's own pessimistic rates
// (llmproxy.EstimateUSDPer*), and at fallback rates far above any list price
// when the catalogue has no price for the model. When the caps leave less room
// than that, max_tokens is CUT to what the room pays for; when the room cannot
// pay for chiefSessionMinMaxTokens of output, the request is refused (400,
// nothing sent). The reservation is replaced by the booked cost (the
// provider's) when the response ends, dropped when the request is not booked,
// and stops counting at chiefSessionReservationTTL if its process died.
//
// 🔴 WHAT HOLDS, EXACTLY: at every admission, booked + reserved ≤ each cap.
// What is BOOKED stays within a cap as long as no request costs more than its
// own estimate. The provider's billed cost can exceed it (billed prices have
// run above list prices, and the margin is finite); such an overrun is booked
// as it is, the cap can be passed by that much, and the next request is then
// refused.
// ---------------------------------------------------------------------------

// Budget estimate settings (see the header above).
const (
	// chiefSessionPriceMargin multiplies the catalogue's list price.
	chiefSessionPriceMargin = 1.25
	// chiefSessionFallbackUSDPer*Token price a model the catalogue gives no
	// usable price for: far above any current list price, so it over-reserves.
	chiefSessionFallbackUSDPerInputToken  = 20e-6
	chiefSessionFallbackUSDPerOutputToken = 100e-6
	// chiefSessionMinMaxTokens is the least output a request is cut to; with
	// less room than that pays for, it is refused.
	chiefSessionMinMaxTokens = 256
	// chiefSessionMinRequestUSD is the least a request is reserved at.
	chiefSessionMinRequestUSD = 0.0001
	// chiefSessionDefaultMaxTokens is assumed (and sent) for a request that
	// names no max_tokens.
	chiefSessionDefaultMaxTokens = 32000
	// chiefSessionReservationTTL is how long a reservation counts if its
	// request is never settled or released (a process that died).
	chiefSessionReservationTTL = 30 * time.Minute
)

// chiefSessionRates are a session model's pessimistic USD per input and
// output token: the catalogue's list price × the margin, floored at the
// proxy's estimate rates; the fallback rates when the catalogue cannot be
// read or gives no price (missing, unparsable or negative, as OpenRouter
// writes a router's variable price).
func (s *Server) chiefSessionRates(ctx context.Context, stored string) (in, out float64) {
	in, out = chiefSessionFallbackUSDPerInputToken, chiefSessionFallbackUSDPerOutputToken
	if s.ext.ModelCatalog == nil {
		return in, out
	}
	cat, err := s.ext.ModelCatalog.Models(ctx)
	if err != nil {
		return in, out
	}
	id := chiefSessionUpstreamModel(stored)
	for _, m := range cat {
		if m.ID != id {
			continue
		}
		pi, err1 := strconv.ParseFloat(m.Pricing.Prompt, 64)
		po, err2 := strconv.ParseFloat(m.Pricing.Completion, 64)
		if err1 != nil || err2 != nil || pi < 0 || po < 0 || math.IsNaN(pi+po) || math.IsInf(pi+po, 0) {
			return in, out
		}
		return max(pi*chiefSessionPriceMargin, llmproxy.EstimateUSDPerInputToken),
			max(po*chiefSessionPriceMargin, llmproxy.EstimateUSDPerOutputToken)
	}
	return in, out
}

// chiefSessionBudget is one request's view of the caps and the rates.
type chiefSessionBudget struct {
	dailyUSD, sessionUSD float64 // sessionUSD 0: no per-session cap
	in, out              float64 // USD per token
	day                  time.Time
}

// decide is THE budget decision, for a request of bodyBytes asking for asked
// output tokens (0: none named) against sp: the USD to reserve and the
// max_tokens to send, or a refusal.
func (b chiefSessionBudget) decide(sp chiefusage.SessionSpend, bodyBytes int, asked int64) (float64, int64, string) {
	room, sessionBinds := b.dailyUSD-sp.DaySpent-sp.DayReserved, false
	if b.sessionUSD > 0 {
		if r := b.sessionUSD - sp.SessionSpent - sp.SessionReserved; r < room {
			room, sessionBinds = r, true
		}
	}
	inCost := float64(bodyBytes) * b.in
	minimal := max(inCost+chiefSessionMinMaxTokens*b.out, chiefSessionMinRequestUSD)
	if room < minimal {
		return 0, 0, b.refusal(sp, sessionBinds)
	}
	want := asked
	if want <= 0 {
		want = chiefSessionDefaultMaxTokens
	}
	if full := inCost + float64(want)*b.out; full <= room {
		return max(full, chiefSessionMinRequestUSD), want, ""
	}
	cut := int64(math.Floor((room - inCost) / b.out))
	return max(inCost+float64(cut)*b.out, chiefSessionMinRequestUSD), cut, ""
}

// refusal is the sentence for a request the caps have no room for.
func (b chiefSessionBudget) refusal(sp chiefusage.SessionSpend, sessionBinds bool) string {
	if sessionBinds {
		return fmt.Sprintf("this session has spent $%.4f of its $%.2f cap%s (%s); nothing was sent. Continue in a new session",
			sp.SessionSpent, b.sessionUSD, inFlight(sp.SessionReserved), envChiefSessionsSessionName)
	}
	return fmt.Sprintf("your chief's OpenRouter sessions have spent $%.4f of today's $%.2f cap%s (%s, UTC day %s; it resets at "+
		"00:00 UTC); nothing was sent", sp.DaySpent, b.dailyUSD, inFlight(sp.DayReserved), envChiefSessionsDailyName,
		b.day.Format("2006-01-02"))
}

// inFlight names reserved spend in a refusal ("" for none).
func inFlight(usd float64) string {
	if usd <= 0 {
		return ""
	}
	return fmt.Sprintf(", and $%.4f more is held by requests in flight", usd)
}

// chiefSessionLedger is a chief session's budget and spend book on the proxy
// (llmproxy.Ledger): a reservation per request (the header above), settled
// into chief_session_llm_usage.
type chiefSessionLedger struct {
	s       *Server
	session chiefusage.SessionRef
}

func (l chiefSessionLedger) Admit(ctx context.Context, req llmproxy.LedgerRequest) (llmproxy.LedgerAdmission, error) {
	cs := l.s.chiefSessions
	in, out := l.s.chiefSessionRates(ctx, l.session.Model)
	b := chiefSessionBudget{dailyUSD: cs.dailyUSD, sessionUSD: cs.sessionUSD, in: in, out: out, day: req.Day}
	var maxTok int64
	var refusal string
	id, _, err := cs.store.Reserve(ctx, l.session.ID, req.Day, chiefSessionReservationTTL, func(sp chiefusage.SessionSpend) float64 {
		var amount float64
		amount, maxTok, refusal = b.decide(sp, req.BodyBytes, req.MaxTokens)
		return amount
	})
	if err != nil {
		return llmproxy.LedgerAdmission{}, fmt.Errorf("chief sessions: reserve: %w", err)
	}
	if refusal != "" {
		return llmproxy.LedgerAdmission{Refusal: refusal}, nil
	}
	return llmproxy.LedgerAdmission{MaxTokens: maxTok, Ticket: id}, nil
}

func (l chiefSessionLedger) Book(ctx context.Context, ticket any, u llmproxy.Usage) error {
	id, _ := ticket.(int64)
	return l.s.chiefSessions.store.Settle(ctx, id, chiefusage.SessionLLMUsage{SessionID: l.session.ID,
		Model: l.session.Model, Day: u.Day, Input: u.Input, Output: u.Output, CacheRead: u.CacheRead,
		CacheWrite: u.CacheWrite, CostUSD: u.CostUSD, Estimated: u.Estimated})
}

func (l chiefSessionLedger) Release(ctx context.Context, ticket any) {
	id, _ := ticket.(int64)
	if id <= 0 {
		return
	}
	if err := l.s.chiefSessions.store.Release(ctx, id); err != nil {
		l.s.logger.Printf("chief sessions: release reservation %d (it lapses at its expiry instead): %v", id, err)
	}
}

// ChiefSessionSpend is a chief session's budget as CheckChiefSessionSpend
// reads it.
type ChiefSessionSpend struct {
	// Day is the UTC day it was read for; the daily cap resets at its end.
	Day time.Time
	// DaySpentUSD is the owner's booked chief-session spend on Day and
	// DayReservedUSD what requests in flight hold, against DailyCapUSD.
	DaySpentUSD, DayReservedUSD, DailyCapUSD float64
	// SessionSpentUSD is the session's lifetime booked spend and
	// SessionReservedUSD what its requests in flight hold, against
	// SessionCapUSD (0: no per-session cap).
	SessionSpentUSD, SessionReservedUSD, SessionCapUSD float64
	// Refusal is "" when the session may spend, else the sentence the owner
	// is shown (the spent and cap amounts and when it resets).
	Refusal string
}

// ErrChiefSessionsOff is CheckChiefSessionSpend's answer when the server does
// not serve chief sessions on OpenRouter.
var ErrChiefSessionsOff = errors.New("chief sessions on OpenRouter are off on this server")

// CheckChiefSessionSpend is THE PRE-TURN CHECK for a chief session on
// OpenRouter (sessionID, chat_sessions.id): whether the caps leave room for a
// minimal request — chiefSessionMinMaxTokens of output at the proxy's floor
// output rate — counting booked spend and requests in flight. ctx must act as
// the session's owner (row-level security). It reserves nothing: each request
// of the turn is admitted by the proxy door's own reservation
// (chiefSessionLedger.Admit), the same decision (chiefSessionBudget.decide).
func (s *Server) CheckChiefSessionSpend(ctx context.Context, sessionID int64) (ChiefSessionSpend, error) {
	cs := s.chiefSessions
	if cs == nil {
		return ChiefSessionSpend{}, ErrChiefSessionsOff
	}
	day := llmproxy.Day(cs.now())
	sp, err := cs.store.Spend(ctx, sessionID, day)
	if err != nil {
		return ChiefSessionSpend{}, fmt.Errorf("chief sessions: read the spend: %w", err)
	}
	b := chiefSessionBudget{dailyUSD: cs.dailyUSD, sessionUSD: cs.sessionUSD, in: llmproxy.EstimateUSDPerInputToken,
		out: llmproxy.EstimateUSDPerOutputToken, day: day}
	_, _, refusal := b.decide(sp, 0, chiefSessionMinMaxTokens)
	return ChiefSessionSpend{Day: day, DaySpentUSD: sp.DaySpent, DayReservedUSD: sp.DayReserved, DailyCapUSD: cs.dailyUSD,
		SessionSpentUSD: sp.SessionSpent, SessionReservedUSD: sp.SessionReserved, SessionCapUSD: cs.sessionUSD,
		Refusal: refusal}, nil
}

// ---------------------------------------------------------------------------
// ANY OPENROUTER MODEL, CHECKED AGAINST THE CATALOGUE (the operator's
// decision): the chief sessions' model check and the picker's search.
//
// The catalogue is the one cached copy every model check reads
// (OpenRouterCatalog, refreshed hourly). 🔴 IT FAILS CLOSED: when it cannot be
// read, a new session's model is refused with that reason — unless the cached
// copy is younger than chiefSessionCatalogMaxAge, which is then used. There is
// no pass-on-shape fallback here (the agent model check has one): a session's
// model is spent on with real money, and a typo'd id would only fail later.
// ---------------------------------------------------------------------------

// chiefSessionCatalogMaxAge is how old a cached catalogue may be and still
// decide a chief session's model when a refresh fails.
const chiefSessionCatalogMaxAge = 24 * time.Hour

// chiefSessionModelsMax bounds the picker search's answer.
const chiefSessionModelsMax = 10

// ChiefSessionCatalogUnreadable is the refusal when no usable catalogue copy
// exists.
const ChiefSessionCatalogUnreadable = "the OpenRouter model list could not be read (and muster has no copy younger than " +
	"24 hours), so a new session's model cannot be checked; try again shortly"

// chiefSessionCatalog is the catalogue copy the chief sessions' model check
// may use: a fresh one, or one younger than chiefSessionCatalogMaxAge when the
// refresh failed. An error otherwise.
func (s *Server) chiefSessionCatalog(ctx context.Context) ([]CatalogModel, error) {
	if s.ext.ModelCatalog == nil {
		return nil, errCatalogUnavailable
	}
	if aged, ok := s.ext.ModelCatalog.(AgedModelCatalog); ok {
		cat, at, err := aged.ModelsAt(ctx)
		if len(cat) == 0 {
			return nil, errors.Join(errCatalogUnavailable, err)
		}
		if err != nil {
			if at.IsZero() || time.Since(at) >= chiefSessionCatalogMaxAge {
				return nil, fmt.Errorf("%w: the cached copy is from %s: %w", errCatalogUnavailable, at.UTC().Format(time.RFC3339), err)
			}
			s.logger.Printf("chief sessions: the model list could not be refreshed (%v); using the copy from %s", err,
				at.UTC().Format(time.RFC3339))
		}
		return cat, nil
	}
	cat, err := s.ext.ModelCatalog.Models(ctx)
	if err == nil && len(cat) == 0 {
		err = errCatalogUnavailable
	}
	return cat, err
}

// ChiefSessionModel is ResolveChiefSessionModel's answer.
type ChiefSessionModel struct {
	// Input is the value as given (trimmed); Model is what the session is
	// stored with, "openrouter/<catalogue id>", "" when refused.
	Input, Model string
	// Resolved: Model is not Input as given (a bare id prefixed, or a case or
	// name variant resolved to its id).
	Resolved bool
	// Refusal is "" when it passed; Suggestions the closest models' stored
	// forms, Ambiguous when the refusal is that several matched.
	Refusal     string
	Suggestions []string
	Ambiguous   bool
}

// ResolveChiefSessionModel is THE model check for a chief session on
// OpenRouter: raw is accepted only as an exact catalogue id (bare, or as
// "openrouter/<id>"), or a case or name variant of exactly one model
// (matchCatalog, the agent model check's rule). Anything else is refused:
// several matches with them as suggestions, none with the closest ids.
//
// 🔴 NO FUZZY RESOLUTION HERE: a stored model is spent on, so a typed value
// that only resembles a model is refused, never guessed. Fuzzy ranking is the
// picker's search alone (handleChiefSessionModels, rankCatalog). The
// catalogue rule is chiefSessionCatalog's.
func (s *Server) ResolveChiefSessionModel(ctx context.Context, raw string) ChiefSessionModel {
	in := strings.TrimSpace(raw)
	out := ChiefSessionModel{Input: in}
	if in == "" {
		out.Refusal = "name an OpenRouter model"
		return out
	}
	cat, err := s.chiefSessionCatalog(ctx)
	if err != nil {
		s.logger.Printf("chief sessions: model %q: %v", in, err)
		out.Refusal = ChiefSessionCatalogUnreadable
		return out
	}
	return resolveChiefSessionModel(in, cat)
}

// resolveChiefSessionModel is ResolveChiefSessionModel's pure body over a
// read catalogue.
func resolveChiefSessionModel(in string, cat []CatalogModel) ChiefSessionModel {
	out := ChiefSessionModel{Input: in}
	r := matchCatalog(in, cat)
	if r.Refusal != "" {
		out.Refusal, out.Suggestions, out.Ambiguous = r.Refusal, r.Suggestions, r.Ambiguous
		return out
	}
	out.Model = r.Model
	if !agents.SessionOnOpenRouter(out.Model) {
		out.Model = agents.OpenRouterModelPrefix + out.Model
	}
	out.Resolved = out.Model != in
	return out
}

// catalogSlug is a catalogue id's stored form (matchCatalog's rule): an id
// OpenRouter owns ("openrouter/auto") is its own.
func catalogSlug(id string) string {
	if strings.HasPrefix(id, agents.OpenRouterModelPrefix) {
		return id
	}
	return agents.OpenRouterModelPrefix + id
}

// rankCatalog is the models matching q at searchrank tier maxTier or better,
// best tier first, ties in the catalogue's (id) order: the picker search's
// ranking (handleChiefSessionModels), never a model check.
func rankCatalog(q string, cat []CatalogModel, maxTier int) []CatalogModel {
	type hit struct {
		m    CatalogModel
		tier int
	}
	var hits []hit
	for _, m := range cat {
		best := -1
		for _, label := range []string{m.ID, m.Name} {
			if label == "" {
				continue
			}
			if t := searchrank.Tier(label, q); t >= 0 && (best < 0 || t < best) {
				best = t
			}
		}
		if best >= 0 && best <= maxTier {
			hits = append(hits, hit{m, best})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].tier < hits[j].tier })
	out := make([]CatalogModel, len(hits))
	for i, h := range hits {
		out[i] = h.m
	}
	return out
}

// ChiefSessionModelsRoute is the chief sessions' model search.
const ChiefSessionModelsRoute = "GET /api/chief/openrouter-models"

// chiefSessionModelHit is one model in the search's answer.
type chiefSessionModelHit struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	ContextLength int64          `json:"context_length"`
	Pricing       CatalogPricing `json:"pricing"`
}

// handleChiefSessionModels serves GET /api/chief/openrouter-models?q=…: the
// top chiefSessionModelsMax catalogue models matching q (every searchrank
// tier, best first), each with its stored id, name, context length and list
// price. Owner-only: the signed-in user must be the owner whose chief the
// door serves (ChiefLLMOwner), and the door must be on (else 404).
func (s *Server) handleChiefSessionModels(w http.ResponseWriter, r *http.Request) {
	if s.chiefSessions == nil {
		http.NotFound(w, r)
		return
	}
	p, ok := authz.From(r.Context())
	if !ok || p.UserID <= 0 || !ChiefLLMOwner(p.UserID) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "only the owner whose chief runs OpenRouter sessions may search their models"})
		return
	}
	cat, err := s.chiefSessionCatalog(r.Context())
	if err != nil {
		s.logger.Printf("chief sessions: model search: %v", err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": ChiefSessionCatalogUnreadable})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	hits := rankCatalog(strings.TrimPrefix(q, agents.OpenRouterModelPrefix), cat, 3)
	out := make([]chiefSessionModelHit, 0, chiefSessionModelsMax)
	for _, m := range hits {
		if len(out) == chiefSessionModelsMax {
			break
		}
		out = append(out, chiefSessionModelHit{ID: catalogSlug(m.ID), Name: m.Name, ContextLength: m.ContextLength, Pricing: m.Pricing})
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	s.writeJSON(w, http.StatusOK, out)
}
