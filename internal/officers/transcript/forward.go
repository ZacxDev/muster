// Package transcript forwards an officer instance's LIVE session transcript to
// HQ while the instance holds a claimed item (docs/officers.md, "Transcript to
// HQ"), so HQ's officer pages can show what the officer is doing.
//
// THE SOURCE is the instance's own Claude Code transcript — the structured
// JSONL the CLI writes, decoded by internal/cctranscript exactly as every other
// reader of a session decodes it — served incrementally by the instance pod's
// ccd on GET /v1/transcript (cmd/ccd/transcript_route.go). Never the screen.
//
// THE SINK is HQ's POST /officers/{id}/items/{item}/transcript, sent with the
// officer's own token by officers.Client (the token never leaves it).
//
// 🔴 EVERY TEXT IS REDACTED BEFORE IT LEAVES: the officer's own token is
// replaced (officers.Client.Scrub — a shape filter cannot know an opaque
// token), then the CI-log secret filter (officership.RedactText, the same
// RedactLines officer_ci_logs uses) withholds every line that looks like it
// carries a credential, then the text is cut to the contract's 8 KiB with a
// marker. Transcript text is never logged.
//
// 🔴 THE SEQ IS A FUNCTION OF THE TRANSCRIPT POSITION, NOT OF MEMORY (Seq):
// the end offset of the event's line in the transcript file times 32768, plus the
// event's index within that line. It strictly increases along the file, so a
// restarted muster derives the SAME seq for the same event, and HQ's
// idempotency rule (a seq at or below its highest stored is skipped) turns a
// resume's re-read into no duplicates. A resume re-reads a backfill window
// (Config.Backfill) of the transcript, so events written while muster was
// down are sent too, as long as they fit in it. It stays below 2^53 for any
// file under 256 GiB, so it survives a JSON reader that parses numbers as
// doubles.
//
// The one case the file position cannot order is a NEW CONVERSATION mid-item
// (a new transcript file, offsets from zero — ccd resumes the primary's own
// conversation by id, so this is an operator's /clear, not a restart). A
// forwarder that sees it shifts every later seq above the highest it had
// (worker.shift). A forwarder that restarted INTO such a file has lost the
// shift; HQ then answers stored 0 with a last_seq above a batch read LIVE
// (past the resume's re-read, so it cannot be a duplicate), and the forwarder
// re-sequences that batch and every later one above it (heal). What that case
// can lose is the part of the resume's re-read HQ skipped as old.
//
// 🔴 IT NEVER BLOCKS THE OFFICER. Claimed, Ended and Sync — called from the
// officer tools and the wake loop — only record state under a mutex; every
// read and post runs on the forwarder's own loop (Run → Tick). An instance's
// buffer is capped (Config.MaxBuffer): past it the OLDEST events are dropped
// and counted (muster_officer_transcript_events_total{result="dropped_buffer"}),
// never held without bound.
package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officership"
)

// Query is one read of an instance's transcript.
type Query struct {
	// Session is the session the reader's offset belongs to ("" for none yet).
	Session string
	// After is the offset to read from; -1 is the tail (the last Backfill
	// bytes; Backfill 0 starts live, at the end).
	After    int64
	Backfill int
}

// Source reads an officer instance's transcript (production: the instance
// pod's ccd, GET /v1/transcript).
type Source interface {
	Read(ctx context.Context, o officers.Officer, instance string, q Query) (cctranscript.Page, error)
}

// Poster is the HQ side: the transcript route, and the officer-token scrub.
type Poster interface {
	Post(ctx context.Context, o officers.Officer, item officers.ItemID, instance string, events []officers.TranscriptEvent) (officers.TranscriptAck, error)
	Scrub(o officers.Officer, s string) string
}

type clientPoster struct{ hq *officers.ClientHQ }

// NewClientPoster is a Poster over each officer's own HQ client.
func NewClientPoster(hq *officers.ClientHQ) Poster { return clientPoster{hq: hq} }

func (p clientPoster) Post(ctx context.Context, o officers.Officer, item officers.ItemID, instance string, events []officers.TranscriptEvent) (officers.TranscriptAck, error) {
	return p.hq.Client(o).PostTranscript(ctx, item, instance, events)
}

