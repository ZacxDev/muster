package api

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE ASKS PAGE'S FILTER CHIPS (operator decisions: "One-tap chips in the
// URL"; source = "The officer or agent"). A row under the search box
// (ui/asks_chips.go): a TYPE — Questions, Confirmations (step-up cards),
// Drafts, Notices — and a SOURCE — each officer, agent or session with items
// in the view, busiest first. One of each at a time, together or alone; a tap
// on the active chip clears it, Clear clears both. The state is the address:
// ?type=<type>&from=<source key>, beside ?q= and ?tab=, so a filter is a
// bookmark, Back undoes it, and it works without a script. It applies to the
// Open, Answered and Drafts tabs and to search results; the Log has its own.
//
// 🔴 ONE SOURCE RULE, ONE PLACE: askSources.sourceFor. What a chip is called,
// what it counts and what it keeps are all that function's answer — over an
// ask, a notice, an answered ask, a draft, a step-up card. "Marketing" is
// everything the marketing officer raised: its questions (the officer's ask,
// asker label "officer <id>"), its drafts (plugin_reply_drafts.officer) AND
// the step-up card that confirms one of its drafts, whoever filed the card (a
// session does: step_up.draft_id → the draft's officer). An agent is its
// agent row (by name: the session page badge's agent:<name> key,
// attention_asker.go); anything else is grouped by the name its card head
// shows (attentionTitle), session:<name>.
//
// 🔴 OWNER SCOPING: every list filtered here is already the reader's — the
// queue, the answers, the drafts and the search all read under the request's
// principal, the feed's own predicate — so a chip is only ever made from the
// reader's items and a filter only ever narrows them. The agent and window
// keys a session page links with resolve AS THE VIEWER (resolveAsker); a key
// that names nothing the viewer has is ignored, as before.
// ---------------------------------------------------------------------------

// The filter's query parameters. ?from= is attention_asker.go's
// askerFromParam (a session page's badge links with it).
const asksTypeParam = "type"

// The type keys (?type=).
const (
	askTypeQuestion = "question"
	askTypeConfirm  = "confirmation"
	askTypeDraft    = "draft"
	askTypeNotice   = "notice"
)

// askTypeChips is the type chips' order and words.
var askTypeChips = []struct{ key, label string }{
	{askTypeQuestion, "Questions"},
	{askTypeConfirm, "Confirmations"},
	{askTypeDraft, "Drafts"},
	{askTypeNotice, "Notices"},
}

// The source keys (?from=) beside attention_asker.go's agent:<name> and
// window:… (a session page's badge links with those). 🔴 EACH NAMES ONE
// THING, NEVER TWO THAT SHARE A NAME (round 1 of the audit, G1): an officer's
// id is unique only within its owner, and a reader sees a team's officers
// beside their own, so its key carries the owner; an agent is its row's id; a
// session its host and its name.
//
//	officer:<owner id>:<officer id>
//	agentid:<agent id>
//	session:<host>:<name>
const (
	askSourceOfficer = "officer:"
	askSourceAgentID = "agentid:"
	askSourceSession = "session:"
)

// asksChipsShown is how many source chips the row draws before More.
const asksChipsShown = 3

// askOrigin is an item's source: its key (?from=), its name, and — for an
// agent — the agent's id.
type askOrigin struct {
	key, label string
	agentID    int64
}

// draftSource is who raised a draft: its owner, its officer ("" for none), its
// drafting agent and that agent's name as the draft's card shows it.
type draftSource struct {
	owner, agentID int64
	officer, agent string
}

// draftCardSource is card's draftSource.
func draftCardSource(c plugins.DraftCard) draftSource {
	d := draftSource{owner: c.Draft.OwnerID, agent: firstNonBlank(c.AgentDisplay, c.Draft.AgentName)}
	if c.Draft.Officer != nil {
		d.officer = *c.Draft.Officer
	}
	if c.Draft.AgentID != nil {
		d.agentID = *c.Draft.AgentID
	}
	return d
}

