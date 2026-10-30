package ccdialog

import (
	"os"
	"strings"
	"testing"
)

// 🔴 THE USAGE-LIMIT MENU IS RECOGNISED, WITH ITS LIMIT LINE; NO OTHER DIALOG IS.
//
// pane_usage_limit_menu.txt is NOT a capture of a live pane: no limit was hit
// while recording. It is the pinned CLI's screen (pane_permission_2.1.296.txt's
// banner and width) with the menu Claude Code 2.1.289 builds for a hit limit —
// title "What do you want to do?", its options in the CLI's order, the cursor on
// "Stop and wait for limit to reset" — and the limit line and cursor row ccd
// logged in production for a weekly limit. The negative controls are real
// captures: 2.1.296's permission prompt and idle prompt, and 2.1.289's
// permission prompt and AskUserQuestion picker (internal/askdrive/testdata).
func TestUsageLimitReadsTheLimitMenuAndNoOtherDialog(t *testing.T) {
	menu := fixture(t, "pane_usage_limit_menu.txt")
	if _, dialog := OnScreen(menu); !dialog {
		t.Fatal("instrument check: the limit menu is not read as a dialog at all")
	}
	const line = "You’ve hit your weekly limit · resets Nov 2, 1am (UTC)"
	limit, shown := UsageLimit(menu)
	if !shown {
		t.Fatal("the usage-limit menu was not read as the limit menu")
	}
	if limit != line {
		t.Errorf("limit line = %q; want %q", limit, line)
	}

	// No limit line on screen (scrolled away): still the menu, with no limit.
	if limit, shown := UsageLimit(strings.Replace(menu, line, "API Error", 1)); !shown || limit != "" {
		t.Errorf("menu without a limit line: %q, %t; want \"\", true", limit, shown)
	}
	// The ASCII apostrophe, as the transcript's record spells it.
	if limit, _ := UsageLimit(strings.Replace(menu, "You’ve", "You've", 1)); limit != "You've hit your weekly limit · resets Nov 2, 1am (UTC)" {
		t.Errorf("ASCII apostrophe: limit line = %q", limit)
	}
	// With usage credits on the CLI labels the option just "Stop"; a sibling
	// option says it is the limit menu.
	credits := strings.Replace(strings.Replace(menu, "1. Stop and wait for limit to reset", "1. Stop", 1),
		"3. Upgrade your plan", "3. Switch to usage credits", 1)
	if _, shown := UsageLimit(credits); !shown {
		t.Error("the usage-credits variant (\"Stop\" + \"Switch to usage credits\") was not read as the limit menu")
	}

	// Negative controls: dialogs that are not the limit menu.
	for _, name := range []string{"pane_permission_2.1.296.txt"} {
		if _, dialog := OnScreen(fixture(t, name)); !dialog {
			t.Fatalf("instrument check: %s is not a dialog", name)
		}
		if _, shown := UsageLimit(fixture(t, name)); shown {
			t.Errorf("%s: read as the limit menu", name)
		}
	}
	if _, shown := UsageLimit(fixture(t, "pane_idle_2.1.296.txt")); shown {
		t.Error("pane_idle_2.1.296.txt: read as the limit menu")
	}
	for _, name := range []string{"not-picker-permission.txt", "single-q.txt", "multi-review.txt"} {
		b, err := os.ReadFile("../../internal/askdrive/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, dialog := OnScreen(string(b)); !dialog {
			t.Fatalf("instrument check: %s is not a dialog", name)
		}
		if _, shown := UsageLimit(string(b)); shown {
			t.Errorf("%s: read as the limit menu", name)
		}
	}
	// A bare "Stop" beside ordinary options is some other menu.
	bare := strings.Replace(strings.Replace(credits, "3. Switch to usage credits", "3. Keep going", 1),
		"2. Wait here, then continue automatically at Nov 2, 1am", "2. Retry", 1)
	if _, dialog := OnScreen(bare); !dialog {
		t.Fatal("instrument check: the bare-Stop menu is not a dialog")
	}
	if _, shown := UsageLimit(bare); shown {
		t.Error("a bare \"Stop\" with no limit-menu sibling was read as the limit menu")
	}
	// The menu's rows above the input box — quoted in the transcript — are not it.
	rule := strings.Repeat("─", 80)
	quoted := "  ❯ 1. Stop and wait for limit to reset\n" + rule + "\n❯ \n" + rule + "\n  ? for shortcuts\n"
	if _, shown := UsageLimit(quoted); shown {
		t.Error("the menu's words above the input box were read as the limit menu")
	}
}
