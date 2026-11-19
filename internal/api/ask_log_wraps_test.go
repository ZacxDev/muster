package api

import (
	"strings"
	"testing"
	"time"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/ZacxDev/muster/internal/asklog"
)

// HOW THE LOG TAB'S SECONDARY LINES WRAP AND CUT, pinned on the STRUCTURE of
// the HTML (which element holds what, with which classes), not on its words:
// a phone screenshot at 390×844 (#832) showed a facts line ending in a
// dangling "·", and a row's sub line cut its age off behind a long place. The
// layout itself is e2e/tests/asks-log.spec.ts's; these say the markup that
// layout depends on is there. (No identifier origin/main lacks, so this file
// compiles there for the red run.)

// wrapParse parses s as body content.
func wrapParse(t *testing.T, s string) *xhtml.Node {
	t.Helper()
	body := &xhtml.Node{Type: xhtml.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := xhtml.ParseFragment(strings.NewReader(s), body)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		body.AppendChild(n)
	}
	return body
}

func wrapAttr(n *xhtml.Node, k string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val, true
		}
	}
	return "", false
}

func wrapHasClass(n *xhtml.Node, c string) bool {
	v, _ := wrapAttr(n, "class")
	for _, f := range strings.Fields(v) {
		if f == c {
			return true
		}
	}
	return false
}

