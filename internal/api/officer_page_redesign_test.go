package api

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE OFFICER PAGE REDESIGN's pure halves (the operator): the spend
// bar's levels and Raise, and the answer box on its own line on every
// surface the shared answer form renders.

// rdRaise stands for the Raise form: a marker no rendering writes itself.
const rdRaise = `<form data-rd-raise-marker></form>`

// TestTheSpendBarLevels (PR A (4)): a $10 limit at $0 (0%), $7.90 (79%:
// plain, no Raise), $8.00 (80%: amber + Raise), $8.50 (85%: amber + Raise),
// $10.00 and $12.00 (held: red + Raise). The tone, the level, the words and
// the bar's fill are each asserted, so a mutant of any one shows.
func TestTheSpendBarLevels(t *testing.T) {
	for _, c := range []struct {
		spent      float64
		level      string
		tone       ui.ProgressTone
		pct, words string
		raise      bool
	}{
		{0, "under", ui.ProgressOK, "0", "$0.00 of $10 today", false},
		{7.9, "under", ui.ProgressOK, "79", "$7.90 of $10 today", false},
		{8, "near", ui.ProgressWarn, "80", "$8.00 of $10 today", true},
		{8.5, "near", ui.ProgressWarn, "85", "$8.50 of $10 today", true},
		{10, "held", ui.ProgressFull, "100", "$10.00 of $10 today", true},
		{12, "held", ui.ProgressFull, "100", "$12.00 of $10 today", true},
	} {
		out := officerSpendHTML(officerSpend{read: true, spent: c.spent, budget: 10, reset: "00:00 UTC", raise: rdRaise})
		for _, want := range []string{`data-spend-level="` + c.level + `"`, `data-progress-tone="` + string(c.tone) + `"`,
			`aria-valuenow="` + c.pct + `"`, `style="width:` + c.pct + `%"`, `>` + c.words + `</p>`, `aria-valuetext="` + c.words + `"`} {
			if !strings.Contains(out, want) {
				t.Errorf("$%.2f of $10: lacks %q:\n%s", c.spent, want, out)
			}
		}
		if got := strings.Contains(out, rdRaise); got != c.raise {
			t.Errorf("$%.2f of $10: Raise drawn %v, want %v", c.spent, got, c.raise)
		}
		if held := strings.Contains(out, "data-run-held"); held != (c.level == "held") {
			t.Errorf("$%.2f of $10: held line %v", c.spent, held)
		}
		if near := strings.Contains(out, "data-run-near"); near != (c.level == "near") {
			t.Errorf("$%.2f of $10: near line %v", c.spent, near)
		}
	}
	// No limit: no bar and no Raise, whatever was spent.
	out := officerSpendHTML(officerSpend{read: true, spent: 25, raise: rdRaise})
	if strings.Contains(out, `role="progressbar"`) || strings.Contains(out, rdRaise) || !strings.Contains(out, "$25.00 spent today · no daily limit") {
		t.Errorf("no limit:\n%s", out)
	}
}

// TestTheSevenDayUsageFoldsIntoTheSpendBarsDetails: the last 7 days' usage
// is in the bar's details (a closed <details>), not a card of its own.
func TestTheSevenDayUsageFoldsIntoTheSpendBarsDetails(t *testing.T) {
	week := "Last 7 days: 41 requests · about $3.17 (an estimate at list price)."
	out := officerSpendHTML(officerSpend{read: true, spent: 1, budget: 10, reset: "00:00 UTC", week: week})
	det := regexp.MustCompile(`(?s)<details data-spend-details>.*?</details>`).FindString(out)
	if !strings.Contains(det, week) || strings.Contains(det, "<details data-spend-details open") {
		t.Fatalf("the week's usage is not in the bar's closed details:\n%s", out)
	}
}

