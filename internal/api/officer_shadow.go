package api

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/officers"
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
}

func shadowKey(officer string, item officers.ItemID) string { return officer + " " + string(item) }

// recordShadow records line against officer's item (an identical line is
// kept once).
func (rt *officerRuntime) recordShadow(officer string, item officers.ItemID, line string) {
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
