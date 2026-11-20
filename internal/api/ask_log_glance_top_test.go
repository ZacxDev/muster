package api

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/asklog"
)

// (Its own file: it names TopQuestion.Question, which origin/main lacks, so
// the rest of ask_log_glance_test.go still compiles there for the red run.)

// TestATopQuestionShowsWhatWasAsked: a repeated question is shown as the
// owner saw it (TopQuestion.Question's last paragraph: what was asked, not the
// report before it), never its lower-cased normal form,
// which stays only in data-norm; its facts are one short line, each fact kept
// whole, "you answered N" only when it was not you every time, and its usual
// answer worded by how usual it is.
func TestATopQuestionShowsWhatWasAsked(t *testing.T) {
	sum := asklog.Summary{Total: 9, Top: []asklog.TopQuestion{
		{Norm: "ship the release now", Question: "Release notes are drafted.\n\nShip the RELEASE now?", Count: 5, ByOwner: 3, TopAnswer: "yes", TopAnswerCount: 4, MedianAnswer: 31 * time.Second},
		{Norm: "keep going", Question: "Keep going?", Count: 2, ByOwner: 2, TopAnswer: "continue", TopAnswerCount: 2, MedianAnswer: 430 * time.Second},
	}}
	html := askLogSummaryHTML(sum, asklog.DefaultTextTTL)
	rows := regexp.MustCompile(`(?s)<li [^>]*data-asklog-top-row.*?</li>`).FindAllString(html, -1)
	if len(rows) != 2 {
		t.Fatalf("%d repeated questions drawn, want 2:\n%s", len(rows), html)
	}
	for i, c := range []struct{ norm, text, facts string }{
		{"ship the release now", "Ship the RELEASE now?", "5 times · you answered 3 · usually “yes” · answered in about 31s"},
		{"keep going", "Keep going?", "2 times · always “continue” · answered in about 7m 10s"},
	} {
		text := regexp.MustCompile(`data-asklog-top-question>([^<]*)<`).FindStringSubmatch(rows[i])
		if text == nil || text[1] != c.text {
			t.Errorf("row %d shows %q, want the asked text %q", i, text, c.text)
		}
		if strings.Contains(reTag.ReplaceAllString(rows[i], ""), c.norm) {
			t.Errorf("row %d shows the normal form %q:\n%s", i, c.norm, rows[i])
		}
		if !strings.Contains(rows[i], `data-norm="`+c.norm+`"`) {
			t.Errorf("row %d does not carry its group's normal form", i)
		}
		facts := regexp.MustCompile(`(?s)data-asklog-top-facts>(.*?)</p>`).FindStringSubmatch(rows[i])[1]
		if got := reTag.ReplaceAllString(facts, ""); got != c.facts {
			t.Errorf("row %d facts %q, want %q", i, got, c.facts)
		}
		if n := strings.Count(facts, `<span class="whitespace-nowrap">`); n != strings.Count(c.facts, " · ")+1 {
			t.Errorf("row %d: %d facts kept whole, want each one", i, n)
		}
	}
}
