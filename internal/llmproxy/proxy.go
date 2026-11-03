// Package llmproxy is muster's LLM proxy for officer instances: an
// Anthropic-Messages-shaped endpoint (POST /llm/v1/messages on the connect
// listener, internal/api) that a routine officer instance's Claude Code points
// ANTHROPIC_BASE_URL at, and that forwards to one provider (OpenRouter's
// Anthropic-compatible POST /api/v1/messages) with the officer's key.
//
// Per request, in order: the body is read and its `model` rewritten to the
// officer's pinned model (the client's is ignored); the officer's spend this
// UTC day is read from the store and the request refused once it reaches the
// daily budget; the instance's tokens on its held item are checked against the
// per-item cap; the request goes upstream with the key attached; the response
// is relayed event by event (server-sent events are flushed as each arrives);
// the usage and cost the provider reported in the stream are booked to the
// store before the response ends, so the instance's next request sees them;
// and one audit record and one log line are written.
//
// 🔴 THE KEY IS ATTACHED HERE AND NOWHERE ELSE. It is never logged, never
// audited, never sent to the client: every byte relayed downstream is passed
// through redact first, and an upstream error body is replaced by muster's
// own. The client's own credential (its muster token) is never sent upstream.
//
// 🔴 COST IS THE PROVIDER'S, NOT A PRICE LIST'S. OpenRouter reports what it
// billed in the stream (`usage.cost` in message_delta). Billed prices have run
// well above list prices, so only a response that carries no cost is booked
// from an estimate, at deliberately pessimistic rates (EstimateUSDPerInputToken,
// EstimateUSDPerOutputToken), and counted as estimated.
package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
)

// DefaultUpstream is the provider's Anthropic-compatible API base; requests go
// to DefaultUpstream + UpstreamPath.
const DefaultUpstream = "https://openrouter.ai/api"

// UpstreamPath is the one provider path the proxy calls. Claude Code calls
// only POST /v1/messages (with ?beta=true) against its base URL, always
// streamed; no count_tokens or model-list call was observed. Every other path
// under /llm/ is refused by the route layer.
const UpstreamPath = "/v1/messages"

// Pessimistic $/token for a response that reported no cost. NOT list prices:
// they are set above the highest billed rate observed for the pinned models,
// so an estimate over-books rather than under-books.
const (
	EstimateUSDPerInputToken  = 0.60e-6
	EstimateUSDPerOutputToken = 1.20e-6
)

// Limits.
const (
	// MaxRequestBytes bounds a request body (a long session's whole context).
	MaxRequestBytes = 32 << 20
	// maxErrorBodyBytes bounds how much of a non-2xx upstream body is read.
	maxErrorBodyBytes = 64 << 10
	// maxJSONBodyBytes bounds a non-streamed 2xx body.
	maxJSONBodyBytes = 32 << 20
	// estimateOutputTokens is the output assumed for a request that reported
	// no usage at all.
	estimateOutputTokens = 4096
)

// Caller is the officer instance a request is made for, with its officer's
// settings. The route layer builds it after authenticating the instance.
type Caller struct {
	Officer          string
	Instance         int
	AgentID          int64
	AgentName        string
	Model            string
	DailyBudgetUSD   float64
	MaxTokensPerItem int64
	Key              string
}

// Usage is one request's metered usage, booked to the store.
type Usage struct {
	Officer    string
	Instance   int
	Day        time.Time // a UTC midnight
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	CostUSD    float64
	Estimated  bool
}

// Store persists each officer's usage per instance and UTC day, so a restart
// does not reset a budget.
type Store interface {
	// SpentToday is officer's booked USD on day, over every instance.
	SpentToday(ctx context.Context, officer string, day time.Time) (float64, error)
	// Book adds u (one request) to its (officer, instance, day) row.
	Book(ctx context.Context, u Usage) error
}

// Record is one request's audit record. It carries no body, no header and no
// credential.
type Record struct {
	Officer        string
	Instance       int
	AgentID        int64
	AgentName      string
	Item           string
	RequestedModel string
	Model          string
	Provider       string
	MessageID      string
	Status         int
	Result         string
	Input          int64
	Output         int64
	CacheRead      int64
	CacheWrite     int64
	CostUSD        float64
	CostEstimated  bool
	StopReason     string
	Duration       time.Duration
}

// Detail is r as one line of key=value pairs (the audit row's detail and the
// log line), in a fixed order.
func (r Record) Detail() string {
	return fmt.Sprintf("officer=%s instance=%d item=%s model=%s requested_model=%s provider=%s msg=%s status=%d result=%s "+
		"in=%d cache_read=%d cache_write=%d out=%d usd=%.6f estimated=%t stop=%s ms=%d",
		r.Officer, r.Instance, orDash(r.Item), orDash(r.Model), orDash(r.RequestedModel), orDash(r.Provider), orDash(r.MessageID),
		r.Status, r.Result, r.Input, r.CacheRead, r.CacheWrite, r.Output, r.CostUSD, r.CostEstimated, orDash(r.StopReason),
		r.Duration.Milliseconds())
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return strings.Join(strings.Fields(s), "_")
}