// askSources is what one render knows to name sources: the drafts it can see
// by id (the Drafts tab's, the search's, and each step-up card's) and the
// officers' display names (officerDisplayName). It reads no agent: an agent's
// name is the one its card already shows.
type askSources struct {
	drafts      map[int64]draftSource
	officerName func(owner int64, id string) string
}

// sourceFor is THE SOURCE RULE: the officer when there is one, else the agent,
// else the session as its card head names it, on host. The zero askOrigin for
// none of them.
func (x askSources) sourceFor(owner int64, officer string, agentID int64, agentName, host, session string) askOrigin {
	if officer = strings.TrimSpace(officer); officer != "" {
		label := ""
		if x.officerName != nil {
			label = strings.TrimSpace(x.officerName(owner, officer))
		}
		if label == "" {
			label = ui.OfficerPlainName("", officer)
		}
		return askOrigin{key: askSourceOfficer + strconv.FormatInt(owner, 10) + ":" + officer, label: label}
	}
	if agentID > 0 {
		return askOrigin{key: askSourceAgentID + strconv.FormatInt(agentID, 10), label: firstNonBlank(strings.TrimSpace(agentName), "Agent"), agentID: agentID}
	}
	if session = strings.TrimSpace(session); session != "" {
		return askOrigin{key: askSourceSession + host + ":" + session, label: session}
	}
	return askOrigin{}
}

// agentAskOf is THE AGENT PREDICATE, the one a session page's asks badge
// counts by (askerOf.has) and an agent's chip keeps by: the agent whose OWN
// ask item it is (an ask an agent raised), 0 for any other item — a notice,
// a waiting session, a step-up card (muster raises those). So the badge's
// number is the number of cards its tap lists.
func agentAskOf(it attentionItem) int64 {
	if it.Kind != attentionKindAsk || it.stepUp != nil {
		return 0
	}
	return it.card.agentID
}

// ofItem is an Open or Answered item's source: a draft's (a draft item, a
// step-up card on a draft the reader can read), else the item's officer, the
// agent whose ask it is, or the session its head names.
func (x askSources) ofItem(it attentionItem) askOrigin {
	if id := draftItemID(it); id > 0 {
		if d, ok := x.drafts[id]; ok {
			return x.ofDraft(d)
		}
	}
	// A step-up card is its draft's OFFICER'S (it confirms the officer's
	// post, whoever filed it); a card on an agent's reply draft is muster's
	// own, not one of the agent's asks (agentAskOf), and is named by its head.
	if it.stepUp != nil && it.stepUp.DraftID > 0 {
		if d, ok := x.drafts[it.stepUp.DraftID]; ok && d.officer != "" {
			return x.ofDraft(d)
		}
	}
	// A session by the name its head shows, without the notes a head adds
	// about where it is now (it is the same session either way).
	who, _, _ := attentionTitle(it)
	who = strings.TrimSuffix(strings.TrimSuffix(who, askNotShown), whereHostOffline)
	if it.card.agentID > 0 && agentAskOf(it) == 0 {
		// An agent's notice, its waiting window: not one of its asks (the
		// agent predicate), so no chip of its own.
		return askOrigin{}
	}
	return x.sourceFor(it.owner, it.OfficerID, agentAskOf(it), who, firstNonBlank(it.card.ref.Host, attentionPlaceHost(it)), who)
}

// ofDraft is a draft's source: its officer, else its drafting agent.
func (x askSources) ofDraft(d draftSource) askOrigin {
	return x.sourceFor(d.owner, d.officer, d.agentID, d.agent, "", d.agent)
}

// draftItemID is the draft a Drafts-tab item (draftKey) is, 0 for any other.
func draftItemID(it attentionItem) int64 {
	if it.Kind != attentionKindPlugin {
		return 0
	}
	rest, ok := strings.CutPrefix(it.Key, "draft-")
	if !ok {
		return 0
	}
	id, _ := strconv.ParseInt(rest, 10, 64)
	return id
}