// wrapFindAll is every element under n carrying attribute k.
func wrapFindAll(n *xhtml.Node, k string) []*xhtml.Node {
	var out []*xhtml.Node
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			if _, ok := wrapAttr(n, k); ok {
				out = append(out, n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func wrapText(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// checkSeparatorsTravelWithTheNextFact: line's direct children are its units
// — elements, joined by whitespace-only text (the line's only break
// opportunities). Every "·" is the FIRST character of a no-wrap unit (so a
// wrap moves it to the start of the next line with its fact, never leaves it
// ending one), and that unit holds a fact after it. firstWhole: the first
// unit is no-wrap too.
func checkSeparatorsTravelWithTheNextFact(t *testing.T, what string, line *xhtml.Node, wantUnits int, firstWhole bool) {
	t.Helper()
	units := 0
	for c := line.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case xhtml.TextNode:
			if strings.TrimSpace(c.Data) != "" {
				t.Errorf("%s: text %q sits between its units, outside any of them (a separator there can end a line)", what, c.Data)
			}
			continue
		case xhtml.ElementNode:
		default:
			continue
		}
		txt := wrapText(c)
		if units == 0 {
			if strings.Contains(txt, "·") {
				t.Errorf("%s: the first unit %q holds a separator", what, txt)
			}
			if firstWhole && !wrapHasClass(c, "whitespace-nowrap") {
				t.Errorf("%s: the first fact %q is not kept whole", what, txt)
			}
		} else {
			if !strings.HasPrefix(txt, "· ") || strings.TrimSpace(strings.TrimPrefix(txt, "· ")) == "" || strings.Count(txt, "·") != 1 {
				t.Errorf("%s: unit %d is %q, want one \"· fact\" (the separator first, with its fact)", what, units, txt)
			}
			if !wrapHasClass(c, "whitespace-nowrap") {
				t.Errorf("%s: unit %d %q can wrap inside itself (its separator could end a line)", what, units, txt)
			}
		}
		units++
	}
	if units != wantUnits {
		t.Errorf("%s: %d units, want %d:\n%s", what, units, wantUnits, wrapText(line))
	}
}

// TestAFactsSeparatorTravelsWithTheFactAfterIt: on a repeated question's
// facts line, and on a row's answer line, a "·" is never the last thing on a
// line: each one leads the no-wrap unit of the fact after it. RED at
// origin/main 585e8042: the facts were no-wrap units joined by a bare " · ",
// so a wrap between them left the "·" ending the line ("3 times · you
// answered 2 · always “continue” ·" / "median 11s" on a phone).
func TestAFactsSeparatorTravelsWithTheFactAfterIt(t *testing.T) {
	sum := asklog.Summary{Total: 9, Top: []asklog.TopQuestion{
		{Norm: "keep going", Question: "Keep going?", Count: 3, ByOwner: 2, TopAnswer: "continue", TopAnswerCount: 3, MedianAnswer: 11 * time.Second},
	}}
	doc := wrapParse(t, askLogSummaryHTML(sum, asklog.DefaultTextTTL))
	facts := wrapFindAll(doc, "data-asklog-top-facts")
	if len(facts) != 1 {
		t.Fatalf("%d facts lines, want 1", len(facts))
	}
	if got := wrapText(facts[0]); got != "3 times · you answered 2 · always “continue” · median 11s" {
		t.Errorf("facts read %q", got)
	}
	checkSeparatorsTravelWithTheNextFact(t, "top facts", facts[0], 4, true)

	// A row's answer line: the quoted answer (free to wrap: it may be long),
	// "· by you", "· after 7s".
	ask := asklog.Ask{ID: 41, Kind: asklog.KindPlainText, Host: "work-laptop", TmuxSession: "ratelimit", Repo: "acme/api",
		Question: "Go on?", Answer: "yes", AnsweredBy: asklog.ByOwner, Outcome: asklog.OutcomeAnswered,
		AskedAt: glanceNow.Add(-15 * time.Minute), AnsweredAt: glanceNow.Add(-15*time.Minute + 7*time.Second)}
	card := wrapParse(t, askLogCardHTML(ask, glanceNow, asklog.DefaultTextTTL))
	ans := wrapFindAll(card, "data-asklog-answer")
	if len(ans) != 1 {
		t.Fatalf("%d answer lines, want 1", len(ans))
	}
	if got := wrapText(ans[0]); got != "“yes” · by you · after 7s" {
		t.Errorf("answer line reads %q", got)
	}
	checkSeparatorsTravelWithTheNextFact(t, "answer", ans[0], 3, false)
}

// TestALogRowsAgeIsNeverCut: a row's sub line is the place, which gives way
// (min-w-0 truncate: its ellipsis inside it), then the age — a separate,
// non-shrinking, non-wrapping element OUTSIDE the truncated one, carrying its
// own "·" — so a long place never hides when it was asked. RED at
// origin/main 585e8042: one truncated span held the place AND the <time>,
// age last, so a long place cut the age ("ratelimit @ work-laptop ·
// acme/api · …").
func TestALogRowsAgeIsNeverCut(t *testing.T) {
	for _, c := range []struct {
		name      string
		ask       asklog.Ask
		where     string
		age       string
		separated bool
	}{
		{"host, session and repo", asklog.Ask{ID: 7, Kind: asklog.KindPlainText, Host: "work-laptop", TmuxSession: "ratelimit", Repo: "acme/api",
			AskedAt: glanceNow.Add(-15 * time.Minute)}, "ratelimit @ work-laptop · acme/api", "· 15m ago", true},
		{"nowhere", asklog.Ask{ID: 8, Kind: asklog.KindAttention, AskedAt: glanceNow.Add(-3 * time.Hour)}, "", "3h ago", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := wrapParse(t, askLogCardHTML(c.ask, glanceNow, asklog.DefaultTextTTL))
			var times []*xhtml.Node
			var walk func(*xhtml.Node)
			walk = func(n *xhtml.Node) {
				if n.Type == xhtml.ElementNode && n.Data == "time" {
					times = append(times, n)
				}
				for k := n.FirstChild; k != nil; k = k.NextSibling {
					walk(k)
				}
			}
			walk(doc)
			if len(times) != 1 {
				t.Fatalf("%d <time> elements, want 1", len(times))
			}
			// The age's own element: the <time>'s nearest ancestor that does
			// not shrink and does not wrap. Nothing between it and the row
			// truncates.
			var age *xhtml.Node
			for p := times[0]; p != nil; p = p.Parent {
				if wrapHasClass(p, "truncate") || wrapHasClass(p, "overflow-hidden") || wrapHasClass(p, "text-ellipsis") {
					t.Fatalf("the age sits inside a truncated element (class %q): a long place cuts it off", func() string { v, _ := wrapAttr(p, "class"); return v }())
				}
				if age == nil && wrapHasClass(p, "shrink-0") && wrapHasClass(p, "whitespace-nowrap") {
					age = p
				}
				if _, ok := wrapAttr(p, "data-asklog-row"); ok {
					break
				}
			}
			if age == nil {
				t.Fatal("the age is in no non-shrinking, non-wrapping element")
			}
			if got := wrapText(age); got != c.age {
				t.Errorf("the age element reads %q, want %q", got, c.age)
			}
			line := age.Parent
			if line == nil || !wrapHasClass(line, "flex") || !wrapHasClass(line, "min-w-0") {
				t.Fatalf("the age's line is not a flex row that may shrink (min-w-0)")
			}
			// The place: the age's sibling BEFORE it (the order is kept), the
			// element that gives way.
			var where *xhtml.Node
			for s := age.PrevSibling; s != nil; s = s.PrevSibling {
				if s.Type == xhtml.ElementNode {
					where = s
					break
				}
				if strings.TrimSpace(s.Data) != "" {
					t.Errorf("text %q sits loose in the sub line", s.Data)
				}
			}
			if !c.separated {
				if where != nil {
					t.Errorf("a row asked nowhere draws a place %q", wrapText(where))
				}
				return
			}
			if where == nil {
				t.Fatal("no place before the age")
			}
			if !wrapHasClass(where, "truncate") || !wrapHasClass(where, "min-w-0") {
				t.Errorf("the place does not give way (want min-w-0 truncate)")
			}
			if got := wrapText(where); got != c.where {
				t.Errorf("the place reads %q, want %q", got, c.where)
			}
		})
	}
}