// Config is New's input.
type Config struct {
	// Upstream is the provider's API base ("" is DefaultUpstream).
	Upstream string
	// HTTP is the upstream transport; nil is a client that follows no
	// redirect.
	HTTP *http.Client
	// Store is required.
	Store Store
	// Audit receives one record per request that reached the budget check;
	// nil records none.
	Audit  func(ctx context.Context, r Record)
	Logger *log.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Proxy is the LLM proxy.
type Proxy struct {
	upstream string
	http     *http.Client
	store    Store
	audit    func(context.Context, Record)
	log      *log.Logger
	now      func() time.Time
	items    *items

	mu         sync.Mutex
	authFailed map[string]int // officer -> the provider status (401/403) while failing
}

// New builds a Proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.Store == nil {
		return nil, errors.New("llmproxy: a store is required (the daily budget must survive a restart)")
	}
	up := strings.TrimRight(cfg.Upstream, "/")
	if up == "" {
		up = DefaultUpstream
	}
	if !strings.HasPrefix(up, "https://") && !strings.HasPrefix(up, "http://") {
		return nil, fmt.Errorf("llmproxy: upstream %q is not an http(s) URL", cfg.Upstream)
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{
			// No overall timeout: a streamed response lasts as long as the
			// model writes. The request's own context ends it.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ResponseHeaderTimeout: 5 * time.Minute,
				TLSHandshakeTimeout:   15 * time.Second,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Proxy{upstream: up, http: hc, store: cfg.Store, audit: cfg.Audit, log: lg, now: now, items: newItems(),
		authFailed: map[string]int{}}, nil
}

// Claimed tells the per-item meter that instance of officer now holds item.
func (p *Proxy) Claimed(officer string, instance int, item string) {
	p.items.claimed(instanceKey{officer, instance}, item, p.now())
}

// Ended tells the per-item meter that instance of officer no longer holds item.
func (p *Proxy) Ended(officer string, instance int, item string) {
	p.items.ended(instanceKey{officer, instance}, item, p.now())
}

// Sync is a wake-loop pass's view of officer's claims, read at at: holding
// maps an instance number to the items HQ shows it claiming.
func (p *Proxy) Sync(officer string, holding map[int][]string, at time.Time) {
	p.items.sync(officer, holding, at)
}

// Day is the UTC day t falls on, as the store keys it.
func Day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Refuse writes an Anthropic-shaped error, which Claude Code shows as the
// turn's API error.
//
// ⚠ Status choice matters to the client: Claude Code retries 429 and 5xx with
// backoff (and a 401 about ten times), so a refusal that must END the turn —
// the budget, the item cap — is a 400, never a 429.
func Refuse(w http.ResponseWriter, status int, errType, msg string) {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": errType, "message": "muster llm proxy: " + msg}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// forwardRequestHeaders are the client headers sent upstream. Everything else
// — the client's Authorization / X-Muster-Token / x-api-key, cookies, its
// session id — stays here.
var forwardRequestHeaders = []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta", "User-Agent", "X-App"}

// relayResponseHeaders are the upstream headers sent back to the client.
var relayResponseHeaders = []string{"Content-Type", "Cache-Control", "Request-Id", "X-Generation-Id", "Retry-After"}

// ServeMessages proxies one POST /llm/v1/messages for c.
func (p *Proxy) ServeMessages(w http.ResponseWriter, r *http.Request, c Caller) {
	start := p.now()
	inst := strconv.Itoa(c.Instance)
	k := instanceKey{c.Officer, c.Instance}
	rec := Record{Officer: c.Officer, Instance: c.Instance, AgentID: c.AgentID, AgentName: c.AgentName, Model: c.Model}
	finish := func(result string, status int) {
		rec.Result, rec.Status, rec.Duration = result, status, p.now().Sub(start)
		metrics.OfficerLLMRequests.WithLabelValues(c.Officer, inst, result).Inc()
		p.log.Printf("llm proxy: agent=%q %s", c.AgentName, rec.Detail())
		if p.audit != nil {
			p.audit(context.WithoutCancel(r.Context()), rec)
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err != nil {
		Refuse(w, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("the request body could not be read (at most %d bytes)", MaxRequestBytes))
		finish("refused_bad_request", http.StatusRequestEntityTooLarge)
		return
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil || msg == nil {
		Refuse(w, http.StatusBadRequest, "invalid_request_error", "the body is not a JSON object")
		finish("refused_bad_request", http.StatusBadRequest)
		return
	}
	if raw, ok := msg["model"]; ok {
		_ = json.Unmarshal(raw, &rec.RequestedModel)
	}
	pinned, _ := json.Marshal(c.Model)
	_ = pinned /* MUTANT M2 */
	out, err := json.Marshal(msg)
	if err != nil {
		Refuse(w, http.StatusBadRequest, "invalid_request_error", "the body could not be re-encoded")
		finish("refused_bad_request", http.StatusBadRequest)
		return
	}

	day := Day(start)
	metrics.OfficerLLMBudget.WithLabelValues(c.Officer).Set(c.DailyBudgetUSD)
	spent, err := p.store.SpentToday(r.Context(), c.Officer, day)
	if err != nil {
		p.log.Printf("llm proxy: officer %s: the day's spend could not be read (refusing, nothing sent): %v", c.Officer, err)
		Refuse(w, http.StatusServiceUnavailable, "api_error", "the officer's budget could not be read just now; nothing was sent")
		finish("refused_store", http.StatusServiceUnavailable)
		return
	}
	metrics.OfficerLLMSpentToday.WithLabelValues(c.Officer).Set(spent)
	if spent >= c.DailyBudgetUSD*1e9 { /* MUTANT M3 */
		Refuse(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(
			"officer %s has spent its daily LLM budget ($%.4f of $%.2f, UTC day %s); nothing was sent. "+
				"Stop here: the item stays queued.", c.Officer, spent, c.DailyBudgetUSD, day.Format("2006-01-02")))
		finish("refused_budget", http.StatusBadRequest)
		return
	}
	used, item := p.items.used(k)
	rec.Item = item
	if used >= c.MaxTokensPerItem {
		what := "item " + item
		if item == "" {
			what = "the work before any claimed item"
		}
		Refuse(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(
			"this instance has used %d tokens on %s, the cap is %d; nothing was sent. Stop here: the item will be "+
				"handed on when its lease lapses.", used, what, c.MaxTokensPerItem))
		finish("refused_item_cap", http.StatusBadRequest)
		return
	}

	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstream+UpstreamPath+upstreamQuery(r), bytes.NewReader(out))
	if err != nil {
		Refuse(w, http.StatusInternalServerError, "api_error", "the upstream request could not be built")
		finish("upstream_error", http.StatusInternalServerError)
		return
	}
	for _, h := range forwardRequestHeaders {
		if v := r.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept-Encoding", "identity")
	up.Header.Set("X-Title", "muster officer")
	up.Header.Set("Authorization", "Bearer "+c.Key)

	m := &meter{}
	resp, err := p.http.Do(up)
	if err != nil {
		p.book(r.Context(), c, k, item, day, m, len(body), &rec)
		Refuse(w, http.StatusBadGateway, "api_error", "the provider could not be reached")
		finish("upstream_error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		p.upstreamAuthFailed(c.Officer, resp.StatusCode)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		Refuse(w, resp.StatusCode, "authentication_error", fmt.Sprintf(
			"the provider refused muster's key for officer %s (HTTP %d). This is a muster configuration fault, not "+
				"the model: the operator has been told.", c.Officer, resp.StatusCode))
		finish("upstream_auth", resp.StatusCode)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		relayHeaders(w, resp)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(redact(b, c.Key))
		finish("upstream_status", resp.StatusCode)
		return
	}
	p.upstreamAuthOK(c.Officer)

	relayHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	streamErr := error(nil)
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		f := &framer{m: m, emit: func(b []byte) error {
			if _, err := w.Write(b); err != nil { /* MUTANT M4 */
				return err
			}
			return nil /* MUTANT M1 */
		}}
		buf := make([]byte, 32<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if err := f.write(buf[:n]); err != nil {
					streamErr = err
					break
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				streamErr = rerr
				break
			}
		}
		if streamErr == nil {
			streamErr = f.flush()
		}
	} else {
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, maxJSONBodyBytes))
		m.observe(b)
		_, werr := w.Write(redact(b, c.Key))
		streamErr = errors.Join(rerr, werr)
	}
	// Booked BEFORE the response ends (this handler returning ends it), so the
	// instance's next request is checked against it.
	p.book(r.Context(), c, k, item, day, m, len(body), &rec)
	if streamErr != nil || m.ErrorType != "" {
		finish("upstream_error", resp.StatusCode)
		return
	}
	finish("ok", resp.StatusCode)
}

// upstreamQuery forwards the one query Claude Code sends (`beta=true`) and
// nothing else.
func upstreamQuery(r *http.Request) string {
	if r.URL.RawQuery == "beta=true" {
		return "?beta=true"
	}
	return ""
}

func relayHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, h := range relayResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
}

