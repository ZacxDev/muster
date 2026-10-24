package suggest

import (
	"strings"
	"testing"
)

// A synthetic conversation every filter test grounds against: the agent offers
// two options and asks; the operator asked two things earlier.
func filterInput(t *testing.T) Built {
	t.Helper()
	b, ok := Build(Input{
		Asks: []string{"make the export job resumable", "and keep the batch size configurable"},
		Tail: []Entry{
			{Role: RoleUser, Text: "make the export job resumable"},
			{Role: RoleTool, Text: "Edit internal/export/job.go"},
			{Role: RoleAssistant, Text: "Two ways: (a) checkpoint every 500 rows in the jobs table, (b) idempotent re-run keyed on batch id. " +
				"(b) is simpler but re-reads the source. Should I run the cleanup script with --dry-run first? Which do you prefer?"},
		},
	})
	if !ok {
		t.Fatal("the fixture has a final reply")
	}
	return b
}

func good() Raw {
	return Raw{Text: "Go with (a), checkpoint every 500 rows", Kind: KindPick, Cites: []string{"R"}, Quote: "(a) checkpoint every 500 rows"}
}

// TestEachFilterRuleDropsWithItsOwnReason: every rule, reached by a case that
// passes every rule before it, drops with ITS reason (so deleting one rule
// turns exactly its row red with a different reason or a survivor).
func TestEachFilterRuleDropsWithItsOwnReason(t *testing.T) {
	b := filterInput(t)
	cases := []struct {
		name string
		raw  func() Raw
		want Drop
	}{
		{"kind", func() Raw { r := good(); r.Kind = "approve"; return r }, DropKind},
		{"empty", func() Raw { r := good(); r.Text = "   "; return r }, DropEmpty},
		{"too long", func() Raw {
			r := good()
			r.Text = "Go with (a), checkpoint every 500 rows " + strings.Repeat("x", 120)
			return r
		}, DropTooLong},
		{"slash", func() Raw { r := good(); r.Text = "/clear then go with (a), checkpoint every 500 rows"; return r }, DropSlashBang},
		{"bang", func() Raw { r := good(); r.Text = "!rm checkpoint every 500 rows"; return r }, DropSlashBang},
		{"slash after space", func() Raw { r := good(); r.Text = "  /compact checkpoint every 500 rows"; return r }, DropSlashBang},
		{"slash after zero-width", func() Raw { r := good(); r.Text = "​/model checkpoint every 500 rows"; return r }, DropSlashBang},
		{"full-width bang", func() Raw { r := good(); r.Text = "！ls checkpoint every 500 rows"; return r }, DropSlashBang},
		{"multiline", func() Raw { r := good(); r.Text = "Go with (a)\ncheckpoint every 500 rows"; return r }, DropMultiline},
		{"url", func() Raw { r := good(); r.Text = "Go with (a), see https://example.test/checkpoint"; return r }, DropURL},
		{"www", func() Raw { r := good(); r.Text = "Go with (a), checkpoint like www.example.test does"; return r }, DropURL},
		{"fence", func() Raw { r := good(); r.Text = "Go with (a) ```checkpoint every 500 rows```"; return r }, DropFence},
		{"secret in text", func() Raw { r := good(); r.Text = "Go with (a), checkpoint, API_KEY=hunter22secret"; return r }, DropSecret},
		{"secret in quote", func() Raw { r := good(); r.Quote = "token ghp_abcdefghijklmnop"; return r }, DropSecret},
		{"no cite", func() Raw { r := good(); r.Cites = nil; return r }, DropCite},
		{"unknown cite", func() Raw { r := good(); r.Cites = []string{"R", "A9"}; return r }, DropCite},
		{"quote not in source", func() Raw { r := good(); r.Quote = "checkpoint every 1000 rows"; return r }, DropQuote},
		{"quote too short", func() Raw { r := good(); r.Quote = "(a)"; return r }, DropQuote},
		{"quote in an uncited source", func() Raw {
			return Raw{Text: "Keep the batch size configurable too", Kind: KindGap, Cites: []string{"A1"}, Quote: "keep the batch size configurable"}
		}, DropQuote},
		{"unrelated to its quote", func() Raw { r := good(); r.Text = "Go with (a), and rename the module"; return r }, DropUnrelated},
		{"unknown term", func() Raw { r := good(); r.Text = "Go with (a), checkpoint every 500 rows with --force"; return r }, DropUnknownTerm},
		{"pick not citing R", func() Raw {
			return Raw{Text: "Pick the resumable export job option", Kind: KindPick, Cites: []string{"A1"}, Quote: "make the export job resumable"}
		}, DropPickNotR},
		{"decide not citing R", func() Raw {
			return Raw{Text: "Approve making the export job resumable, but only after tests", Kind: KindDecide, Cites: []string{"A1"}, Quote: "make the export job resumable"}
		}, DropDecideNotR},
		{"decide with no condition", func() Raw {
			return Raw{Text: "Yes, run the cleanup script", Kind: KindDecide, Cites: []string{"R"}, Quote: "run the cleanup script with --dry-run first"}
		}, DropNoCondition},
		{"gap citing only R", func() Raw {
			return Raw{Text: "The batch id re-run reads the source again", Kind: KindGap, Cites: []string{"R"}, Quote: "idempotent re-run keyed on batch id"}
		}, DropGapNoAsk},
		{"generic reply on an unrelated quote", func() Raw {
			return Raw{Text: "Looks good!", Kind: KindRedirect, Cites: []string{"R"}, Quote: "Which do you prefer?"}
		}, DropUnrelated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, dropped := Filter([]Raw{c.raw()}, b)
			if len(kept) != 0 || len(dropped) != 1 || dropped[0].Reason != c.want {
				t.Fatalf("kept %v, dropped %+v; want one drop for %q", kept, dropped, c.want)
			}
		})
	}
}