func (p clientPoster) Scrub(o officers.Officer, s string) string { return p.hq.Client(o).Scrub(s) }

// Defaults.
const (
	DefaultInterval        = 2 * time.Second
	DefaultMaxBuffer       = 1000
	DefaultMaxRetries      = 5
	DefaultMaxBackoff      = time.Minute
	DefaultBackfill        = 256 << 10
	DefaultMaxPostsPerTick = 10
	DefaultRequestTimeout  = 15 * time.Second
)

// Config is New's input. Zero values take the defaults.
type Config struct {
	Source Source
	Poster Poster
	Logger *log.Logger
	Now    func() time.Time
	// Interval is the cadence: how often each instance's transcript is read
	// and its new events posted.
	Interval time.Duration
	// MaxBuffer caps one instance's events read but not yet accepted by HQ.
	MaxBuffer int
	// MaxRetries is how many times one batch is re-sent after an HQ outage
	// (5xx, 429, no answer) before it is dropped (counted).
	MaxRetries int
	// MaxBackoff caps the wait between re-sends (Interval doubled per failure).
	MaxBackoff time.Duration
	// Backfill is the window, in bytes, a RESUME (a held item muster was not
	// forwarding — after a restart) re-reads from the end of the transcript.
	Backfill int
	// MaxPostsPerTick bounds one instance's posts per tick.
	MaxPostsPerTick int
	// RequestTimeout bounds one read or post.
	RequestTimeout time.Duration
}

// Results, the label values of muster_officer_transcript_events_total.
const (
	resultPosted       = "posted"
	resultBuffer       = "dropped_buffer"
	resultRetries      = "dropped_retries"
	resultRefused      = "dropped_refused"
	resultSuperseded   = "dropped_superseded"
	resultUnsequenced  = "dropped_unsequenced"
	truncatedMarkerFmt = "… [truncated %d bytes]"
)

// Forwarder is the transcript forwarder: one worker per officer instance that
// holds a claimed item of a transcript-enabled officer.
type Forwarder struct {
	cfg Config
	log *log.Logger
	now func() time.Time

	tickMu sync.Mutex // one Tick at a time

	mu      sync.Mutex
	workers map[string]*worker // by instance agent name
	// stopped is (officer, instance, item) HQ refused (409 not_claimed …): not
	// restarted by a pass until a new claim, or until the item leaves the
	// held list.
	stopped map[key]bool
}

type key struct {
	officer, instance string
	item              officers.ItemID
}

// worker is one instance's forwarding state. The control fields are guarded
// by Forwarder.mu; the data fields belong to the tick loop alone.
type worker struct {
	o        officers.Officer
	instance string

	// control (Forwarder.mu)
	item  officers.ItemID
	gen   int       // bumped when item changes
	since time.Time // when this item's forwarding began
	dead  bool

	// data (tick only)
	seenGen  int
	session  string
	cursor   int64
	backfill int
	// notBefore, set, skips lines older than it on the first read: a claim's
	// forwarding starts at the claim (less ClaimSlack), not at whatever the
	// backfill window holds of the instance's earlier work.
	notBefore time.Time
	buf       []officers.TranscriptEvent
	lastSeq   int64
	// shift is added to every seq read from the current transcript file (a
	// new conversation mid-item: Seq's doc).
	shift int64
	// maxSeq is the highest seq this worker has assigned.
	maxSeq int64
	// replayMax is the highest seq its FIRST read (a claim's or a resume's
	// window) assigned: a batch above it was read live.
	replayMax   int64
	reads       int
	failures    int
	nextAt      time.Time
	readFailing bool
}

