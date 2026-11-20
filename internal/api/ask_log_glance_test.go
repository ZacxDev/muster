package api

import (
	"context"
	"io"
	"log"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE LOG TAB AT A GLANCE (ask_log_read.go): what a stranger reads off a phone
// screenshot of /attention?tab=log. Rendered with no database: the summary and
// the rows come from a fake store, so what is under test is the drawing.

// glanceStore is an asklog.Store whose reads answer fixed values; any other
// method panics (the embedded nil interface), so the tab can only read.
type glanceStore struct {
	asklog.Store
	sum  asklog.Summary
	rows []asklog.Ask
}

func (g glanceStore) Summarize(context.Context, asklog.Filter, int) (asklog.Summary, error) {
	return g.sum, nil
}

func (g glanceStore) Page(context.Context, asklog.Filter, int64, int) ([]asklog.Ask, int64, error) {
	return g.rows, 0, nil
}

var glanceNow = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

// glancePanel is the Log tab's panel over st.
func glancePanel(t *testing.T, st asklog.Store) string {
	t.Helper()
	s := &Server{now: func() time.Time { return glanceNow }, logger: log.New(io.Discard, "", 0), askLog: &askLogger{cfg: AskLog{Store: st}}}
	return s.askLogPanelHTML(context.Background(), url.Values{})
}

var (
	reTag    = regexp.MustCompile(`<[^>]*>`)
	reDigits = regexp.MustCompile(`[0-9]+`)
)

// visibleWords is html's text, lower-cased, without tags or numbers.
func visibleWords(html string) []string {
	return strings.Fields(strings.ToLower(reDigits.ReplaceAllString(reTag.ReplaceAllString(html, " "), " ")))
}

// glanceOutcomeSum is a log of 64 asks whose outcome counts are pairwise
// distinct and equal to no constant the tab names (its 10% share): 41
// answered, 13 moved on, 4 declined, 3 with no outcome, 2 expired, 1
// superseded. 10% of 64 is 6.4, so Moved on stands alone and Declined,
// Expired and Superseded (4 + 2 + 1 = 7) fold.
func glanceOutcomeSum() asklog.Summary {
	return asklog.Summary{Total: 64, Answered: 45,
		ByKind: map[string]int{asklog.KindPlainText: 64},
		ByOutcome: map[string]int{asklog.OutcomeAnswered: 41, asklog.OutcomeMovedOn: 13, asklog.OutcomeDeclined: 4, asklog.OutcomeOpen: 3,
			asklog.OutcomeExpired: 2, asklog.OutcomeSuperseded: 1},
		ByAnsweredBy: map[string]int{asklog.ByOwner: 45, asklog.AnsweredByNone: 19}}
}

// TestTheLogNeverSaysTheHeadersOpenWord: the page header says "N open" — what
// waits on you now — so the LOG's row with no outcome, wherever it is drawn
// (its "How it ended" count, the Outcome filter, a row's badge), shares no word
// with it, and reads "No answer". RED at origin/main fcb27a6b: all three said
// "Open", under a header reading "0 open" (the operator's phone screenshot:
// "Asks · 0 open" over the log's "Open 3").
func TestTheLogNeverSaysTheHeadersOpenWord(t *testing.T) {
	header := visibleWords(ui.AsksOpenCountHTML(17))
	if len(header) != 1 || header[0] != "open" {
		t.Fatalf("the header's count reads %q; this test assumes its one word is \"open\"", header)
	}
	sum := glanceOutcomeSum()
	surfaces := map[string]string{
		"How it ended": regexp.MustCompile(`(?s)data-count="open">(.*?)</div>`).FindStringSubmatch(askLogSummaryHTML(sum, asklog.DefaultTextTTL))[1],
		"the filter":   regexp.MustCompile(`<option value="open"[^>]*>([^<]*)</option>`).FindStringSubmatch(askLogFormHTML(url.Values{}))[1],
		"a row's badge": regexp.MustCompile(`data-asklog-outcome>([^<]*)<`).FindStringSubmatch(
			askLogCardHTML(asklog.Ask{ID: 23, Kind: asklog.KindPlainText, Question: "Merge it?", AskedAt: glanceNow.Add(-time.Hour)}, glanceNow, asklog.DefaultTextTTL))[1],
	}
	for where, html := range surfaces {
		words := visibleWords(html)
		if got := strings.Join(words, " "); got != "no answer" {
			t.Errorf("%s labels an ask with no outcome %q, want \"no answer\"", where, got)
		}
		for _, w := range words {
			if w == header[0] {
				t.Errorf("%s says %q, the header's own word for what waits on you now", where, w)
			}
		}
	}
}

// countRows is the "How it ended" rows: key, label and count, in order.
func countRows(t *testing.T, html, key string) []string {
	t.Helper()
	block := regexp.MustCompile(`(?s)<dl [^>]*aria-labelledby="asklog-counts-` + key + `">.*?</dl>`).FindString(html)
	var out []string
	for _, m := range regexp.MustCompile(`data-count="([^"]+)"><dt[^>]*>([^<]*)</dt><dd[^>]*>([^<]*)</dd>`).FindAllStringSubmatch(block, -1) {
		out = append(out, m[1]+"|"+m[2]+"|"+m[3])
	}
	return out
}

// TestRareOutcomesFoldIntoOther: "How it ended" shows Answered, No answer and
// any outcome holding a tenth of the asks on rows of their own, most first, and
// FOLDS the rest into one "Other" row (their sum, last), whose breakdown — each
// outcome's count and meaning — is in the ⓘ. A fold of ONE outcome is drawn by
// its own name. RED at origin/main fcb27a6b: every outcome was its own pill.
func TestRareOutcomesFoldIntoOther(t *testing.T) {
	html := askLogSummaryHTML(glanceOutcomeSum(), asklog.DefaultTextTTL)
	want := []string{"answered|Answered|41", "moved_on|Skipped|13", "open|No answer|3", "other|Other|7"}
	if got := countRows(t, html, "outcome"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("How it ended: %q, want %q", got, want)
	}
	sheet := regexp.MustCompile(`(?s)data-asklog-outcome-meanings>.*?</dl>`).FindString(html)
	for k, folded := range map[string]bool{asklog.OutcomeDeclined: true, asklog.OutcomeExpired: true, asklog.OutcomeSuperseded: true,
		asklog.OutcomeAnswered: false, asklog.OutcomeMovedOn: false, asklog.OutcomeOpen: false} {
		m := regexp.MustCompile(`<div data-meaning="` + k + `"( data-folded)?>`).FindStringSubmatch(sheet)
		if m == nil {
			t.Errorf("the ⓘ does not explain %s", k)
			continue
		}
		if (m[1] != "") != folded {
			t.Errorf("the ⓘ marks %s folded=%v, want %v", k, m[1] != "", folded)
		}
	}
	if !strings.Contains(sheet, "Replaced by a newer question before anyone answered.") {
		t.Errorf("the ⓘ does not say what Superseded means:\n%s", sheet)
	}

	// One rare outcome: no "Other 2", the outcome's own name. 50 answered, 6
	// with no outcome, 2 gone (58: Gone's 2 is under 5.8).
	one := asklog.Summary{Total: 58, ByOutcome: map[string]int{asklog.OutcomeAnswered: 50, asklog.OutcomeOpen: 6, asklog.OutcomeGone: 2}}
	want = []string{"answered|Answered|50", "open|No answer|6", "gone|Gone|2"}
	if got := countRows(t, askLogSummaryHTML(one, asklog.DefaultTextTTL), "outcome"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("one rare outcome: %q, want %q", got, want)
	}
}

// TestTheBreakdownsAreStatisticsNotPills: no count in the summary is drawn
// as a pill (a pill reads as a tap target; none of them is one). RED at
// origin/main fcb27a6b: ~15 pill chips.
func TestTheBreakdownsAreStatisticsNotPills(t *testing.T) {
	html := askLogSummaryHTML(glanceOutcomeSum(), asklog.DefaultTextTTL)
	section := regexp.MustCompile(`(?s)data-asklog-summary>.*?</section>`).FindString(html)
	if section == "" {
		t.Fatalf("no summary section:\n%s", html)
	}
	if n := strings.Count(section, ui.AsksPillBaseClass); n != 0 {
		t.Errorf("%d pills in the summary:\n%s", n, section)
	}
	// POSITIVE CONTROL: the rows are there, as rows.
	if got := countRows(t, html, "kind"); len(got) != 1 || got[0] != "plain_text|Question in chat|64" {
		t.Errorf("Kind of question: %q", got)
	}
}

// TestTheLogTabsHeadingsShareOneClass: every section of the tab — the three
// breakdowns, the questions asked most often, the list — is headed by an <h2>
// in the Asks page's ONE heading class (the Open tab's group labels' own).
// RED at origin/main fcb27a6b: "By kind" / "By outcome" / "Answered by" were
// small title-case spans beside the caps "ASKED MOST OFTEN" and "ASKS".
func TestTheLogTabsHeadingsShareOneClass(t *testing.T) {
	st := glanceStore{sum: glanceOutcomeSum(), rows: []asklog.Ask{{ID: 31, Kind: asklog.KindPlainText, Question: "Merge it?", AskedAt: glanceNow.Add(-time.Hour)}}}
	st.sum.Top = []asklog.TopQuestion{{Norm: "merge it", Count: 3, ByOwner: 3}}
	panel := glancePanel(t, st)
	open := regexp.MustCompile(`class="([^"]*)"`).FindStringSubmatch(ui.AsksGroupLabel("x", "Questions", 2))[1]
	heads := regexp.MustCompile(`<h2 [^>]*class="([^"]*)"[^>]*>([^<]*)`).FindAllStringSubmatch(panel, -1)
	var names []string
	for _, h := range heads {
		names = append(names, h[2])
		if h[1] != open {
			t.Errorf("heading %q has class %q, want the Asks page's %q", h[2], h[1], open)
		}
	}
	want := []string{"Kind of question", "How it ended", "Who answered", "Asked most often", "Every ask, newest first"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("headings %q, want %q", names, want)
	}
	// Every breakdown is a heading, not a span: no title-case label is left.
	if strings.Contains(panel, `<span class="px-1 text-xs font-semibold text-fg2">`) {
		t.Errorf("a breakdown still has its own label style:\n%s", panel)
	}
}