// redact replaces every occurrence of key in b. A provider has no reason to
// echo the key, and this makes sure an echo never reaches the instance.
func redact(b []byte, key string) []byte {
	if key == "" || !bytes.Contains(b, []byte(key)) {
		return b
	}
	return bytes.ReplaceAll(b, []byte(key), []byte("[redacted]"))
}

// book meters one request's usage: metrics, the per-item count, the store.
func (p *Proxy) book(ctx context.Context, c Caller, k instanceKey, item string, day time.Time, m *meter, reqBytes int, rec *Record) {
	u := m.Usage
	rec.Provider, rec.MessageID, rec.StopReason = m.Provider, m.MessageID, m.StopReason
	if m.Model != "" {
		rec.Model = m.Model
	}
	rec.Input, rec.Output, rec.CacheRead, rec.CacheWrite = val(u.Input), val(u.Output), val(u.CacheRead), val(u.CacheWrite)
	var cost float64
	estimated := false
	switch {
	case u.Cost != nil && *u.Cost >= 0:
		cost = *u.Cost
	case u.seen():
		estimated = true
		cost = float64(rec.Input+rec.CacheRead+rec.CacheWrite)*EstimateUSDPerInputToken + float64(rec.Output)*EstimateUSDPerOutputToken
	default:
		// Nothing reported (the provider never answered, or the stream broke
		// off before message_start): assume the provider read the whole
		// request (about 3 bytes a token) and wrote estimateOutputTokens.
		estimated = true
		cost = float64(reqBytes/3)*EstimateUSDPerInputToken + estimateOutputTokens*EstimateUSDPerOutputToken
	}
	rec.CostUSD, rec.CostEstimated = cost, estimated
	inst := strconv.Itoa(c.Instance)
	metrics.OfficerLLMTokens.WithLabelValues(c.Officer, inst, "in").Add(float64(rec.Input))
	metrics.OfficerLLMTokens.WithLabelValues(c.Officer, inst, "out").Add(float64(rec.Output))
	metrics.OfficerLLMTokens.WithLabelValues(c.Officer, inst, "cache_read").Add(float64(rec.CacheRead))
	metrics.OfficerLLMTokens.WithLabelValues(c.Officer, inst, "cache_write").Add(float64(rec.CacheWrite))
	metrics.OfficerLLMCost.WithLabelValues(c.Officer, inst).Add(cost)
	if estimated {
		metrics.OfficerLLMCostEstimated.WithLabelValues(c.Officer).Inc()
	}
	p.items.add(k, item, u.tokens())
	err := p.store.Book(context.WithoutCancel(ctx), Usage{Officer: c.Officer, Instance: c.Instance, Day: day,
		Input: rec.Input, Output: rec.Output, CacheRead: rec.CacheRead, CacheWrite: rec.CacheWrite, CostUSD: cost, Estimated: estimated})
	if err != nil {
		metrics.OfficerLLMBookFailures.WithLabelValues(c.Officer).Inc()
		p.log.Printf("llm proxy: 🔴 officer %s: usage of $%.6f could NOT be written (a restart would forget it): %v", c.Officer, cost, err)
	}
}

