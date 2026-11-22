package api

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
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

// The source keys' prefixes beside attention_asker.go's agent: and window:.
const (
	askSourceOfficer = "officer:"
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

// askSources is what one render reads to name sources: the reader's agents
// by id, the drafts it can see by id (the Drafts tab's, and each step-up
// card's), and the officer's display name (officerDisplayName).
type askSources struct {
	agents      map[int64]agents.Agent
	drafts      map[int64]plugins.Draft
	officerName func(owner int64, id string) string
}

// sourceFor is THE SOURCE RULE: the officer when there is one, else the agent
// (a row the reader can read), else the session as the card head names it.
// The zero askOrigin for none of them.
func (x askSources) sourceFor(owner int64, officer string, agentID int64, session string) askOrigin {
	if officer = strings.TrimSpace(officer); officer != "" {
		label := ""
		if x.officerName != nil {
			label = strings.TrimSpace(x.officerName(owner, officer))
		}
		if label == "" {
			label = ui.OfficerPlainName("", officer)
		}
		return askOrigin{key: askSourceOfficer + officer, label: label}
	}
	if a, ok := x.agents[agentID]; ok && agentID > 0 && a.Name != "" {
		return askOrigin{key: askerKeyAgent + a.Name, label: firstNonBlank(a.DisplayName, a.Name), agentID: agentID}
	}
	if session = strings.TrimSpace(session); session != "" {
		return askOrigin{key: askSourceSession + session, label: session}
	}
	return askOrigin{}
}

// ofItem is an Open or Answered item's source: a draft's (a draft item, a
// step-up card on a draft the reader can read), else the item's officer,
// agent, or the name its head shows.
func (x askSources) ofItem(it attentionItem) askOrigin {
	if id := draftItemID(it); id > 0 {
		if d, ok := x.drafts[id]; ok {
			return x.ofDraft(d)
		}
	}
	if it.stepUp != nil && it.stepUp.DraftID > 0 {
		if d, ok := x.drafts[it.stepUp.DraftID]; ok {
			return x.ofDraft(d)
		}
	}
	// A session by the name its head shows, without the notes a head adds
	// about where it is now (it is the same session either way).
	who, _, _ := attentionTitle(it)
	who = strings.TrimSuffix(strings.TrimSuffix(who, askNotShown), whereHostOffline)
	return x.sourceFor(it.owner, it.OfficerID, it.card.agentID, who)
}

// ofDraft is a draft's source: its officer, else its drafting agent, else
// the agent name it stored.
func (x askSources) ofDraft(d plugins.Draft) askOrigin {
	officer := ""
	if d.Officer != nil {
		officer = *d.Officer
	}
	var agent int64
	if d.AgentID != nil {
		agent = *d.AgentID
	}
	return x.sourceFor(d.OwnerID, officer, agent, d.AgentName)
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
// legacy: only its asks, attention_asker.go), and the source's name.
type asksFilter struct {
	typ, from string
	agentID   int64
	window    *askerOf
	label     string
}

// on reports whether any filter is set.
func (f asksFilter) on() bool { return f.typ != "" || f.from != "" }

// keepItem reports whether f keeps item it (hosts: the render's windows, read
// for a window key).
func (f asksFilter) keepItem(x askSources, it attentionItem, hosts func() []mirror.HostView) bool {
	if f.typ != "" && itemType(it) != f.typ {
		return false
	}
	switch {
	case f.from == "":
		return true
	case f.window != nil:
		return f.window.has(it, hosts)
	case f.agentID > 0:
		return x.ofItem(it).agentID == f.agentID
	}
	return x.ofItem(it).key == f.from
}

// keepDraft reports whether f keeps draft d.
func (f asksFilter) keepDraft(x askSources, d plugins.Draft) bool {
	if f.typ != "" && f.typ != askTypeDraft {
		return false
	}
	switch {
	case f.from == "":
		return true
	case f.window != nil:
		return false
	case f.agentID > 0:
		return x.ofDraft(d).agentID == f.agentID
	}
	return x.ofDraft(d).key == f.from
}

// items is items f keeps.
func (f asksFilter) items(x askSources, items []attentionItem, hosts []mirror.HostView) []attentionItem {
	if !f.on() {
		return items
	}
	hs := func() []mirror.HostView { return hosts }
	out := items[:0:0]
	for _, it := range items {
		if f.keepItem(x, it, hs) {
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
		if f.keepDraft(x, c.Draft) {
			out = append(out, c)
		}
	}
	return out
}

// query is f as the query its live lists re-read with ("" for none).
func (f asksFilter) query() string {
	v := url.Values{}
	if f.typ != "" {
		v.Set(asksTypeParam, f.typ)
	}
	if f.from != "" {
		v.Set(askerFromParam, f.from)
	}
	return v.Encode()
}

// parseAsksFilter is the filter q names, as ctx's principal: a known type, and
// a source key — an agent's or a window's resolved as the viewer (an
// unresolvable one is ignored: resolveAsker), an officer's or a session's
// taken as written (it only ever narrows the reader's own items).
func (s *Server) parseAsksFilter(ctx context.Context, q url.Values, hosts []mirror.HostView) asksFilter {
	var f asksFilter
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
		k, ok := s.resolveAsker(ctx, ref, func() []mirror.HostView { return hosts })
		if !ok {
			break
		}
		f.from, f.label = ref.key(), k.label
		if ref.agent != "" {
			f.agentID = k.agentID
		} else {
			f.window = &k
		}
	case strings.HasPrefix(from, askSourceOfficer) && len(from) > len(askSourceOfficer):
		f.from, f.label = from, ui.OfficerPlainName("", strings.TrimPrefix(from, askSourceOfficer))
	case strings.HasPrefix(from, askSourceSession) && len(from) > len(askSourceSession):
		f.from, f.label = from, strings.TrimPrefix(from, askSourceSession)
	}
	return f
}

// readAskSources is the render's askSources: the drafts given and each
// step-up card's in items that is not among them, then the agents all of
// them name — every read as the reader (row-level security: another
// person's draft or agent is not found, and names nothing) — and officers'
// names.
func (s *Server) readAskSources(ctx context.Context, drafts []plugins.DraftCard, items ...[]attentionItem) askSources {
	x := askSources{agents: map[int64]agents.Agent{}, drafts: map[int64]plugins.Draft{}, officerName: s.officerDisplayName}
	for _, c := range drafts {
		x.drafts[c.Draft.ID] = c.Draft
	}
	if s.ext.Plugins != nil {
		missing := map[int64]bool{}
		for _, list := range items {
			for _, it := range list {
				id := draftItemID(it)
				if it.stepUp != nil && it.stepUp.DraftID > 0 {
					id = it.stepUp.DraftID
				}
				if _, ok := x.drafts[id]; id > 0 && !ok && len(missing) < asksFilterDraftReads {
					missing[id] = true
				}
			}
		}
		for id := range missing {
			if c, err := s.ext.Plugins.DraftCard(ctx, id); err == nil {
				x.drafts[id] = c.Draft
			}
		}
	}
	if s.ext.Agents == nil {
		return x
	}
	ids := map[int64]bool{}
	for _, d := range x.drafts {
		if d.AgentID != nil && *d.AgentID > 0 && d.Officer == nil {
			ids[*d.AgentID] = true
		}
	}
	for _, list := range items {
		for _, it := range list {
			if it.card.agentID > 0 {
				ids[it.card.agentID] = true
			}
		}
	}
	for id := range ids {
		if a, err := s.ext.Agents.Get(ctx, id); err == nil {
			x.agents[id] = a
		}
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
	hosts   func() []mirror.HostView
	types   map[string]int
	sources map[string]int
	labels  map[string]string
	order   []string
}

func newAsksChipCounts(f asksFilter, hosts []mirror.HostView) *asksChipCounts {
	return &asksChipCounts{f: f, hosts: func() []mirror.HostView { return hosts },
		types: map[string]int{}, sources: map[string]int{}, labels: map[string]string{}}
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
		c.add(itemType(it), x.ofItem(it), bs.keepItem(x, it, c.hosts), bt.keepItem(x, it, c.hosts))
	}
}

// addDrafts counts drafts.
func (c *asksChipCounts) addDrafts(x askSources, cards []plugins.DraftCard) {
	bs, bt := c.bySource(), c.byType()
	for _, d := range cards {
		c.add(askTypeDraft, x.ofDraft(d.Draft), bs.keepDraft(x, d.Draft), bt.keepDraft(x, d.Draft))
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
func answeredFiltered(asks []attention.Ask, items []attentionItem, f asksFilter, x askSources, hosts []mirror.HostView, now time.Time, proposals map[int64]bool) string {
	if !f.on() {
		return answeredFeedHTML(asks, items, now, proposals)
	}
	hs := func() []mirror.HostView { return hosts }
	var ka []attention.Ask
	var ki []attentionItem
	for i, a := range asks {
		if f.keepItem(x, items[i], hs) {
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
		if f.keepItem(x, searchItem(a), func() []mirror.HostView { return nil }) {
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

// draftsForFilter is the reader's drafts as the Drafts tab reads them (both
// views), for naming the sources of a live list's items; nil without plugins
// or on a read error (logged by readDraftViews).
func (s *Server) draftsForFilter(ctx context.Context) []plugins.DraftCard {
	if s.ext.Plugins == nil {
		return nil
	}
	pending, archived, _ := s.readDraftViews(ctx, 0)
	return append(pending, archived...)
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
