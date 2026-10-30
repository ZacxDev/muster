package ui

import (
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAgentHeaderMenuRowIsTheMenusHeight: the header's second row — the status
// strip, then the ⋯ — has the ⋯ trigger's height as its minimum, for both row
// shapes (a gateway's and a claude-code agent's). It centres its items, so a
// row sized by the strip alone until the ⋯ arrived moved the strip's status
// icon when it did (menuRowMin; e2e cls-long-transcript's stall after the
// strip is the behavioural case). With no card there is no ⋯ and no minimum.
func TestAgentHeaderMenuRowIsTheMenusHeight(t *testing.T) {
	for _, kind := range []string{agents.KindGateway, agents.KindClaudeCode} {
		t.Run(kind, func(t *testing.T) {
			v := sampleAgentDetailView()
			v.Kind = kind
			v.Card = AgentCardView{ID: v.ID, Name: v.Name, Status: "running"}
			var b strings.Builder
			if err := RenderAgentDetail(&b, v, featuresAllOn); err != nil {
				t.Fatal(err)
			}
			doc := parseFragment(t, b.String())
			trigger, slot, strip := byID(doc, "agent-actions-open"), byID(doc, AgentMenuSlot), byID(doc, AgentStripID)
			if trigger == nil || slot == nil || strip == nil {
				t.Fatalf("trigger %v, slot %v, strip %v", trigger, slot, strip)
			}
			if !hasClass(attrOf(trigger, "class"), "h-11") {
				t.Fatalf("the ⋯ trigger is not h-11 (the minimum's premise): %q", attrOf(trigger, "class"))
			}
			row := slot.Parent
			if strip.Parent != row {
				t.Fatal("the strip and the ⋯ are not in one row")
			}
			if c := attrOf(row, "class"); !hasClass(c, "items-center") || !hasClass(c, "min-h-11") {
				t.Errorf("the second row: %q, want items-center with min-h-11", c)
			}
		})
	}
	v := sampleAgentDetailView()
	var b strings.Builder
	if err := RenderAgentDetail(&b, v, featuresAllOn); err != nil {
		t.Fatal(err)
	}
	doc := parseFragment(t, b.String())
	if byID(doc, AgentMenuSlot) != nil {
		t.Fatal("control: a view with no card drew the ⋯")
	}
	rows := 0
	for _, n := range allAttr(doc, "class") {
		if c := attrOf(n, "class"); hasClass(c, "pl-11") && hasClass(c, "items-center") {
			rows++
			if hasClass(c, "min-h-11") {
				t.Error("a second row with no ⋯ is held at the ⋯'s height")
			}
		}
	}
	if rows != 1 {
		t.Fatalf("control: %d gateway second rows found, want 1", rows)
	}
}

// TestAgentHeaderToolsReserveTheirWidth: the first header row's controls right
// of the title sit in one box whose width class is the controls' own total —
// each h-11 w-11 (2.75rem), gap-3 (0.75rem) between — with and without focus
// history. A frame painted part-way through parsing the row then shows each
// control where it ends up (agentHeaderTools; e2e cls-long-transcript's stall
// after the new-chat button is the behavioural case).
func TestAgentHeaderToolsReserveTheirWidth(t *testing.T) {
	for _, tc := range []struct {
		name  string
		focus string
		want  []string // the controls' aria-labels, in order
	}{
		{"no focus history", "", []string{"New chat session", "Chat history"}},
		{"focus history", FocusHistoryPath("agent:42", ""), []string{"New chat session", "Chat history", "Focus history"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := sampleAgentDetailView()
			v.FocusHistory = tc.focus
			var b strings.Builder
			if err := RenderAgentDetail(&b, v, featuresAllOn); err != nil {
				t.Fatal(err)
			}
			boxes := allAttr(parseFragment(t, b.String()), "data-header-tools")
			if len(boxes) != 1 {
				t.Fatalf("%d header tool boxes, want 1", len(boxes))
			}
			box := boxes[0]
			if !insideTag(box, "header") {
				t.Fatal("the tools box is not in the page header")
			}
			// The controls: each direct child, or the button inside a
			// display:contents form (newChatButton's), which is the flex item.
			var labels []string
			for c := box.FirstChild; c != nil; c = c.NextSibling {
				if c.Type != html.ElementNode {
					continue
				}
				ctl := c
				if c.Data == "form" {
					if !hasClass(attrOf(c, "class"), "contents") {
						t.Fatalf("a form in the tools box is not display:contents, so it is the flex item: %q", attrOf(c, "class"))
					}
					ctl = nil
					for f := c.FirstChild; f != nil; f = f.NextSibling {
						if f.Type == html.ElementNode && f.Data == "button" {
							ctl = f
						}
					}
					if ctl == nil {
						t.Fatal("the new-chat form has no button")
					}
				}
				if !hasClass(attrOf(ctl, "class"), "h-11") || !hasClass(attrOf(ctl, "class"), "w-11") {
					t.Errorf("control %q is not h-11 w-11 (the width arithmetic's premise): %q", attrOf(ctl, "aria-label"), attrOf(ctl, "class"))
				}
				labels = append(labels, attrOf(ctl, "aria-label"))
			}
			if strings.Join(labels, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("controls %q, want %q", labels, tc.want)
			}
			if !hasClass(attrOf(box, "class"), "gap-3") || !hasClass(attrOf(box, "class"), "shrink-0") {
				t.Errorf("the tools box is not gap-3 shrink-0: %q", attrOf(box, "class"))
			}
			n := float64(len(labels))
			want := "w-[" + strconv.FormatFloat(n*2.75+(n-1)*0.75, 'f', -1, 64) + "rem]"
			if !hasClass(attrOf(box, "class"), want) {
				t.Errorf("the tools box's width class: %q, want %s", attrOf(box, "class"), want)
			}
		})
	}
}
