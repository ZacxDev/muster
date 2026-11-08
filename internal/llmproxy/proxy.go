// Package llmproxy is muster's LLM proxy for officer instances: an
// Anthropic-Messages-shaped endpoint (POST /llm/v1/messages on the connect
// listener, internal/api) that a routine officer instance's Claude Code points
// ANTHROPIC_BASE_URL at, and that forwards to one provider (OpenRouter's
// Anthropic-compatible POST /api/v1/messages) with the officer's key.
//
// Per request, in order: the body is read and rebuilt from an allowlist of
// Anthropic Messages fields (forwardBodyFields), its `model` set to the
// officer's pinned model (the client's is ignored); the officer's spend this
// UTC day is read from the store and the request refused once it reaches the
// daily budget; the instance's tokens on its held item are checked against the
// per-item cap; the request goes upstream with the key attached; the response
// is relayed event by event (server-sent events are flushed as each arrives);
// the usage and cost the provider reported in the stream are booked to the
// store before the response ends, so the instance's next request sees them;
// and one audit record and one log line are written.
//
// The admin's chief is the one other caller (Caller.Unbudgeted): the same
// allowlist, pin, key handling and metering, on the one key the route layer
// hands it (the server's MUSTER_AGENT_OPENROUTER_KEY, internal/api), and with
// no budget, no item cap and no usage row — the key's own provider-side credit
// limit is its cap, so a 402 is answered with the caller's KeyRemedy.
//
// 🔴 THE KEY IS ATTACHED HERE AND NOWHERE ELSE. It is never logged, never
// audited, never sent to the client: every byte relayed downstream is passed
// through redact first, a 401/403 body (and a KeyRemedy caller's 402 body) is
// replaced by muster's own message, and every other non-2xx body is relayed
// redacted. The client's own
// credential (its muster token) is never sent upstream.
//
// 🔴 THE MODEL PIN HOLDS BECAUSE THE BODY IS AN ALLOWLIST. OpenRouter reads
// routing fields in the body (fallbacks, models, provider, route, plugins,
// transforms) that can serve a request from a model other than the pinned
// one. Only the Anthropic Messages fields in forwardBodyFields go upstream;
// every other key is dropped. Inside `messages`, a mid-conversation `system`
// entry (which the provider refuses) is folded into the user turn
// (foldSystemMessages); nothing else in a message is changed.
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
	"net/http/httptrace"
	"regexp"
	"sort"
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

