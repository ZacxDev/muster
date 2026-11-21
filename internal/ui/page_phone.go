package ui

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/pages"
)

// page_phone.go — the phone check page_create and page_update answer with: a
// note for each markdown-block shape that reads badly on a phone. It refuses
// nothing; it tells the agent what to change, naming the block.
//
// It lives HERE, beside the markdown grammar, because two of its rules ARE
// that grammar: what a table is (mdTableStartsAt, mdTableCells) and what a
// code span is (mdInlineCode). The third, which images a page draws, is
// internal/pages' own rule (pages.MarkdownImageDrawn), the one its renderer
// draws by. Each is read from the one place that owns it, so the check cannot
// call something a table the renderer does not draw as one.

const (
	// PhoneMaxTableColumns is the most columns a markdown table has before the
	// check says it is cramped on a phone.
	PhoneMaxTableColumns = 5
	// PhoneMaxCodeSpan is the longest inline code span, in characters, before
	// the check says it is hard to read on a phone.
	PhoneMaxCodeSpan = 60
)

// mdImageSyntax is any markdown image as a writer spells it: what was meant
// as an image, drawn or not.
var mdImageSyntax = regexp.MustCompile(`!\[[^\]\n]*\]\([^)\n]*\)`)

// PagePhoneNotes is the phone check of a version's blocks: one note per
// markdown block and rule it breaks, in block order, each naming the block
// (blocks[i], 0-based, as the call's blocks array) and what to change. nil
// when there is nothing to say.
//
// The rules: a table with more than PhoneMaxTableColumns columns; an inline
// code span longer than PhoneMaxCodeSpan characters; a markdown image the
// page cannot draw (pages.MarkdownImageDrawn: an http(s) address, an SVG or
// other data: type, malformed base64...). A fenced code block is not looked
// in (the renderer shows it as code), nor is a code span for images (its
// text is shown as written, on purpose).
func PagePhoneNotes(blocks []pages.Block) []string {
	var notes []string
	for i, b := range blocks {
		if b.Type != pages.BlockMarkdown {
			continue
		}
		at := "blocks[" + strconv.Itoa(i) + "] (markdown): "
		tables, spans, images := phoneScan(b.Text)
		for _, n := range tables {
			notes = append(notes, at+"a table with "+strconv.Itoa(n)+" columns is cramped on a phone (it scrolls sideways). "+
				"Keep a table to "+strconv.Itoa(PhoneMaxTableColumns)+" columns or fewer: split it, or make the extra columns a list under a heading per row.")
		}
		for _, n := range spans {
			notes = append(notes, at+"an inline code span of "+strconv.Itoa(n)+" characters is hard to read on a phone. "+
				"Keep inline code to "+strconv.Itoa(PhoneMaxCodeSpan)+" characters or fewer: put a long command or URL in a fenced code block, or link it once.")
		}
		for _, img := range images {
			notes = append(notes, at+"the image "+phoneQuote(img)+" is shown as text, not drawn. "+
				"In markdown write ![what it shows](asset:<id>) (page_asset_upload) or a small data:image/png, jpeg, webp or gif base64 URI; "+
				"an http(s) address or an SVG is never drawn.")
		}
	}
	return notes
}

// phoneScan walks src as markdownRender does, far enough to find each
// table's column count, each over-long code span's length and each image the
// page cannot draw. Fenced code is skipped; a table's body rows are consumed
// as the renderer consumes them.
func phoneScan(src string) (tables, spans []int, images []string) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); {
		line := lines[i]
		if mdFenceRe.MatchString(line) {
			for i++; i < len(lines) && !mdFenceEndRe.MatchString(lines[i]); i++ {
			}
			i++
			continue
		}
		isList := mdBulletRe.MatchString(line) || mdOrderedRe.MatchString(line)
		other := mdHeadingRe.MatchString(line) || mdBlockquote.MatchString(line) || mdThematicBreak.MatchString(line)
		if !isList && !other && mdTableStartsAt(lines, i) {
			if n := len(mdTableCells(line)); n > PhoneMaxTableColumns {
				tables = append(tables, n)
			}
			rows := []string{line}
			for i += 2; i < len(lines) && strings.Contains(lines[i], "|") && !mdBlankLineRe.MatchString(lines[i]); i++ {
				rows = append(rows, lines[i])
			}
			for _, r := range rows {
				for _, c := range mdTableCells(r) {
					spans, images = phoneInline(c, spans, images)
				}
			}
			continue
		}
		spans, images = phoneInline(line, spans, images)
		i++
	}
	return tables, spans, images
}

// phoneInline adds one line's over-long code spans and undrawable images.
func phoneInline(line string, spans []int, images []string) ([]int, []string) {
	for _, m := range mdInlineCode.FindAllStringSubmatch(line, -1) {
		if n := utf8.RuneCountInString(m[1]); n > PhoneMaxCodeSpan {
			spans = append(spans, n)
		}
	}
	for _, m := range mdImageSyntax.FindAllString(mdInlineCode.ReplaceAllString(line, ""), -1) {
		if !pages.MarkdownImageDrawn(m) {
			images = append(images, m)
		}
	}
	return spans, images
}

// phoneQuote is an image's syntax for a note: itself, or its start when long
// (a data: URI is kilobytes).
func phoneQuote(s string) string {
	const max = 60
	if utf8.RuneCountInString(s) <= max {
		return "`" + s + "`"
	}
	r := []rune(s)
	return "`" + string(r[:max]) + "…`"
}