// upstreamAuthFailed records a 401/403 from the provider: counted every time,
// logged loudly once when the officer's state changes into failing (or from
// one failing status to the other).
func (p *Proxy) upstreamAuthFailed(officer string, status int) {
	metrics.OfficerLLMUpstreamAuth.WithLabelValues(officer, strconv.Itoa(status)).Inc()
	metrics.OfficerLLMUpstreamAuthFailing.WithLabelValues(officer).Set(1)
	p.mu.Lock()
	prev := p.authFailed[officer]
	p.authFailed[officer] = status
	p.mu.Unlock()
	if prev != status || true { /* MUTANT M7 */
		p.log.Printf("llm proxy: 🔴 THE PROVIDER REFUSES OFFICER %s's KEY (HTTP %d). Every request of its routine "+
			"instances fails until the key is fixed; Claude Code retries a 401 about ten times, so on the instance this "+
			"looks like a stall. Check the officer's key (%s<NAME> or the shared key).", officer, status, "MUSTER_OFFICER_OPENROUTER_KEY_")
	}
}

// upstreamAuthOK records a 2xx after a failing state: logged once.
func (p *Proxy) upstreamAuthOK(officer string) {
	p.mu.Lock()
	prev, was := p.authFailed[officer]
	delete(p.authFailed, officer)
	p.mu.Unlock()
	metrics.OfficerLLMUpstreamAuthFailing.WithLabelValues(officer).Set(0)
	if was {
		p.log.Printf("llm proxy: officer %s: the provider accepts its key again (it had answered HTTP %d)", officer, prev)
	}
}