// New builds a forwarder.
func New(cfg Config) (*Forwarder, error) {
	if cfg.Source == nil || cfg.Poster == nil {
		return nil, errors.New("transcript: a forwarder needs a source and a poster")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MaxBuffer <= 0 {
		cfg.MaxBuffer = DefaultMaxBuffer
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.Backfill <= 0 {
		cfg.Backfill = DefaultBackfill
	}
	if cfg.MaxPostsPerTick <= 0 {
		cfg.MaxPostsPerTick = DefaultMaxPostsPerTick
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	return &Forwarder{cfg: cfg, log: cfg.Logger, now: cfg.Now, workers: map[string]*worker{}, stopped: map[key]bool{}}, nil
}

// ClaimSlack is how far before a claim's own time its first read reaches: the
// pod's clock and muster's can differ, and the officer_claim call itself (the
// tool_use, written just before the claim) belongs to the item.
const ClaimSlack = 5 * time.Second

// Claimed is officer_claim succeeding: instance now holds item. Forwarding
// starts AT THE CLAIM: the first read takes the transcript's backfill window
// and keeps only the lines written since the claim (less ClaimSlack) — what
// the instance does from the claim on is the item's, and nothing written
// between the claim and the forwarder's next tick is lost. An instance already
// forwarding another item switches to this one, keeping its place in the
// transcript.
func (f *Forwarder) Claimed(o officers.Officer, instance string, item officers.ItemID) {
	if !o.TranscriptEnabled() {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stopped, key{o.ID, instance, item})
	now := f.now()
	if w := f.workers[instance]; w != nil && !w.dead {
		if w.item != item {
			w.item, w.since = item, now
			w.gen++
		}
		return
	}
	f.workers[instance] = &worker{o: o, instance: instance, item: item, since: now, cursor: -1, backfill: f.cfg.Backfill,
		notBefore: now.Add(-ClaimSlack)}
}

// Ended is officer_complete or officer_release succeeding: the item is no
// longer the instance's, and forwarding for it stops.
func (f *Forwarder) Ended(o officers.Officer, instance string, item officers.ItemID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stopped, key{o.ID, instance, item})
	if w := f.workers[instance]; w != nil && w.item == item {
		w.dead = true
		delete(f.workers, instance)
	}
}

// Sync is one wake-loop pass's view of o's claimed items, read at at: every
// item one of o's instances holds under a live lease is forwarded (a RESUME,
// from the backfill window, when muster was not forwarding it — after a
// restart), and forwarding stops for an item no instance of o holds any more.
// A claim made after at (Claimed) is not undone by this pass.
func (f *Forwarder) Sync(o officers.Officer, claimed []officers.Item, at time.Time) {
	if !o.TranscriptEnabled() {
		return
	}
	reg := officers.NewRegistry([]officers.Officer{o})
	held := map[string][]officers.ItemID{}
	heldKey := map[key]bool{}
	for _, it := range claimed {
		if !it.LeaseLive(at) {
			continue
		}
		if _, _, ok := reg.ForAgentName(it.ClaimedBy); !ok {
			continue
		}
		held[it.ClaimedBy] = append(held[it.ClaimedBy], it.ID)
		heldKey[key{o.ID, it.ClaimedBy, it.ID}] = true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.stopped {
		if k.officer == o.ID && !heldKey[k] {
			delete(f.stopped, k)
		}
	}
	for inst, w := range f.workers {
		if w.o.ID != o.ID || heldKey[key{o.ID, inst, w.item}] || w.since.After(at) {
			continue
		}
		w.dead = true
		delete(f.workers, inst)
	}
	insts := make([]string, 0, len(held))
	for inst := range held {
		insts = append(insts, inst)
	}
	sort.Strings(insts)
	for _, inst := range insts {
		if f.workers[inst] != nil {
			continue // forwarding an item it still holds
		}
		items := held[inst]
		sort.Slice(items, func(i, j int) bool { return items[i] < items[j] })
		for _, item := range items {
			if f.stopped[key{o.ID, inst, item}] {
				continue
			}
			f.workers[inst] = &worker{o: o, instance: inst, item: item, since: f.now(), cursor: -1, backfill: f.cfg.Backfill}
			f.log.Printf("officers: %s: forwarding instance %s's transcript for item %s to HQ (resumed: the instance holds it)", o.ID, inst, item)
			break
		}
	}
}

// Active is each forwarding instance's item.
func (f *Forwarder) Active() map[string]officers.ItemID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]officers.ItemID{}
	for inst, w := range f.workers {
		out[inst] = w.item
	}
	return out
}

// Run ticks every Interval until ctx ends.
func (f *Forwarder) Run(ctx context.Context) {
	t := time.NewTicker(f.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.tick(ctx)
		}
	}
}