// answerRow is item key's answer box row and what follows the box.
func answerRow(t *testing.T, surface, out string) (row, actions string) {
	t.Helper()
	m := regexp.MustCompile(`(?s)<div class="([^"]*)" data-asks-answer-row>(<textarea [^>]*></textarea>)<div class="([^"]*)" data-asks-answer-actions>(.*?)</form>`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("%s: the answer box is not on a line of its own, followed by its actions row:\n%s", surface, out)
	}
	return m[1] + " " + m[2], m[3] + " " + m[4]
}

// TestTheAnswerBoxSitsOnALineOfItsOwn (PR A (8)): on the Asks page (its
// feed) and on an officer's page (a list drawn with a return), the answer
// form's box is the FIRST thing in its row, a column, full width; Answer and
// Dismiss are in the actions row after it — never beside the box.
func TestTheAnswerBoxSitsOnALineOfItsOwn(t *testing.T) {
	now := time.Date(2000, 1, 3, 12, 0, 0, 0, time.UTC)
	items := []attentionItem{{Key: "ask-31", Kind: attentionKindAsk, AskID: 31, Text: "Which repo first?"}}
	feed, _ := askFeedHTML(items, "", 0, now)
	officer := attentionItemsHTMLTo(items, now, officersPagePath+"/scout:alpha")
	for surface, out := range map[string]string{"the Asks page": feed, "an officer's page": officer} {
		row, actions := answerRow(t, surface, out)
		if !strings.Contains(row, ui.AskFormAnswerRowClass) || !strings.Contains(ui.AskFormAnswerRowClass, "flex-col") {
			t.Errorf("%s: the box row is not a column: %q", surface, row)
		}
		if !strings.Contains(row, `class="`+ui.AskFormAnswerInputClass+`"`) || !strings.Contains(ui.AskFormAnswerInputClass, "w-full") {
			t.Errorf("%s: the box is not full width: %q", surface, row)
		}
		if !strings.Contains(actions, `>Answer</button>`) || strings.Contains(actions, "<textarea") {
			t.Errorf("%s: Answer is not in the actions row under the box:\n%s", surface, actions)
		}
	}
}

// TestARefreshThatRestartedEverythingSaysSo (G1): what Save and refresh tells
// the person, from exactly what happened. Every copy restarted and only a
// piece of held work could not be handed back: a notice that it restarted —
// never "could not be restarted". A copy not stopped, or not started: a
// problem naming how many of each. A failure before anything: the retry line.
func TestARefreshThatRestartedEverythingSaysSo(t *testing.T) {
	for _, c := range []struct {
		name          string
		res           officers.RefreshResult
		err           error
		notice, words string
	}{
		{"all restarted", officers.RefreshResult{Restarted: []string{"a", "b"}}, nil, "refreshed", ""},
		{"none running", officers.RefreshResult{}, nil, "refreshed-idle", ""},
		{"all restarted, work not handed back", officers.RefreshResult{Restarted: []string{"a", "b"}, Unreleased: []officers.ItemID{"7"}}, nil, "refreshed-unreleased", ""},
		{"one not stopped", officers.RefreshResult{Restarted: []string{"a"}, NotStopped: []string{"b"}}, nil, "",
			"Saved. 1 of its 2 running copies restarted in a new conversation. 1 could not be stopped and keep their old conversation. Try Save and refresh again to restart the rest."},
		{"one not started", officers.RefreshResult{Restarted: []string{"a"}, NotStarted: []string{"b"}, Unreleased: []officers.ItemID{"7"}}, nil, "",
			"Saved. 1 of its 2 running copies restarted in a new conversation. 1 stopped and did not start again; they start with their next work. 1 piece(s) of work they held go back to the queue in a few minutes. Try Save and refresh again to restart the rest."},
		{"a failure before anything", officers.RefreshResult{}, errors.New("read"), "",
			"Saved, but it could not be restarted just now: try Save and refresh again in a moment."},
	} {
		notice, problem := refreshOutcome(c.res, c.err)
		if notice != c.notice || problem != c.words {
			t.Errorf("%s: notice %q problem %q; want %q %q", c.name, notice, problem, c.notice, c.words)
		}
	}
}
