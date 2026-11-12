package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officership"
)

// ---------------------------------------------------------------------------
// THE SHEPHERD'S SHADOW LEDGER (docs/officers.md "Shadow").
//
// A shepherd write HQ runs in shadow is refused (officership's
// shadowRefusal) and its "would have …" line recorded here, per (officer,
// item). When an instance completes that item through officer_complete, the
// lines are appended to the summary HQ receives, so the item's record says
// what the loop would have done. In memory only: a restart forgets them.
// ---------------------------------------------------------------------------

// shadowHeader opens the block appended to a completion summary.
const shadowHeader = "Shadow (muster refused these writes):"

// maxShadowLines bounds one item's recorded lines; later ones are counted.
const maxShadowLines = 50

type shadowLedger struct {
	lines   []string
	dropped int
	// pushRefused is set once a PUSH under the item was refused in shadow:
	// the head a hand-off would name was never pushed (pushRefusedInShadow).
	pushRefused bool
	// landed is set once a push under the item (a fix item) LANDED: GitHub's
	// report applied it (officership's FixPushed). fixHandoffRefusal.
	landed bool
}

func shadowKey(officer string, item officers.ItemID) string { return officer + " " + string(item) }

// recordShadow records line against officer's item (an identical line is
// kept once); push marks it a refused push.
func (rt *officerRuntime) recordShadow(officer string, item officers.ItemID, line string, push bool) {
	rt.shadowMu.Lock()
	defer rt.shadowMu.Unlock()
	if rt.shadow == nil {
		rt.shadow = map[string]*shadowLedger{}
	}
	k := shadowKey(officer, item)
	l := rt.shadow[k]
	if l == nil {
		l = &shadowLedger{}
		rt.shadow[k] = l
	}
	if push {
		l.pushRefused = true
	}
	for _, have := range l.lines {
		if have == line {
			return
		}
	}
	if len(l.lines) >= maxShadowLines {
		l.dropped++
		return
	}
	l.lines = append(l.lines, line)
}

// shadowSummary is summary with officer's item's shadow block appended, at
// most max runes in all: the block is cut (whole lines, then a count of the
// rest), never the instance's own text. No lines: summary unchanged.
func (rt *officerRuntime) shadowSummary(officer string, item officers.ItemID, summary string, max int) string {
	rt.shadowMu.Lock()
	l := rt.shadow[shadowKey(officer, item)]
	var lines []string
	dropped := 0
	if l != nil {
		lines, dropped = append([]string(nil), l.lines...), l.dropped
	}
	rt.shadowMu.Unlock()
	return withShadowBlock(summary, lines, dropped, max)
}

// recordFixPush records that a push under officer's fix item landed.
func (rt *officerRuntime) recordFixPush(officer string, item officers.ItemID) {
	rt.shadowMu.Lock()
	defer rt.shadowMu.Unlock()
	if rt.shadow == nil {
		rt.shadow = map[string]*shadowLedger{}
	}
	k := shadowKey(officer, item)
	if rt.shadow[k] == nil {
		rt.shadow[k] = &shadowLedger{}
	}
	rt.shadow[k].landed = true
}

// fixPushLanded reports whether a push under officer's item landed, until the
// item's completion forgets it.
func (rt *officerRuntime) fixPushLanded(officer string, item officers.ItemID) bool {
	rt.shadowMu.Lock()
	defer rt.shadowMu.Unlock()
	l := rt.shadow[shadowKey(officer, item)]
	return l != nil && l.landed
}

// pushRefusedInShadow reports whether a push under officer's item was refused
// in shadow (recordShadow with push), until the item's completion forgets it.
func (rt *officerRuntime) pushRefusedInShadow(officer string, item officers.ItemID) bool {
	rt.shadowMu.Lock()
	defer rt.shadowMu.Unlock()
	l := rt.shadow[shadowKey(officer, item)]
	return l != nil && l.pushRefused
}

// forgetShadow drops officer's item's lines (once its completion carried them).
func (rt *officerRuntime) forgetShadow(officer string, item officers.ItemID) {
	rt.shadowMu.Lock()
	defer rt.shadowMu.Unlock()
	delete(rt.shadow, shadowKey(officer, item))
}

// withShadowBlock is shadowSummary's text rule.
func withShadowBlock(summary string, lines []string, dropped, max int) string {
	if len(lines) == 0 {
		return summary
	}
	room := max - utf8.RuneCountInString(summary)
	head := "\n\n" + shadowHeader
	if utf8.RuneCountInString(head) > room {
		return summary
	}
	var b strings.Builder
	b.WriteString(head)
	used := utf8.RuneCountInString(head)
	more := func(n int) string { return "\n- … and " + strconv.Itoa(n) + " more" }
	for i, ln := range lines {
		add := "\n- " + ln
		rest := len(lines) - i - 1 + dropped
		need := utf8.RuneCountInString(add)
		if rest > 0 {
			need += utf8.RuneCountInString(more(rest))
		}
		if used+need > room {
			if m := more(len(lines) - i + dropped); used+utf8.RuneCountInString(m) <= room {
				b.WriteString(m)
			}
			return summary + b.String()
		}
		b.WriteString(add)
		used += utf8.RuneCountInString(add)
	}
	if dropped > 0 {
		if m := more(dropped); used+utf8.RuneCountInString(m) <= room {
			b.WriteString(m)
		}
	}
	return summary + b.String()
}

// fixHandoffRefusal is why a draft officer's officer_complete WITH a hand-off
// of item is refused, or "": HQ refuses a fix item's hand-off at an unchanged
// head, so when item is a *.pr_fix item (read from HQ with the officer's
// token) and no push under it LANDED during the hold (recordFixPush), muster
// refuses first. Any other item, and any other stage, is "". 🔴 HQ's items
// unreadable is a refusal (fail closed).
func (s *Server) fixHandoffRefusal(ctx context.Context, o officers.Officer, c *officers.Client, instance string, item officers.ItemID) string {
	if !o.Ship.IsDraft() || s.officerRT.fixPushLanded(o.ID, item) {
		return ""
	}
	items, err := c.AllItems(ctx, officers.StatusClaimed)
	if err != nil {
		return fmt.Sprintf("muster could not read HQ's items to check whether item %s is a fix item with a pushed fix (%v); try again", item, err)
	}
	for _, it := range items {
		if it.ID != item || !officership.IsPRFix(it.Kind) {
			continue
		}
		branch := "the branch of " + it.SubjectKey
		if repo, n, ok := strings.Cut(it.SubjectKey, "#"); ok {
			if rt := s.officerRT.ships[o.ID]; rt != nil && rt.gate != nil {
				if gh := rt.gate.GHFor[strings.ToLower(repo)]; gh != nil {
					if num, err := strconv.Atoi(n); err == nil {
						if pr, err := gh.PR(ctx, num); err == nil && pr.HeadRef != "" {
							branch = pr.HeadRef
						}
					}
				}
			}
		}
		return fmt.Sprintf("no push landed on %s while holding item %s: HQ refuses a fix's hand-off at an unchanged head. Push the fix, "+
			"then complete done with the handoff — or complete escalated, or without a handoff", branch, item)
	}
	return ""
}
