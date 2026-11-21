package api

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE ASKS PAGE IN PLAIN WORDS (the jargon audit, theme "asks"): the Log tab's
// filters, tiles, labels and row sub line, and the Open feed's question face,
// say what a newcomer reads, not muster's internal names. Each test pins the
// WHOLE normalised string, so a reword fails it rather than slipping past.

var plainTagRe = regexp.MustCompile(`<[^>]*>`)

// plainText is s's visible text: tags dropped, whitespace collapsed.
func plainText(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(plainTagRe.ReplaceAllString(s, " "))), " ")
}

// The filter form, every field and option, as one line.
func TestTheAskLogFiltersSayPlainWords(t *testing.T) {
	form := askLogFormHTML(url.Values{})
	const want = "Filters Kind Any Question in chat Question with choices Agent's question Question (opencode) Permission (opencode) " +
		"Outcome Any No answer Answered Answered in session Declined Skipped Expired Cancelled Replaced Gone Unknown " +
		"Answered by Any You The chief Someone else Nobody When Last 24 hours Last 7 days Last 30 days All time " +
		"App Any Claude Code opencode Agent Host Project Session Apply Clear"
	if got := plainText(form); got != want {
		t.Errorf("the filter form reads\n%q\nwant\n%q", got, want)
	}
	// The placeholders are attributes, not text: pinned by field.
	for _, w := range []string{`name="host" value="" placeholder="any"`, `name="repo" value="" placeholder="any"`, `name="session" value="" placeholder="Session name"`} {
		if !strings.Contains(form, w) {
			t.Errorf("the form lacks %s:\n%s", w, form)
		}
	}
	for _, bad := range []string{"Runtime", "Repository", "owner/name", "tmux or session id", "All kept", "Moved on", "Superseded", "opencode question", "opencode permission request"} {
		if strings.Contains(form, bad) {
			t.Errorf("the filter form still says %q", bad)
		}
	}
}

// The four tiles, in words rather than statistics. P90Answer and P90Within
// differ, so the 9-in-10 tile is seen to draw the discrete one (its claim is
// only true of that; the pg test pins the 10/20/40 s case end to end).
func TestTheAskLogTilesSayHowLongInWords(t *testing.T) {
	sum := asklog.Summary{Total: 7, Answered: 5, ByAnsweredBy: map[string]int{asklog.ByOwner: 4},
		MedianAnswer: 31 * time.Second, P90Answer: 7*time.Minute + 3*time.Second, P90Within: 7*time.Minute + 57*time.Second}
	out := askLogSummaryHTML(sum, 30*24*time.Hour)
	var tiles []string
	for _, m := range regexp.MustCompile(`data-asklog-stat="[a-z0-9]+"><span[^>]*>([^<]*)</span><span[^>]*>([^<]*)</span></div>`).FindAllStringSubmatch(out, -1) {
		tiles = append(tiles, m[1]+" "+m[2])
	}
	const want = "7 asks | 4 answered by you | 31s typical time to answer | 7m 57s 9 in 10 answered within"
	if got := strings.Join(tiles, " | "); got != want {
		t.Errorf("the tiles read\n%q\nwant\n%q", got, want)
	}
	for _, bad := range []string{"percentile", "median to answer"} {
		if strings.Contains(out, bad) {
			t.Errorf("the summary still says %q", bad)
		}
	}
}

// A row's sub line says the session ON its computer, never "session @ host".
func TestALogRowSaysWhereInWords(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		a    asklog.Ask
		want string
	}{
		{"session and host", asklog.Ask{Host: "atlas", TmuxSession: "ratelimit", Repo: "acme/api"}, "ratelimit on atlas · acme/api · 15m ago"},
		{"host only", asklog.Ask{Host: "nomad", Repo: "acme/web"}, "on nomad · acme/web · 15m ago"},
		{"no repo", asklog.Ask{Host: "atlas", TmuxSession: "onboarding"}, "onboarding on atlas · 15m ago"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.a.ID, c.a.Kind, c.a.Runtime, c.a.AskedAt = 7, asklog.KindPlainText, asklog.RuntimeClaudeCode, now.Add(-15*time.Minute)
			out := askLogCardHTML(c.a, now, 30*24*time.Hour)
			i := strings.Index(out, `data-asklog-sub`)
			if i < 0 {
				t.Fatalf("no sub line:\n%s", out)
			}
			sub := out[strings.LastIndex(out[:i], "<"):]
			sub = sub[:strings.Index(sub, `</span></span></span>`)]
			if got := plainText(sub); got != c.want {
				t.Errorf("the sub line reads %q, want %q", got, c.want)
			}
			if strings.Contains(out, " @ ") || strings.Contains(out, ">@ ") {
				t.Errorf("the row still says \"@\":\n%s", out)
			}
		})
	}
}

// 🔴 A QUESTION IS A SENTENCE, NOT CODE: a held question (and a session
// waiting on AskUserQuestion) is drawn in the card's own face; only a session
// waiting on a tool call keeps the code face. The positive control is the
// Bash item, which MUST stay monospace, so the check can see the class.
func TestAnAsksQuestionIsNotDrawnAsCode(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	at := mirror.Place{Code: "fern:2", Host: "atlas"}
	question := "The limiter is written. Should the limit be per API key or per client IP?"
	for _, c := range []struct {
		name string
		it   attentionItem
		mono bool
	}{
		{"held question", attentionItem{Key: "session-aaaaaaaaaaaaaaaa", Kind: attentionKindQuestion, Source: "tmux fern:2 @ atlas", target: "tmux fern:2 @ atlas", at: at, Text: question, Since: now.Add(-time.Minute)}, false},
		{"session waiting on a question", attentionItem{Key: "session-bbbbbbbbbbbbbbbb", Kind: attentionKindSession, Source: "tmux fern:2 @ atlas", target: "tmux fern:2 @ atlas", at: at, Text: question, tool: claudeCodeQuestionTool, Since: now.Add(-time.Minute)}, false},
		{"session waiting on a tool call", attentionItem{Key: "session-cccccccccccccccc", Kind: attentionKindSession, Source: "tmux fern:2 @ atlas", target: "tmux fern:2 @ atlas", at: at, Text: "Bash: npm publish", tool: "Bash", Since: now.Add(-time.Minute)}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := askFeedCardHTML(c.it, now)
			m := regexp.MustCompile(`<div class="([^"]*)" data-attention-text>([^<]*)</div>`).FindStringSubmatch(out)
			if m == nil {
				t.Fatalf("no question line:\n%s", out)
			}
			if m[2] != c.it.Text {
				t.Fatalf("the question line reads %q, want %q", m[2], c.it.Text)
			}
			if got := strings.Contains(" "+m[1]+" ", " font-mono "); got != c.mono {
				t.Errorf("monospace = %v, want %v (class %q)", got, c.mono, m[1])
			}
		})
	}
}