// itemType is an item's type chip key, "" for an item no type chip names (a
// waiting session, a task to route, a pairing request).
func itemType(it attentionItem) string {
	switch {
	case draftsTabItem(it):
		return askTypeDraft
	case it.stepUp != nil:
		return askTypeConfirm
	case it.Kind == attentionKindAsk || it.Kind == attentionKindQuestion:
		return askTypeQuestion
	case it.Kind == attentionKindNotice:
		return askTypeNotice
	}
	return ""
}

// asksFilter is the filter a request names: its type, its source key, and
// how that key resolved — an agent's id, a window (a session page's badge,
// legacy: only its asks, attention_asker.go), the source's name — and the
// reader's windows, read only for a window key.
type asksFilter struct {
	typ, from string
	agentID   int64
	window    *askerOf
	label     string
	hosts     func() []mirror.HostView
}

// on reports whether any filter is set.
func (f asksFilter) on() bool { return f.typ != "" || f.from != "" }

// keepItem reports whether f keeps item it.
func (f asksFilter) keepItem(x askSources, it attentionItem) bool {
	if f.typ != "" && itemType(it) != f.typ {
		return false
	}
	switch {
	case f.from == "":
		return true
	case f.window != nil:
		hosts := f.hosts
		if hosts == nil {
			hosts = func() []mirror.HostView { return nil }
		}
		return f.window.has(it, hosts)
	}
	return x.ofItem(it).key == f.from
}

// keepDraft reports whether f keeps the draft d is the source of.
func (f asksFilter) keepDraft(x askSources, d draftSource) bool {
	if f.typ != "" && f.typ != askTypeDraft {
		return false
	}
	switch {
	case f.from == "":
		return true
	case f.window != nil:
		return false
	}
	return x.ofDraft(d).key == f.from
}

// items is items f keeps.
func (f asksFilter) items(x askSources, items []attentionItem) []attentionItem {
	if !f.on() {
		return items
	}
	out := items[:0:0]
	for _, it := range items {
		if f.keepItem(x, it) {
			out = append(out, it)
		}
	}
	return out
}

// draftCards is cards f keeps.
func (f asksFilter) draftCards(x askSources, cards []plugins.DraftCard) []plugins.DraftCard {
	if !f.on() {
		return cards
	}
	out := cards[:0:0]
	for _, c := range cards {
		if f.keepDraft(x, draftCardSource(c)) {
			out = append(out, c)
		}
	}
	return out
}

// query is f as the query its live lists re-read with ("" for none).
func (f asksFilter) query() string {
	v := url.Values{}
	f.into(v)
	return v.Encode()
}

// into sets f's parameters on v.
func (f asksFilter) into(v url.Values) {
	if f.typ != "" {
		v.Set(asksTypeParam, f.typ)
	}
	if f.from != "" {
		v.Set(askerFromParam, f.from)
	}
}

// requestAsksFilter is THE filter of request r, for the page and for every
// live endpoint alike (round 1, Y1: the live ones parsed with no windows, so a
// window key resolved to nothing and was silently dropped). hosts is the
// reader's windows when the caller has read them; nil, and they are read here
// — once, and only for a window key.
func (s *Server) requestAsksFilter(r *http.Request, hosts func() []mirror.HostView) asksFilter {
	if hosts == nil {
		var hs []mirror.HostView
		read := false
		hosts = func() []mirror.HostView {
			if !read && s.ext.Mirror != nil {
				hs, read = s.ext.Mirror.Hosts(r.Context()), true
			}
			return hs
		}
	}
	return s.parseAsksFilter(r.Context(), r.URL.Query(), hosts)
}

