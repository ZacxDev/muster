package api

import (
	"cmp"
	"context"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE COMMAND PALETTE'S SEARCH OF EVERYTHING ELSE (operator ask: "switch into
// asks. So we can just type like an ask ID and then it'll show up in a result,
// as well as pages … I could type IDs or titles for either of them"). The
// palette finds sessions and the app's screens on the page; asks and Pages
// documents are found here, once typing pauses (ui.PaletteFindRoute).
//
//   - AN ID ("1001", "#1001", "ask 1001", "page 7") is looked up by id ONLY,
//     never as text (asks_search.go's rule: the number in some question must
//     not get between the owner and the ask an agent named) — and is let
//     through whatever its length, below the text floor. A bare number names
//     an ask and a page; "ask N" an ask only, "page N" / "doc N" a page only.
//   - ANYTHING ELSE is text, from ui.PaletteServerMinRunes up to
//     paletteFindMaxRunes runes: an ask's question and the rest of what the
//     Asks page's search matches (attention.Search), a page's title.
//
// 🔴 WHAT EACH REACHES. An ask is the caller's OWN, never a team's and never
// one a session put to the chief: attention.Search names the owner in the
// query, and each answer passes askHasPage — the ask page's own rule — so
// every result links to a page that opens. A page is one the caller's Pages
// screen lists (pages.Store.Pages: row-level security, a team's page its
// member reads included), and its link is that screen's (/pages/{id}).
//
// RANKED: an ask named by its id first; then open asks above answered,
// dismissed or expired ones; then those whose QUESTION holds the text above
// those that match elsewhere (an answer, an option, who asked); then newest.
// Each kind is cut to paletteFindShown.
// ---------------------------------------------------------------------------

// paletteFindShown is how many asks, and how many pages, one search answers.
const paletteFindShown = 5

// paletteFindRead is how many asks a text search reads before it ranks them:
// the open ones must not be cut by newer answered ones before the rank.
const paletteFindRead = 50

// paletteFindMaxRunes is the longest text searched (the stores' own bound).
const paletteFindMaxRunes = attention.MaxSearchTextRunes

// paletteQueryID is an id-shaped query: an optional kind, an optional "#",
// the number, spaces allowed between.
var paletteQueryID = regexp.MustCompile(`^(?i)(ask|page|doc)?\s*#?\s*([0-9]{1,18})$`)

// paletteFindQuery is what one search looks for.
type paletteFindQuery struct {
	// AskID and PageID are the ids an id-shaped query names (0: none).
	AskID, PageID int64
	// Text is the words to find ("" for an id, or for nothing to search).
	Text string
}

// parsePaletteFind is raw as a search: an id, text, or — too short, too long
// or blank — nothing.
func parsePaletteFind(raw string) paletteFindQuery {
	raw = strings.TrimSpace(raw)
	if m := paletteQueryID.FindStringSubmatch(raw); m != nil {
		if n, err := strconv.ParseInt(m[2], 10, 64); err == nil && n > 0 {
			switch strings.ToLower(m[1]) {
			case "ask":
				return paletteFindQuery{AskID: n}
			case "page", "doc":
				return paletteFindQuery{PageID: n}
			}
			return paletteFindQuery{AskID: n, PageID: n}
		}
	}
	if n := utf8.RuneCountInString(raw); n < ui.PaletteServerMinRunes || n > paletteFindMaxRunes {
		return paletteFindQuery{}
	}
	return paletteFindQuery{Text: raw}
}

// paletteAsk is one ask the search found.
type paletteAsk struct {
	ID    int64  `json:"id"`
	Href  string `json:"href"`
	Title string `json:"title"`
	// State is its state's words ("Open", "Answered", "Notice · Open").
	State string `json:"state"`
	Open  bool   `json:"open"`
	// ByID: the query named this ask's id.
	ByID bool `json:"byId"`
}

// palettePage is one Pages document the search found.
type palettePage struct {
	ID    int64  `json:"id"`
	Href  string `json:"href"`
	Title string `json:"title"`
	ByID  bool   `json:"byId"`
}

// paletteFound is the search's answer.
type paletteFound struct {
	Asks  []paletteAsk  `json:"asks"`
	Pages []palettePage `json:"pages"`
}

func (s *Server) registerPaletteFindRoutes(mux Mux) {
	mux.HandleFunc("GET "+ui.PaletteFindRoute, s.requireSession(s.handlePaletteFind))
}

// handlePaletteFind is GET /ui/palette/find?q=…: {"asks": [...], "pages":
// [...]}, each ranked and cut to paletteFindShown. A query that is neither an
// id nor text of a searchable length answers both empty without reading
// anything; so does a kind this server does not serve.
func (s *Server) handlePaletteFind(w http.ResponseWriter, r *http.Request) {
	q := parsePaletteFind(r.URL.Query().Get("q"))
	out := paletteFound{Asks: []paletteAsk{}, Pages: []palettePage{}}
	ctx := r.Context()
	out.Asks = append(out.Asks, s.paletteAsks(ctx, q)...)
	out.Pages = append(out.Pages, s.palettePages(ctx, q)...)
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, out)
}

