package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/textplain"
)

// ---------------------------------------------------------------------------
// THE ATTENTION SUMMARY: GET /api/attention/summary (docs/attention.md#the-summary-api).
//
// What a status bar polls: everything waiting on the token's owner, each
// host's online state and its Claude windows' busy/idle counts, and whether the
// owner's chief nudges are off, shadow or live. One JSON document, no stream
// (no route a token can open streams: api_tokens.go).
//
// 🔴 "WAITING ON ME" IS attentionItems, AND NOTHING ELSE. The summary adds no
// predicate of its own: its items are exactly what the Asks page, the badge
// and attention_list read, so the bar and the page cannot disagree about what
// waits. Busy and idle are context only; they never make anything "waiting".
//
// 🔴 EACH ITEM SAYS WHAT ITS PUSH SAYS, AND NO MORE. title is the item's
// source as the title rule makes it (textplain.Title, the sink's own rule for
// a notification title); why is the first line of attentionPushBody — secrets
// redacted, a waiting session as summary only, a reply draft as who and where
// only. Claude's last words (Detail), the asker's options and the suggested
// replies are never in it. That is the disclosure the operator already chose
// for the lock screen.
//
// ⚠ NO PER-ITEM LOOKUPS. An item's link is the tap route (attentionOpenURL),
// which resolves the target when it is opened, so building the summary costs
// what one badge render costs: no ByAsk, no agent read, no focus read.
// ---------------------------------------------------------------------------

// attentionSummaryPath is the summary's route (requireAPIToken).
const attentionSummaryPath = "/api/attention/summary"

// The values of attentionSummary.NudgeMode.
const (
	nudgeModeOff = "off"
)

// attentionSummary is the summary document. Its field names are a contract
// with pollers (docs/attention.md): add fields, never rename one.
type attentionSummary struct {
	// TS is the server's clock when the summary was built, Unix seconds.
	TS int64 `json:"ts"`
	// NudgeMode is "off" (chief nudges do not run here), "shadow" or "live"
	// for the token's owner.
	NudgeMode string `json:"nudge_mode"`
	// Partial: a source of the queue could not be read, so items may be
	// missing (attentionItems' error). A poller must not read a partial
	// summary as a quiet one.
	Partial bool                   `json:"partial"`
	Hosts   []attentionSummaryHost `json:"hosts"`
	Items   []attentionSummaryItem `json:"items"`
}

// attentionSummaryHost is one of the owner's mirror hosts, by the name it
// reports (MUSTER_MIRROR_HOST).
type attentionSummaryHost struct {
	Host   string `json:"host"`
	Online bool   `json:"online"`
	// StatusReport: the host reports its windows' Claude status, so a zero
	// count (and no waiting session) from it is a measurement.
	StatusReport bool `json:"status_report"`
	Busy         int  `json:"busy"`
	Idle         int  `json:"idle"`
}

// attentionSummaryItem is one item waiting on the owner.
type attentionSummaryItem struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	// Host is the mirror host the item is on, "" when none (an agent's ask,
	// muster's own, a reply draft, a task).
	Host  string `json:"host"`
	Title string `json:"title"`
	Why   string `json:"why"`
	// Since is when it started waiting, Unix seconds (0 unknown); AgeSecs is
	// TS minus Since (0 unknown).
	Since   int64 `json:"since"`
	AgeSecs int64 `json:"age_secs"`
	Offline bool  `json:"offline"`
	// URL is where the owner acts on it, absolute on MUSTER_PUBLIC_URL; ""
	// when that is not set (the summary names no origin it was not told).
	URL string `json:"url"`
}

func (s *Server) handleAttentionSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := requestOwner(ctx)
	if owner <= 0 {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "the attention summary is a user's own; this credential names none"})
		return
	}
	items, err := s.attentionItems(ctx)
	if err != nil {
		s.logger.Printf("attention: summary: %v", err)
	}
	var hosts []mirror.HostView
	if s.ext.Mirror != nil {
		hosts = s.ext.Mirror.Hosts(ctx)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, s.attentionSummaryOf(owner, items, err != nil, hosts, s.now()))
}

// attentionSummaryOf builds owner's summary from its queue and its hosts.
func (s *Server) attentionSummaryOf(owner int64, items []attentionItem, partial bool, hosts []mirror.HostView, now time.Time) attentionSummary {
	out := attentionSummary{TS: now.Unix(), NudgeMode: nudgeModeOff, Partial: partial,
		Hosts: []attentionSummaryHost{}, Items: []attentionSummaryItem{}}
	if s.chiefNudgesOn() {
		out.NudgeMode = s.chiefNudgeMode(owner)
	}
	for _, h := range hosts {
		out.Hosts = append(out.Hosts, attentionSummaryHostOf(h))
	}
	base := strings.TrimRight(s.auth.PublicURL, "/")
	for _, it := range items {
		out.Items = append(out.Items, attentionSummaryItemOf(it, base, now))
	}
	return out
}

// attentionSummaryHostOf is h's line: its Claude windows (and other agent
// runtimes', Window.Spec) counted by status.
//
// ⚠ AN OFFLINE HOST COUNTS NOTHING: a count is a claim about NOW, and a host
// that is gone cannot vouch for it (the rule HostView.Work follows).
func attentionSummaryHostOf(h mirror.HostView) attentionSummaryHost {
	out := attentionSummaryHost{Host: h.Host, Online: h.Online, StatusReport: h.StatusReport}
	if !h.Online {
		return out
	}
	for _, w := range h.Windows {
		if _, agent := w.Spec(); !w.Claude && !agent {
			continue
		}
		switch w.Status {
		case mirror.StatusBusy:
			out.Busy++
		case mirror.StatusIdle:
			out.Idle++
		}
	}
	return out
}

// attentionSummaryItemOf is it as the summary lists it. base is the public
// origin without a trailing slash, "" for none.
func attentionSummaryItemOf(it attentionItem, base string, now time.Time) attentionSummaryItem {
	out := attentionSummaryItem{Key: it.Key, Kind: it.Kind, Host: it.host, Title: textplain.Title(it.Source),
		Why: firstLine(attentionPushBody(it)), Offline: it.Offline}
	if out.Host == "" {
		out.Host = it.card.ref.Host // an ask from a mirrored session
	}
	path := it.Link
	if attentionOpenKey.MatchString(it.Key) {
		path = attentionOpenURL(it.Key)
	}
	if !it.Since.IsZero() {
		out.Since = it.Since.Unix()
		if age := now.Unix() - out.Since; age > 0 {
			out.AgeSecs = age
		}
	}
	if base != "" && path != "" {
		out.URL = base + path
	}
	return out
}