// parseAsksFilter is the filter q names, as ctx's principal: a known type, and
// a source key — an agent's or a window's resolved as the viewer (an
// unresolvable one is ignored: resolveAsker), an officer's or a session's
// taken as written (it only ever narrows the reader's own items).
func (s *Server) parseAsksFilter(ctx context.Context, q url.Values, hosts func() []mirror.HostView) asksFilter {
	f := asksFilter{hosts: hosts}
	switch t := q.Get(asksTypeParam); t {
	case askTypeQuestion, askTypeConfirm, askTypeDraft, askTypeNotice:
		f.typ = t
	}
	from := strings.TrimSpace(q.Get(askerFromParam))
	switch {
	case from == "":
	case strings.HasPrefix(from, askerKeyAgent), strings.HasPrefix(from, askerKeyWindow):
		ref, ok := parseAskerKey(from)
		if !ok {
			break
		}
		k, ok := s.resolveAsker(ctx, ref, hosts)
		if !ok {
			break
		}
		if ref.agent != "" {
			// A session page's agent badge: the agent's chip (its id key).
			f.from, f.label, f.agentID = askSourceAgentID+strconv.FormatInt(k.agentID, 10), k.label, k.agentID
		} else {
			f.from, f.label, f.window = ref.key(), k.label, &k
		}
	case strings.HasPrefix(from, askSourceOfficer):
		owner, id, ok := strings.Cut(strings.TrimPrefix(from, askSourceOfficer), ":")
		if n, err := strconv.ParseInt(owner, 10, 64); ok && err == nil && n > 0 && id != "" {
			f.from, f.label = from, ui.OfficerPlainName("", id)
		}
	case strings.HasPrefix(from, askSourceAgentID):
		if n, err := strconv.ParseInt(strings.TrimPrefix(from, askSourceAgentID), 10, 64); err == nil && n > 0 {
			f.from, f.label, f.agentID = from, "Agent", n
		}
	case strings.HasPrefix(from, askSourceSession):
		if _, name, ok := strings.Cut(strings.TrimPrefix(from, askSourceSession), ":"); ok && name != "" {
			f.from, f.label = from, name
		}
	}
	return f
}

// readAskSources is the render's askSources: the drafts given, the drafts the
// items' step-up cards already carry (withAskBoxes read them for their
// decision controls: stepUpDraftsFor), and — in ONE batch read that does not
// sweep (plugins.DraftCardsByID; round 1, Y2: a DraftCard per draft was a
// sweep, a write transaction, per draft per render) — the rest the items name
// (a step-up card's draft the reader may not decide, a draft item's).
// Row-level security: another person's draft is absent, and its card is then
// named by its own head.
func (s *Server) readAskSources(ctx context.Context, drafts []plugins.DraftCard, items ...[]attentionItem) askSources {
	x := askSources{drafts: map[int64]draftSource{}, officerName: s.officerDisplayName}
	for _, c := range drafts {
		x.drafts[c.Draft.ID] = draftCardSource(c)
	}
	for _, list := range items {
		for _, it := range list {
			if it.stepUpDraftSrc != nil && it.stepUp != nil {
				x.drafts[it.stepUp.DraftID] = *it.stepUpDraftSrc
			}
		}
	}
	if s.ext.Plugins == nil {
		return x
	}
	var missing []int64
	seen := map[int64]bool{}
	for _, list := range items {
		for _, it := range list {
			id := draftItemID(it)
			if it.stepUp != nil && it.stepUp.DraftID > 0 {
				id = it.stepUp.DraftID
				if it.stepUpDraftRead {
					// Read already (withAskBoxes); absent = not the reader's.
					continue
				}
			}
			if _, ok := x.drafts[id]; id > 0 && !ok && !seen[id] && len(missing) < asksFilterDraftReads {
				seen[id] = true
				missing = append(missing, id)
			}
		}
	}
	if len(missing) == 0 {
		return x
	}
	got, err := s.ext.Plugins.DraftCardsByID(ctx, missing)
	if err != nil {
		s.logger.Printf("asks: the filter's drafts: %v", err)
	}
	for _, c := range got {
		x.drafts[c.Draft.ID] = draftCardSource(c)
	}
	return x
}