// paletteAsks is the caller's asks q finds, ranked, at most paletteFindShown.
func (s *Server) paletteAsks(ctx context.Context, q paletteFindQuery) []paletteAsk {
	if s.ext.Attention == nil || (q.AskID <= 0 && q.Text == "") {
		return nil
	}
	found, err := s.ext.Attention.Search(ctx, attention.Search{AskID: q.AskID, Text: q.Text, Limit: paletteFindRead})
	if err != nil {
		s.logger.Printf("palette: search asks: %v", err)
		return nil
	}
	viewer := requestOwner(ctx)
	text := strings.ToLower(q.Text)
	type ranked struct {
		a        paletteAsk
		question bool
	}
	var rs []ranked
	for _, a := range found {
		// 🔴 Only an ask whose page opens for the caller (their own, never
		// the chief's): a result is a link, and no link may be a 404.
		if !askHasPage(a, viewer) {
			continue
		}
		title := strings.TrimSpace(a.Question)
		if a.StepUp != nil {
			title = textplain.Title(stepUpTitleText(a.StepUp))
		}
		rs = append(rs, ranked{
			a: paletteAsk{ID: a.ID, Href: askPagePath(a.ID), Title: firstNonBlank(title, askerLabel(a)), State: askSearchState(a),
				Open: a.Status == attention.Open, ByID: q.AskID > 0 && a.ID == q.AskID},
			question: text != "" && strings.Contains(strings.ToLower(title), text),
		})
	}
	slices.SortStableFunc(rs, func(x, y ranked) int {
		switch {
		case x.a.ByID != y.a.ByID:
			return boolFirst(x.a.ByID)
		case x.a.Open != y.a.Open:
			return boolFirst(x.a.Open)
		case x.question != y.question:
			return boolFirst(x.question)
		}
		// Newest first (Search's own order; the ids rise with time).
		return compareDesc(x.a.ID, y.a.ID)
	})
	out := make([]paletteAsk, 0, min(len(rs), paletteFindShown))
	for i := 0; i < len(rs) && i < paletteFindShown; i++ {
		out = append(out, rs[i].a)
	}
	return out
}

// palettePages is the caller's Pages documents q finds, at most
// paletteFindShown: the one an id names, or those whose title holds the text,
// newest activity first.
func (s *Server) palettePages(ctx context.Context, q paletteFindQuery) []palettePage {
	if !s.pagesOn() {
		return nil
	}
	if q.PageID > 0 {
		p, err := s.ext.Pages.Page(ctx, q.PageID)
		if err != nil {
			// Another owner's page reads as a missing one (row-level security).
			return nil
		}
		return []palettePage{{ID: p.ID, Href: pagePath(p.ID), Title: p.Title, ByID: true}}
	}
	if q.Text == "" {
		return nil
	}
	found, err := s.ext.Pages.TitleSearch(ctx, q.Text, paletteFindShown)
	if err != nil {
		s.logger.Printf("palette: search pages: %v", err)
		return nil
	}
	out := make([]palettePage, 0, len(found))
	for _, p := range found {
		out = append(out, palettePage{ID: p.ID, Href: pagePath(p.ID), Title: p.Title})
	}
	return out
}

// boolFirst orders true before false (x and y differ).
func boolFirst(x bool) int {
	if x {
		return -1
	}
	return 1
}

// compareDesc orders larger first.
func compareDesc(x, y int64) int { return cmp.Compare(y, x) }
