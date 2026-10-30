package ui

import (
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

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