// asksFilterDraftReads bounds the drafts one render reads for step-up cards.
const asksFilterDraftReads = 50

// asksChipCounts counts the view's items by type and by source — each
// dimension over what the OTHER one keeps (f's source for the type chips, its
// type for the source chips), so under Marketing the Questions chip counts
// Marketing's questions: what a tap on it would list.
type asksChipCounts struct {
	f       asksFilter
	types   map[string]int
	sources map[string]int
	labels  map[string]string
	order   []string
}

func newAsksChipCounts(f asksFilter) *asksChipCounts {
	return &asksChipCounts{f: f, types: map[string]int{}, sources: map[string]int{}, labels: map[string]string{}}
}

// bySource is c's filter without its type; byType without its source.
func (c *asksChipCounts) bySource() asksFilter { f := c.f; f.typ = ""; return f }
func (c *asksChipCounts) byType() asksFilter   { return asksFilter{typ: c.f.typ} }

func (c *asksChipCounts) add(typ string, src askOrigin, typeOK, sourceOK bool) {
	if typ != "" && typeOK {
		c.types[typ]++
	}
	if src.key == "" || !sourceOK {
		return
	}
	if _, ok := c.sources[src.key]; !ok {
		c.order = append(c.order, src.key)
		c.labels[src.key] = src.label
	}
	c.sources[src.key]++
}

// addItems counts items (the Open and Answered tabs', search results').
func (c *asksChipCounts) addItems(x askSources, items []attentionItem) {
	bs, bt := c.bySource(), c.byType()
	for _, it := range items {
		c.add(itemType(it), x.ofItem(it), bs.keepItem(x, it), bt.keepItem(x, it))
	}
}

// addDrafts counts drafts.
func (c *asksChipCounts) addDrafts(x askSources, cards []plugins.DraftCard) {
	bs, bt := c.bySource(), c.byType()
	for _, d := range cards {
		src := draftCardSource(d)
		c.add(askTypeDraft, x.ofDraft(src), bs.keepDraft(x, src), bt.keepDraft(x, src))
	}
}

// asksChipHref is the page's address with type and from set (each "" drops
// it), keeping q and tab.
func asksChipHref(q url.Values, typ, from string) string {
	v := url.Values{}
	for _, k := range []string{asksSearchParam, asksTabParam} {
		if s := strings.TrimSpace(q.Get(k)); s != "" {
			v.Set(k, s)
		}
	}
	if typ != "" {
		v.Set(asksTypeParam, typ)
	}
	if from != "" {
		v.Set(askerFromParam, from)
	}
	if len(v) == 0 {
		return attentionPath
	}
	return attentionPath + "?" + v.Encode()
}

// asksChipsView is the chip row for filter f over the view's counts c (q the
// request's query, for each link's q and tab). The sources are busiest first
// (ties: by name); asksChipsShown of them inline, the rest under More — the
// active one always inline, named even when the view holds none of its items.
func asksChipsView(f asksFilter, c *asksChipCounts, q url.Values) ui.AsksChipsView {
	var v ui.AsksChipsView
	total := 0
	for _, n := range c.sources {
		total += n
	}
	for _, n := range c.types {
		total += n
	}
	if total == 0 && !f.on() {
		return v
	}
	for _, t := range askTypeChips {
		to := t.key
		if f.typ == t.key {
			to = ""
		}
		v.Types = append(v.Types, ui.AsksChip{Label: t.label, Href: asksChipHref(q, to, f.from), Active: f.typ == t.key, Count: c.types[t.key], Key: t.key})
	}
	keys := append([]string(nil), c.order...)
	sort.SliceStable(keys, func(i, j int) bool {
		if c.sources[keys[i]] != c.sources[keys[j]] {
			return c.sources[keys[i]] > c.sources[keys[j]]
		}
		return strings.ToLower(c.labels[keys[i]]) < strings.ToLower(c.labels[keys[j]])
	})
	chip := func(k, label string) ui.AsksChip {
		to := k
		if f.from == k {
			to = ""
		}
		return ui.AsksChip{Label: label, Href: asksChipHref(q, f.typ, to), Active: f.from == k, Count: c.sources[k], Key: k}
	}
	if f.from != "" {
		label := firstNonBlank(c.labels[f.from], f.label)
		v.Sources = append(v.Sources, chip(f.from, label))
	}
	for _, k := range keys {
		if k == f.from {
			continue
		}
		if len(v.Sources) < asksChipsShown {
			v.Sources = append(v.Sources, chip(k, c.labels[k]))
		} else {
			v.More = append(v.More, chip(k, c.labels[k]))
		}
	}
	if f.on() {
		v.Clear = asksChipHref(q, "", "")
	}
	return v
}