func (f *Forwarder) tick(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			f.log.Printf("officers: PANIC in the transcript forwarder: %v", rec)
		}
	}()
	f.Tick(ctx)
}

// Tick runs one step for every worker: read its transcript's new lines, then
// post what is buffered (unless it is backing off).
func (f *Forwarder) Tick(ctx context.Context) {
	f.tickMu.Lock()
	defer f.tickMu.Unlock()
	f.mu.Lock()
	ws := make([]*worker, 0, len(f.workers))
	for _, w := range f.workers {
		ws = append(ws, w)
	}
	f.mu.Unlock()
	sort.Slice(ws, func(i, j int) bool { return ws[i].instance < ws[j].instance })
	for _, w := range ws {
		if ctx.Err() != nil {
			return
		}
		f.step(ctx, w)
	}
}

func (f *Forwarder) step(ctx context.Context, w *worker) {
	f.mu.Lock()
	dead, item, gen := w.dead, w.item, w.gen
	f.mu.Unlock()
	if dead {
		return
	}
	if gen != w.seenGen {
		// A new item: what was buffered for the old one is not sent to it.
		f.count(resultSuperseded, len(w.buf))
		w.buf, w.lastSeq, w.failures, w.nextAt, w.seenGen, w.replayMax = nil, 0, 0, time.Time{}, gen, 0
	}
	f.read(ctx, w)
	f.flush(ctx, w, item, gen)
}

func (f *Forwarder) count(result string, n int) {
	if n > 0 {
		metrics.OfficerTranscriptEvents.WithLabelValues(result).Add(float64(n))
	}
}

// read appends the transcript's new events to w's buffer, mapped, redacted,
// cut and sequenced, then applies the buffer cap.
func (f *Forwarder) read(ctx context.Context, w *worker) {
	rctx, cancel := context.WithTimeout(ctx, f.cfg.RequestTimeout)
	page, err := f.cfg.Source.Read(rctx, w.o, w.instance, Query{Session: w.session, After: w.cursor, Backfill: w.backfill})
	cancel()
	if err != nil {
		if !w.readFailing {
			f.log.Printf("officers: %s: instance %s's transcript cannot be read (retrying every %s): %v", w.o.ID, w.instance, f.cfg.Interval, err)
		}
		w.readFailing = true
		return
	}
	if w.readFailing {
		f.log.Printf("officers: %s: instance %s's transcript reads again", w.o.ID, w.instance)
		w.readFailing = false
	}
	if w.session != "" && page.Session != w.session {
		// A new conversation: its offsets start again from zero, so its seqs
		// are shifted above every seq this worker assigned.
		w.shift = max(w.maxSeq, w.lastSeq) + 1
		f.log.Printf("officers: %s: instance %s's session changed; its transcript seqs continue above %d", w.o.ID, w.instance, w.shift)
	}
	w.session, w.cursor, w.backfill = page.Session, page.Next, 0
	notBefore := w.notBefore
	w.notBefore = time.Time{}
	w.reads++
	// Only seqs THIS worker assigned are skipped here; HQ's last_seq is not
	// (a live event below it is the heal's case, in flush).
	last := w.maxSeq
	now := f.now()
	unsequenced := 0
	for _, l := range page.Lines {
		at := now
		if l.At > 0 {
			at = time.UnixMilli(l.At)
			if at.Before(notBefore) {
				continue // the instance's work before this claim
			}
		}
		for i, ev := range l.Events {
			kind, text, ok := mapEvent(ev)
			if !ok {
				continue
			}
			seq, ok := Seq(l.End, i)
			if seq += w.shift; !ok || seq >= maxSeq {
				unsequenced++
				continue
			}
			if seq <= last {
				continue // already sent (a resume's re-read, a re-served line)
			}
			w.buf = append(w.buf, officers.TranscriptEvent{Seq: seq, At: at.UTC().Format(time.RFC3339Nano), Kind: kind,
				Text: f.clean(w.o, text)})
			last = seq
			w.maxSeq = seq
			if w.reads == 1 {
				w.replayMax = seq
			}
		}
	}
	if unsequenced > 0 {
		f.count(resultUnsequenced, unsequenced)
		f.log.Printf("officers: %s: instance %s: dropped %d transcript event(s) with no valid seq (a line with more than %d events)",
			w.o.ID, w.instance, unsequenced, maxIdx+1)
	}
	if over := len(w.buf) - f.cfg.MaxBuffer; over > 0 {
		w.buf = append([]officers.TranscriptEvent(nil), w.buf[over:]...)
		f.count(resultBuffer, over)
		f.log.Printf("officers: %s: instance %s: transcript buffer full (%d events waiting for HQ): dropped the oldest %d",
			w.o.ID, w.instance, f.cfg.MaxBuffer, over)
	}
}

