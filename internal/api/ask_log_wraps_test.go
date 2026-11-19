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

// wrapClass is n's class attribute.
func wrapClass(n *xhtml.Node) string {
	v, _ := wrapAttr(n, "class")
	return v
}

// wrapElems is n's element children, in document order; it fails on any
// non-blank text between them (text loose in a flex row is a part of no
// item).
func wrapElems(t *testing.T, what string, n *xhtml.Node) []*xhtml.Node {
	t.Helper()
	var out []*xhtml.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == xhtml.ElementNode:
			out = append(out, c)
		case c.Type == xhtml.TextNode && strings.TrimSpace(c.Data) != "":
			t.Errorf("%s: text %q sits loose between its parts", what, c.Data)
		}
	}
	return out
}

// wrapWantClasses fails for each of classes n lacks.
func wrapWantClasses(t *testing.T, what string, n *xhtml.Node, classes ...string) {
	t.Helper()
	for _, c := range classes {
		if !wrapHasClass(n, c) {
			t.Errorf("%s lacks class %q (has %q)", what, c, wrapClass(n))
		}
	}
}

// TestALogRowsAgeIsNeverCut: a row's age is its own non-shrinking,
// non-wrapping element carrying its own "·", never inside an element that
// truncates; the one element that clips it (the sub line's tail, which also
// holds the repo) lays it out FIRST (order-first, so it is never the item
// that wraps onto the clipped line) and never narrows past its min-content
// (min-w-min) — so a long place never hides when it was asked. RED at
// origin/main 585e8042: one truncated span held the place AND the <time>,
// age last, so a long place cut the age ("ratelimit @ work-laptop ·
// acme/api · …").
func TestALogRowsAgeIsNeverCut(t *testing.T) {
	for _, c := range []struct {
		name string
		ask  asklog.Ask
		age  string
	}{
		{"host, session and repo", asklog.Ask{ID: 7, Kind: asklog.KindPlainText, Host: "work-laptop", TmuxSession: "ratelimit", Repo: "acme/api",
			AskedAt: glanceNow.Add(-15 * time.Minute)}, "· 15m ago"},
		{"nowhere", asklog.Ask{ID: 8, Kind: asklog.KindAttention, AskedAt: glanceNow.Add(-3 * time.Hour)}, "3h ago"},
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
			// truncates; the one ancestor that clips is the tail.
			var age *xhtml.Node
			for p := times[0]; p != nil; p = p.Parent {
				if wrapHasClass(p, "truncate") || wrapHasClass(p, "text-ellipsis") {
					t.Fatalf("the age sits inside a truncated element (class %q): a long place cuts it off", wrapClass(p))
				}
				if wrapHasClass(p, "overflow-hidden") {
					if _, ok := wrapAttr(p, "data-asklog-tail"); !ok || age == nil || age.Parent != p {
						t.Fatalf("the age sits inside a clipping element (class %q) that is not its own tail", wrapClass(p))
					}
					wrapWantClasses(t, "the tail clipping the age", p, "min-w-min")
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
			// First on its line, whatever its place in the markup.
			wrapWantClasses(t, "the age", age, "order-first")
		})
	}
}