// asksFilterEmptyLine is a filtered list's empty line.
const asksFilterEmptyLine = "Nothing here matches this filter."

// asksFilterEmptyHTML is a filtered feed's empty state (in place of All
// clear, which would say nothing waits anywhere).
func asksFilterEmptyHTML(attrs string) string {
	return ui.AsksEmptyHTML(` data-asks-filter-empty`+attrs, "", "", asksFilterEmptyLine, "")
}

// answeredFiltered is the Answered tab's feed over asks, f applied (items[i]
// is asks[i] as an item).
func answeredFiltered(asks []attention.Ask, items []attentionItem, f asksFilter, x askSources, now time.Time, proposals map[int64]bool) string {
	if !f.on() {
		return answeredFeedHTML(asks, items, now, proposals)
	}
	var ka []attention.Ask
	var ki []attentionItem
	for i, a := range asks {
		if f.keepItem(x, items[i]) {
			ka, ki = append(ka, a), append(ki, items[i])
		}
	}
	if len(ka) == 0 {
		return asksFilterEmptyHTML(` data-asks-empty="answered"`)
	}
	return answeredFeedHTML(ka, ki, now, proposals)
}

// searchFiltered is res with f applied (its asks as items to name their
// sources).
func searchFiltered(res asksSearchResult, f asksFilter, x askSources) asksSearchResult {
	if !f.on() {
		return res
	}
	out := res
	out.asks = nil
	for _, a := range res.asks {
		if f.keepItem(x, searchItem(a)) {
			out.asks = append(out.asks, a)
		}
	}
	out.drafts = f.draftCards(x, res.drafts)
	return out
}

// searchItem is a search result's ask as an item (askItem, no windows: the
// result names its asker by what it stored, askerLabel), its kind a notice's
// for a notice.
func searchItem(a attention.Ask) attentionItem {
	it := askItem(a)
	if a.Kind == attention.KindNotice {
		it.Kind = attentionKindNotice
	}
	return it
}

// filteredSearch is res with f applied, its sources read as ctx's principal;
// res itself when f is off.
func (s *Server) filteredSearch(ctx context.Context, res asksSearchResult, f asksFilter) asksSearchResult {
	if !f.on() {
		return res
	}
	var items []attentionItem
	for _, a := range res.asks {
		items = append(items, searchItem(a))
	}
	return searchFiltered(res, f, s.readAskSources(ctx, res.drafts, items))
}

// asksSearchKeep is the filter as the search form's hidden fields, so a
// submitted or typed search keeps it.
func asksSearchKeep(f asksFilter) []ui.AsksKeep {
	var keep []ui.AsksKeep
	if f.typ != "" {
		keep = append(keep, ui.AsksKeep{Name: asksTypeParam, Value: f.typ})
	}
	if f.from != "" {
		keep = append(keep, ui.AsksKeep{Name: askerFromParam, Value: f.from})
	}
	return keep
}