// flush posts w's buffer in batches of at most MaxTranscriptEvents.
func (f *Forwarder) flush(ctx context.Context, w *worker, item officers.ItemID, gen int) {
	now := f.now()
	if now.Before(w.nextAt) {
		return
	}
	for posts := 0; len(w.buf) > 0 && posts < f.cfg.MaxPostsPerTick; posts++ {
		n := min(len(w.buf), officers.MaxTranscriptEvents)
		batch := w.buf[:n]
		pctx, cancel := context.WithTimeout(ctx, f.cfg.RequestTimeout)
		ack, err := f.cfg.Poster.Post(pctx, w.o, item, w.instance, batch)
		cancel()
		var hqErr *officers.HQError
		switch {
		case err == nil && ack.Stored == 0 && ack.LastSeq >= batch[n-1].Seq && batch[0].Seq > w.replayMax:
			// A LIVE batch HQ skipped whole as old: HQ's seqs for this item came
			// from a shifted file this (restarted) forwarder never saw. Move it
			// and everything after it above HQ's last_seq and send it again.
			delta := ack.LastSeq - batch[0].Seq + 1
			for i := range w.buf {
				w.buf[i].Seq += delta
			}
			w.shift += delta
			w.maxSeq += delta
			f.log.Printf("officers: %s: instance %s: HQ holds item %s's transcript past this position (a new conversation before muster restarted); re-sequenced above %d",
				w.o.ID, w.instance, item, ack.LastSeq)
		case err == nil:
			f.count(resultPosted, n)
			w.lastSeq = max(w.lastSeq, batch[n-1].Seq, ack.LastSeq)
			w.buf = w.buf[n:]
			w.failures, w.nextAt = 0, time.Time{}
		case errors.As(err, &hqErr) && hqErr.Status == http.StatusBadRequest:
			f.count(resultRefused, n)
			w.buf = w.buf[n:]
			f.log.Printf("officers: %s: instance %s: HQ refused a transcript batch of %d for item %s (400%s); dropped",
				w.o.ID, w.instance, n, item, hqCode(hqErr))
		case errors.As(err, &hqErr) && hqErr.Status != http.StatusTooManyRequests:
			// 409 not_claimed / officer_disabled, 404 not this officer's item,
			// 401/403: a decision, not an outage — stop for this item.
			f.log.Printf("officers: %s: instance %s: stopped forwarding item %s's transcript: HQ answered %d%s",
				w.o.ID, w.instance, item, hqErr.Status, hqCode(hqErr))
			f.stop(w, gen, item)
			return
		default:
			// An outage (5xx, 429, no answer): back off; retries are capped.
			w.failures++
			if w.failures > f.cfg.MaxRetries {
				f.count(resultRetries, n)
				f.log.Printf("officers: %s: instance %s: HQ did not take a transcript batch of %d for item %s after %d attempts; dropped (%v)",
					w.o.ID, w.instance, n, item, w.failures, err)
				w.buf = w.buf[n:]
				w.failures = 0
			}
			w.nextAt = now.Add(f.backoff(w.failures))
			return
		}
	}
}

func (f *Forwarder) backoff(failures int) time.Duration {
	d := f.cfg.Interval
	for i := 0; i < failures && d < f.cfg.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, f.cfg.MaxBackoff)
}

// stop ends w for item after an HQ refusal, unless the worker moved on.
func (f *Forwarder) stop(w *worker, gen int, item officers.ItemID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped[key{w.o.ID, w.instance, item}] = true
	if w.gen == gen && !w.dead {
		w.dead = true
		if f.workers[w.instance] == w {
			delete(f.workers, w.instance)
		}
	}
}