// TestAskLogPlaceNeverCutToAStub: a row's sub line is the session @ host
// part, then a TAIL holding the repo and the age; the repo is its own
// element, with a flex basis of the room six readable characters need, in a
// one-line tail that wraps and clips — so a repo with less room than that is
// not drawn at all, never drawn as a stub ("· a…"). The full place is the
// sub line's title. The layout is e2e/tests/asks-log.spec.ts's ("no place is
// cut to a stub"); this pins the markup it depends on. RED at origin/main
// 8ac45385: the place was ONE truncated span, "ratelimit @ work-laptop ·
// acme-corporation/…", so a phone cut its repo to "· a…" — there was no repo
// element, no tail and no title.
func TestAskLogPlaceNeverCutToAStub(t *testing.T) {
	const repo = "acme-corporation/api-gateway-monorepo"
	for _, c := range []struct {
		name            string
		ask             asklog.Ask
		title, who, rep string
		age             string
	}{
		{"session, host and repo", asklog.Ask{ID: 7, Kind: asklog.KindPlainText, Host: "work-laptop", TmuxSession: "ratelimit", Repo: repo,
			AskedAt: glanceNow.Add(-2 * time.Hour)}, "ratelimit on work-laptop · " + repo, "ratelimit on work-laptop", "· " + repo, "· 2h ago"},
		{"host and repo", asklog.Ask{ID: 9, Kind: asklog.KindPlainText, Host: "studio-mac", Repo: "acme/api",
			AskedAt: glanceNow.Add(-20 * time.Hour)}, "on studio-mac · acme/api", "on studio-mac", "· acme/api", "· 20h ago"},
		{"agent, no repo", asklog.Ask{ID: 10, Kind: asklog.KindAttention, AgentID: 42,
			AskedAt: glanceNow.Add(-15 * time.Minute)}, "agent 42", "agent 42", "", "· 15m ago"},
		{"repo only", asklog.Ask{ID: 11, Kind: asklog.KindAttention, Repo: "acme/api",
			AskedAt: glanceNow.Add(-15 * time.Minute)}, "acme/api", "", "acme/api", "· 15m ago"},
		{"nowhere", asklog.Ask{ID: 12, Kind: asklog.KindAttention, AskedAt: glanceNow.Add(-3 * time.Hour)}, "", "", "", "3h ago"},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := wrapParse(t, askLogCardHTML(c.ask, glanceNow, asklog.DefaultTextTTL))
			subs := wrapFindAll(doc, "data-asklog-sub")
			if len(subs) != 1 {
				t.Fatalf("%d sub lines, want 1", len(subs))
			}
			sub := subs[0]
			// The full place is there to read, whatever the line shows.
			title, ok := wrapAttr(sub, "title")
			if c.title == "" {
				if ok {
					t.Errorf("a row asked nowhere has a place title %q", title)
				}
			} else if title != c.title {
				t.Errorf("the sub line's title is %q (present %v), want the full place %q", title, ok, c.title)
			}
			wrapWantClasses(t, "the sub line", sub, "flex", "min-w-0")
			parts := wrapElems(t, "the sub line", sub)
			// The session-and-computer part, when there is one, leads and truncates.
			if c.who != "" {
				if len(parts) == 0 {
					t.Fatal("the sub line is empty")
				}
				if _, ok := wrapAttr(parts[0], "data-asklog-who"); !ok {
					t.Fatalf("the sub line's first part is not the session-and-computer part: %q", wrapText(parts[0]))
				}
				if got := wrapText(parts[0]); got != c.who {
					t.Errorf("the session-and-computer part reads %q, want %q", got, c.who)
				}
				wrapWantClasses(t, "the session-and-computer part", parts[0], "min-w-0", "truncate")
				parts = parts[1:]
			}
			if len(parts) != 1 {
				t.Fatalf("after the session-and-computer part, %d parts, want 1 (the tail)", len(parts))
			}
			tail := parts[0]
			if _, ok := wrapAttr(tail, "data-asklog-tail"); !ok {
				t.Fatalf("the sub line's last part is not the tail: %q", wrapText(tail))
			}
			// One line that wraps and clips its second; drawn right-to-left and
			// packed to its left, it reads "· repo · age" with no gap; it takes
			// the rest of the line and never narrows past the age.
			wrapWantClasses(t, "the tail", tail, "flex", "h-4", "flex-wrap", "overflow-hidden", "flex-row-reverse", "justify-end", "content-start", "grow", "min-w-min")
			items := wrapElems(t, "the tail", tail)
			want := 1
			if c.rep != "" {
				want = 2
			}
			if len(items) != want {
				t.Fatalf("the tail has %d items, want %d", len(items), want)
			}
			age := items[len(items)-1]
			if _, ok := wrapAttr(age, "data-asklog-age"); !ok {
				t.Fatalf("the tail's last item is not the age: %q", wrapText(age))
			}
			if got := wrapText(age); got != c.age {
				t.Errorf("the age reads %q, want %q", got, c.age)
			}
			if c.rep == "" {
				if r := wrapFindAll(sub, "data-asklog-repo"); len(r) != 0 {
					t.Errorf("a row with no repo draws one: %q", wrapText(r[0]))
				}
				return
			}
			r := items[0]
			if _, ok := wrapAttr(r, "data-asklog-repo"); !ok {
				t.Fatalf("the tail's first item is not the repo: %q", wrapText(r))
			}
			if got := wrapText(r); got != c.rep {
				t.Errorf("the repo reads %q, want %q", got, c.rep)
			}
			// 🔴 Its basis is the room a readable part needs; with less it
			// wraps onto the clipped line. It grows to its own width and no
			// further, and holds the tail no wider than the age (w-0).
			wrapWantClasses(t, "the repo", r, "basis-[12ch]", "grow", "max-w-max", "w-0", "min-w-0", "truncate")
			// The line reads as the place: the parts' text, in order.
			if got := strings.Join(strings.Fields(wrapText(sub)), " "); got != c.title+" "+c.age {
				t.Errorf("the sub line reads %q, want %q", got, c.title+" "+c.age)
			}
		})
	}
}