// Caller is the agent a request is made for, with its settings: an officer
// routine instance (budgeted), or the chief (Unbudgeted). The route layer
// builds it after authenticating the agent.
type Caller struct {
	// Officer labels the caller in metrics, logs and the audit detail: the
	// officer id, or "chief".
	Officer string
	// Instance is the routine instance's number in its pool, and
	// InstanceLabel how it is named in metrics and logs (`r<n>`).
	Instance         int
	InstanceLabel    string
	AgentID          int64
	AgentName        string
	Model            string
	DailyBudgetUSD   float64
	MaxTokensPerItem int64
	Key              string
	// Unbudgeted: muster enforces NO budget and NO per-item cap for this
	// caller, and keeps no usage row for it (the store is neither read nor
	// written); the cap is the key's own provider-side credit limit. Usage is
	// still metered in the metrics, the log line and the audit record.
	//
	// 🔴 IT IS A SEPARATE FIELD SO THAT ZERO VALUES STAY SAFE. A budgeted
	// caller (the zero value, false) whose DailyBudgetUSD is 0 is refused on
	// its first request (spent 0 >= budget 0), exactly as an officer with a
	// missing budget always was: "no budget" can never be spelled as a 0.
	Unbudgeted bool
	// Title is the X-Title header the provider attributes the request to;
	// "" is "muster officer".
	Title string
	// KeyRemedy is what the client is told when the provider refuses the key
	// (401/403), and the log line's remedy; "" is the officer's (the operator's
	// MUSTER_OFFICER_OPENROUTER_KEY_<NAME>). A caller with one is also told it
	// on a 402 (a spent credit limit), which is NOT counted as a refused key;
	// an officer's 402 is relayed.
	KeyRemedy string
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

// Record is one request's audit record. It carries no request or response
// body, no header and no credential; on a provider error, only the provider's
// error type and message (UpstreamError), redacted and truncated.
type Record struct {
	Officer        string
	Instance       string // the instance's label, `r<n>`
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
	// UpstreamError is a non-2xx provider answer's error, "<type>: <message>",
	// redacted and truncated by upstreamError; "" on a 2xx.
	UpstreamError string
}

// Detail is r as one line of key=value pairs (the audit row's detail and the
// log line), in a fixed order. upstream_error, quoted, ends the line when the
// provider answered with an error.
func (r Record) Detail() string {
	d := fmt.Sprintf("officer=%s instance=%s item=%s model=%s requested_model=%s provider=%s msg=%s status=%d result=%s "+
		"in=%d cache_read=%d cache_write=%d out=%d usd=%.6f estimated=%t stop=%s ms=%d",
		r.Officer, r.Instance, orDash(r.Item), orDash(r.Model), orDash(r.RequestedModel), orDash(r.Provider), orDash(r.MessageID),
		r.Status, r.Result, r.Input, r.CacheRead, r.CacheWrite, r.Output, r.CostUSD, r.CostEstimated, orDash(r.StopReason),
		r.Duration.Milliseconds())
	if r.UpstreamError != "" {
		d += fmt.Sprintf(" upstream_error=%q", r.UpstreamError)
	}
	return d
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
	// Store keeps each officer's spend. nil is a proxy with no budget store:
	// it serves Unbudgeted callers only, and a budgeted caller is refused
	// (refused_store, nothing sent) because its spend cannot be read — never
	// served as if it had no budget.
	Store Store
	// Audit receives one record per request ServeMessages answers (a
	// malformed body's refusal included); nil records none.
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
	st := cfg.Store
	if st == nil {
		st = noStore{}
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
	return &Proxy{upstream: up, http: hc, store: st, audit: cfg.Audit, log: lg, now: now, items: newItems(),
		authFailed: map[string]int{}}, nil
}

// noStore is the Store of a proxy built with none: every read fails, so a
// budgeted caller there takes the ordinary "the budget could not be read"
// refusal and nothing is sent.
type noStore struct{}

var errNoStore = errors.New("llmproxy: this proxy has no budget store")

func (noStore) SpentToday(context.Context, string, time.Time) (float64, error) { return 0, errNoStore }
func (noStore) Book(context.Context, Usage) error                              { return errNoStore }

// Claimed tells the per-item meter that instance of officer now holds item.
func (p *Proxy) Claimed(officer string, instance int, item string) {
	p.items.claimed(instanceKey{officer, instance}, item, p.now())
}

// ClaimedAttempt is Claimed, with HQ's `attempts` for the claim.
func (p *Proxy) ClaimedAttempt(officer string, instance int, item string, attempts int) {
	p.Claimed(officer, instance, item)
}

// ItemCost is the USD the instance's hold on item has booked.
func (p *Proxy) ItemCost(officer string, instance int, item string) (float64, bool) {
	return 0, false
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

// forwardBodyFields are the request-body keys sent upstream: the Anthropic
// Messages request fields Claude Code uses, `model` aside (always the pin).
// Every other key, OpenRouter's routing fields above all, is dropped.
var forwardBodyFields = map[string]bool{
	"messages": true, "system": true, "max_tokens": true, "metadata": true, "stop_sequences": true,
	"stream": true, "temperature": true, "top_k": true, "top_p": true, "tools": true, "tool_choice": true,
	"thinking": true, "context_management": true,
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
	inst := c.InstanceLabel
	k := instanceKey{c.Officer, c.Instance}
	rec := Record{Officer: c.Officer, Instance: inst, AgentID: c.AgentID, AgentName: c.AgentName, Model: c.Model}
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
	fwd := make(map[string]json.RawMessage, len(forwardBodyFields)+1)
	var dropped []string
	for key, v := range msg {
		if forwardBodyFields[key] {
			fwd[key] = v
		} else if key != "model" {
			dropped = append(dropped, key)
		}
	}
	if raw, ok := fwd["messages"]; ok {
		if folded, n := foldSystemMessages(raw); n > 0 {
			fwd["messages"] = folded
			p.log.Printf("llm proxy: agent=%q officer=%s: folded %d mid-conversation system message(s) into the user turn",
				c.AgentName, c.Officer, n)
		}
	}
	pinned, _ := json.Marshal(c.Model)
	fwd["model"] = pinned
	if len(dropped) > 0 {
		sort.Strings(dropped)
		// Key names only, never their values.
		p.log.Printf("llm proxy: agent=%q officer=%s: dropped body fields not forwarded: %s", c.AgentName, c.Officer,
			orDash(strings.Join(dropped, ",")))
	}
	out, err := json.Marshal(fwd)
	if err != nil {
		Refuse(w, http.StatusBadRequest, "invalid_request_error", "the body could not be re-encoded")
		finish("refused_bad_request", http.StatusBadRequest)
		return
	}

	day := Day(start)
	item := ""
	// An Unbudgeted caller (the chief) skips the budget and the item cap
	// entirely: the key's own credit limit is its cap (Caller.Unbudgeted).
	if !c.Unbudgeted {
		metrics.OfficerLLMBudget.WithLabelValues(c.Officer).Set(c.DailyBudgetUSD)
		spent, err := p.store.SpentToday(r.Context(), c.Officer, day)
		if err != nil {
			p.log.Printf("llm proxy: officer %s: the day's spend could not be read (refusing, nothing sent): %v", c.Officer, err)
			Refuse(w, http.StatusServiceUnavailable, "api_error", "the officer's budget could not be read just now; nothing was sent")
			finish("refused_store", http.StatusServiceUnavailable)
			return
		}
		metrics.OfficerLLMSpentToday.WithLabelValues(c.Officer).Set(spent)
		if spent >= c.DailyBudgetUSD {
			Refuse(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(
				"officer %s has spent its daily LLM budget ($%.4f of $%.2f, UTC day %s); nothing was sent. "+
					"Stop here: the item stays queued.", c.Officer, spent, c.DailyBudgetUSD, day.Format("2006-01-02")))
			finish("refused_budget", http.StatusBadRequest)
			return
		}
		var used int64
		used, item = p.items.used(k)
		rec.Item = item
		// 🔴 THE ITEM CAP BINDS A HELD ITEM ONLY. A turn holding none (a
		// kickoff, a re-prompt, the turn between items) is no item's work: the
		// daily budget above is its wall. Capping it killed kickoffs whose first
		// request re-read, uncached, a resumed context larger than the cap.
		if item != "" && used >= c.MaxTokensPerItem {
			Refuse(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(
				"this instance has used %d tokens on item %s, the cap is %d; nothing was sent. Stop here: the item will be "+
					"handed on when its lease lapses.", used, item, c.MaxTokensPerItem))
			finish("refused_item_cap", http.StatusBadRequest)
			return
		}
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
	title := c.Title
	if title == "" {
		title = "muster officer"
	}
	up.Header.Set("X-Title", title)
	up.Header.Set("Authorization", "Bearer "+c.Key)

	m := &meter{}
	// wrote: the request was written to the provider. A connection that fails
	// before that (DNS, refused, TLS) reached no provider, which billed nothing.
	var wroteMu sync.Mutex
	wrote := false
	up = up.WithContext(httptrace.WithClientTrace(up.Context(), &httptrace.ClientTrace{
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			if i.Err == nil {
				wroteMu.Lock()
				wrote = true
				wroteMu.Unlock()
			}
		},
	}))
	resp, err := p.http.Do(up)
	if err != nil {
		wroteMu.Lock()
		sent := wrote
		wroteMu.Unlock()
		if sent {
			// Written, then no response: the provider may have billed it, so it
			// is booked from the pessimistic estimate.
			p.book(r.Context(), c, k, item, day, m, len(body), &rec)
		} else {
			p.log.Printf("llm proxy: officer %s: the provider could not be reached (nothing sent, nothing booked): %v", c.Officer, err)
		}
		Refuse(w, http.StatusBadGateway, "api_error", "the provider could not be reached")
		finish("upstream_error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		p.upstreamAuthFailed(c, resp.StatusCode)
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		rec.UpstreamError = upstreamError(b, c.Key)
		// 🔴 A 400, NOT THE PROVIDER'S STATUS: Claude Code retries a 401 about
		// ten times with backoff, which on an instance reads as a stall; a 400
		// ends the turn with this message. The audit and the metrics keep the
		// provider's own status.
		text := fmt.Sprintf("the provider refused muster's key for officer %s (HTTP %d). This is a muster configuration fault, not "+
			"the model: the operator has been told.", c.Officer, resp.StatusCode)
		if c.KeyRemedy != "" {
			text = fmt.Sprintf("the provider refused the OpenRouter key (HTTP %d): %s", resp.StatusCode, c.KeyRemedy)
		}
		Refuse(w, http.StatusBadRequest, "authentication_error", text)
		finish("upstream_auth", resp.StatusCode)
		return
	}
	if resp.StatusCode == http.StatusPaymentRequired && c.KeyRemedy != "" {
		// 🔴 A 402 IS A SPENT CREDIT LIMIT, NOT A BROKEN KEY. For a caller whose
		// only cap is its key's provider-side credit limit (the chief), the 402
		// is the expected way that cap is hit, so it is answered with the
		// caller's remedy, as a 400 for the same reason as above, and it does
		// NOT go through upstreamAuthFailed: the key is fine, so the
		// auth-failing gauge and the 🔴 key log line stay untouched. An officer
		// (no KeyRemedy) still has its 402 relayed below.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		rec.UpstreamError = upstreamError(b, c.Key)
		p.log.Printf("llm proxy: %s: the provider answered HTTP 402 (payment required: the key's credit limit is probably "+
			"spent, agent %q): %s", c.Officer, c.AgentName, c.KeyRemedy)
		Refuse(w, http.StatusBadRequest, "billing_error", fmt.Sprintf("the provider refused the request for payment "+
			"(HTTP 402): the OpenRouter key's credit limit is probably used up; %s", c.KeyRemedy))
		finish("upstream_status", resp.StatusCode)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		rec.UpstreamError = upstreamError(b, c.Key)
		relayHeaders(w, resp)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(redactError(b, c.Key))
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
			if _, err := w.Write(redact(b, c.Key)); err != nil {
				return err
			}
			return rc.Flush()
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
// echo the key, and this makes sure an echo never reaches the instance. It is
// the one rule for a 2xx body (the model's own output), which is otherwise
// relayed unchanged.
func redact(b []byte, key string) []byte {
	if key == "" || !bytes.Contains(b, []byte(key)) {
		return b
	}
	return bytes.ReplaceAll(b, []byte(key), []byte("[redacted]"))
}

// keyShaped is an `sk-…` key of any provider; authSchemeToken a token after a
// bearer scheme word.
var (
	keyShaped       = regexp.MustCompile(`sk-[A-Za-z0-9][A-Za-z0-9_-]{15,}`)
	authSchemeToken = regexp.MustCompile(`(?i)\b(bearer)\s+[^\s"',;]+`)
)

// redactError is redact, then anything key-shaped, for a provider's non-2xx
// body: both the body relayed to the instance and the record's upstream_error
// go through it, so the two never disagree. It is not applied to a 2xx body,
// where the model may legitimately write `Bearer ${token}` in code.
func redactError(b []byte, key string) []byte {
	b = keyShaped.ReplaceAll(redact(b, key), []byte("[redacted]"))
	return authSchemeToken.ReplaceAll(b, []byte("${1} [redacted]"))
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
		// Nothing reported (the request was written but never answered, or the
		// stream broke off before message_start): assume the provider read the whole
		// request (about 3 bytes a token) and wrote estimateOutputTokens.
		estimated = true
		cost = float64(reqBytes/3)*EstimateUSDPerInputToken + estimateOutputTokens*EstimateUSDPerOutputToken
	}
	rec.CostUSD, rec.CostEstimated = cost, estimated
	inst := c.InstanceLabel
	metrics.OfficerLLMTokens.WithLabelValues(c.Officer, inst, "in").Add(float64(rec.Input))
	metrics.OfficerLLMTokens.WithLabelValues(c.Officer, inst, "out").Add(float64(rec.Output))
	metrics.OfficerLLMTokens.WithLabelValues(c.Officer, inst, "cache_read").Add(float64(rec.CacheRead))
	metrics.OfficerLLMTokens.WithLabelValues(c.Officer, inst, "cache_write").Add(float64(rec.CacheWrite))
	metrics.OfficerLLMCost.WithLabelValues(c.Officer, inst).Add(cost)
	if estimated {
		metrics.OfficerLLMCostEstimated.WithLabelValues(c.Officer).Inc()
	}
	if c.Unbudgeted {
		// 🔴 NOT BOOKED, DELIBERATELY, AND SAID SO. Nothing reads an Unbudgeted
		// caller's spend (no budget), and officer_llm_usage's officer column
		// admits only `<type>:<area>` ids, so a "chief" row would be refused by
		// the database. Its usage is in the metrics above, the log line and the
		// audit record (Record.Detail), which is where it is read.
		return
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
//
// The state is kept per Caller.Officer. The chief is one caller ("chief", the
// admin's chief on the one server key), so its state is that key's. A 402 never
// reaches here for a KeyRemedy caller: a spent credit limit is not a refused
// key. A 403 does, whatever the provider meant by it.
func (p *Proxy) upstreamAuthFailed(c Caller, status int) {
	officer := c.Officer
	metrics.OfficerLLMUpstreamAuth.WithLabelValues(officer, strconv.Itoa(status)).Inc()
	metrics.OfficerLLMUpstreamAuthFailing.WithLabelValues(officer).Set(1)
	p.mu.Lock()
	prev := p.authFailed[officer]
	p.authFailed[officer] = status
	p.mu.Unlock()
	if prev != status {
		if c.KeyRemedy != "" {
			p.log.Printf("llm proxy: 🔴 THE PROVIDER REFUSES %s's OPENROUTER KEY (HTTP %d, agent %q). Every request "+
				"fails until the key is fixed: %s", officer, status, c.AgentName, c.KeyRemedy)
			return
		}
		p.log.Printf("llm proxy: 🔴 THE PROVIDER REFUSES OFFICER %s's KEY (HTTP %d). Every request of its routine "+
			"instances fails until the key is fixed; Claude Code retries a 401 about ten times, so on the instance this "+
			"looks like a stall. Check the officer's key (%s<NAME>).", officer, status, "MUSTER_OFFICER_OPENROUTER_KEY_")
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