// hqCode is HQ's error code from a refusal body, as " code" or "". The body's
// other fields are not logged: only HQ's own vocabulary.
func hqCode(e *officers.HQError) string {
	var b struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(e.Body, &b) == nil && b.Code != "" && len(b.Code) <= 64 && !strings.ContainsAny(b.Code, " \n") {
		return " " + b.Code
	}
	return ""
}

// --- mapping -----------------------------------------------------------------

// maxToolArgs bounds a tool_use's argument summary when the call has no
// one-line target.
const maxToolArgs = 200

// mapEvent is THE mapping from a transcript event to the contract's kinds:
// the human's prompt → user; assistant text → assistant; a tool call →
// tool_use as "ToolName: <target>"; a tool result → tool_result; an API error
// or a harness notice → system. Injected context draws nothing and is not
// sent.
func mapEvent(ev cctranscript.Event) (kind, text string, ok bool) {
	switch ev.Kind {
	case cctranscript.KindPrompt:
		return officers.TranscriptUser, ev.Text, true
	case cctranscript.KindText:
		return officers.TranscriptAssistant, ev.Text, true
	case cctranscript.KindToolCall:
		return officers.TranscriptToolUse, toolSummary(ev), true
	case cctranscript.KindToolResult:
		if !ev.OK {
			return officers.TranscriptToolResult, "[error] " + ev.Output, true
		}
		return officers.TranscriptToolResult, ev.Output, true
	case cctranscript.KindError:
		return officers.TranscriptSystem, "API error: " + ev.Text, true
	case cctranscript.KindNotice:
		if ev.Status != "" {
			return officers.TranscriptSystem, ev.Text + " (" + ev.Status + ")", true
		}
		return officers.TranscriptSystem, ev.Text, true
	}
	return "", "", false
}

func toolSummary(ev cctranscript.Event) string {
	arg := strings.TrimSpace(ev.Target)
	if arg == "" {
		arg = cctranscript.Cap(strings.Join(strings.Fields(ev.Input), " "), maxToolArgs)
	}
	if arg == "" {
		return ev.Tool
	}
	return ev.Tool + ": " + arg
}

// clean is THE text pipeline, in this order: the officer's token replaced,
// the CI-log secret filter, the 8 KiB cut.
func (f *Forwarder) clean(o officers.Officer, s string) string {
	return Truncate(officership.RedactText(f.cfg.Poster.Scrub(o, s)), officers.MaxTranscriptText)
}

// Truncate cuts s to at most n bytes on a character boundary, ending it with
// "… [truncated N bytes]" (N the bytes removed) when it cuts.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// The marker for the most bytes that could be removed is the longest.
	keep := n - len(fmt.Sprintf(truncatedMarkerFmt, len(s)))
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + fmt.Sprintf(truncatedMarkerFmt, len(s)-keep)
}

// --- the seq -------------------------------------------------------------------

// The seq's layout: the line's end offset in its transcript file, then 15 bits
// of the event's index within that line. The end offset strictly increases
// from line to line along one file and the index within one, so seq does.
// maxSeq keeps it a JSON-safe integer (below 2^53): 2^53 / 2^15 = 2^38, so an
// end offset below 256 GiB (a transcript far past any session's).
//
// 🔴 15 BITS SO NO LINE'S EVENTS ARE EVER DROPPED. ccd never serves a line
// longer than its 1 MiB read window (cctranscript.ReadLines skips one), and an
// event is at least one content block of well over 32 bytes, so a served line
// carries fewer than 2^20 / 32 = 32768 events. (4 bits dropped every event
// past the 16th of a user record carrying many parallel tool results.)
const (
	idxBits = 15
	maxIdx  = 1<<idxBits - 1
	maxSeq  = 1 << 53
)

// Seq is the seq of the idx'th event of the line ending at end, or false when
// none exists (an index past 32767, an offset past 256 GiB).
func Seq(end int64, idx int) (int64, bool) {
	if end <= 0 || end >= maxSeq>>idxBits || idx < 0 || idx > maxIdx {
		return 0, false
	}
	return end<<idxBits | int64(idx), true
}