// TestTheStockPhraseBackstopDropsAGroundedGenericReply: a stock phrase that
// somehow passes the citation and relatedness checks is still dropped by the
// (heuristic) stock list.
func TestTheStockPhraseBackstopDropsAGroundedGenericReply(t *testing.T) {
	b, _ := Build(Input{Tail: []Entry{{Role: RoleAssistant, Text: "The migration ran. Continue with the rollout? Should I continue?"}}})
	kept, dropped := Filter([]Raw{{Text: "Continue", Kind: KindRedirect, Cites: []string{"R"}, Quote: "Should I continue?"}}, b)
	if len(kept) != 0 || len(dropped) != 1 || dropped[0].Reason != DropStock {
		t.Fatalf("kept %v dropped %+v, want the stock-phrase drop", kept, dropped)
	}
}

// TestAGroundedSetSurvivesWhole: the positive control — suggestions of each
// kind that are grounded and well-formed all survive, in order.
func TestAGroundedSetSurvivesWhole(t *testing.T) {
	b := filterInput(t)
	raw := []Raw{
		good(),
		{Text: "Also keep the batch size configurable, you skipped it", Kind: KindGap, Cites: []string{"A2"}, Quote: "keep the batch size configurable"},
		{Text: "Run the cleanup with --dry-run first, but don't delete anything yet", Kind: KindDecide, Cites: []string{"R"}, Quote: "run the cleanup script with --dry-run first"},
	}
	kept, dropped := Filter(raw, b)
	if len(kept) != 3 || len(dropped) != 0 {
		t.Fatalf("kept %v dropped %+v, want all three", kept, dropped)
	}
	for i, k := range kept {
		if k.Text != strings.TrimSpace(raw[i].Text) || k.Kind != raw[i].Kind {
			t.Fatalf("kept[%d] = %+v, want %+v", i, k, raw[i])
		}
	}
}

// TestAQuoteMatchesWithItsWhitespaceFolded: a quote the model re-flowed across
// a line break is still verbatim.
func TestAQuoteMatchesWithItsWhitespaceFolded(t *testing.T) {
	b := filterInput(t)
	r := good()
	r.Quote = "(a)   checkpoint\nevery 500 rows"
	if kept, d := Filter([]Raw{r}, b); len(kept) != 1 {
		t.Fatalf("dropped %+v", d)
	}
}

// TestDuplicatesAndOverflowAreDropped: the same text twice keeps one; more
// than MaxSuggestions keeps the first three.
func TestDuplicatesAndOverflowAreDropped(t *testing.T) {
	b := filterInput(t)
	g := good()
	dup := good()
	dup.Text = "go with (a) — checkpoint every 500 rows!"
	kept, dropped := Filter([]Raw{g, dup}, b)
	if len(kept) != 1 || len(dropped) != 1 || dropped[0].Reason != DropDuplicate {
		t.Fatalf("kept %v dropped %+v", kept, dropped)
	}
	var many []Raw
	for _, n := range []string{"one", "two", "three", "four"} {
		r := good()
		r.Text = "Go with (a), checkpoint every 500 rows, take " + n
		many = append(many, r)
	}
	kept, dropped = Filter(many, b)
	if len(kept) != MaxSuggestions || len(dropped) != 1 || dropped[0].Reason != DropOverflow || !strings.HasSuffix(kept[2].Text, "three") {
		t.Fatalf("kept %v dropped %+v", kept, dropped)
	}
}

// TestAGapRAlreadyCoversMovesLast: the ranking heuristic moves, never drops.
func TestAGapRAlreadyCoversMovesLast(t *testing.T) {
	b := filterInput(t)
	covered := Raw{Text: "Make the export job resumable as asked", Kind: KindGap, Cites: []string{"A1"}, Quote: "make the export job resumable"}
	kept, _ := Filter([]Raw{covered, good()}, b)
	// "export" and "resumable" — A1's content words — are not both in R, so
	// it is NOT covered and keeps its place; the reverse case below moves.
	if len(kept) != 2 || kept[0].Kind != KindGap {
		t.Fatalf("kept %v", kept)
	}
	b2, _ := Build(Input{Asks: []string{"fix the flaky retry test"}, Tail: []Entry{{Role: RoleAssistant, Text: "Fixed the flaky retry test, 20/20 green. Want me to open the PR?"}}})
	gap := Raw{Text: "Show the flaky retry test runs before the PR", Kind: KindGap, Cites: []string{"A1"}, Quote: "fix the flaky retry test"}
	verify := Raw{Text: "Show me the 20/20 output before the PR", Kind: KindVerify, Cites: []string{"R"}, Quote: "20/20 green"}
	kept, _ = Filter([]Raw{gap, verify}, b2)
	if len(kept) != 2 || kept[0].Kind != KindVerify || kept[1].Kind != KindGap {
		t.Fatalf("a gap R covers should move last: %v", kept)
	}
}
