// Package termscreen reads a tmux pane capture WITH its styles
// (`tmux capture-pane -p -e`) into rows of cells. It is the one SGR reader:
// hermeshost reads Hermes's input box by italic and background, ccdialog
// reads Claude Code's ghost suggestion by dim.
//
// 🔴 A LEAF: it imports only the standard library. cmd/ccd links it (through
// ccdialog) into the agent pod, which internal/modulegate holds to no
// network or storage client.
package termscreen

import (
	"strconv"
	"strings"
)

// Cell is one character of a captured screen, whether it is italic, whether
// it is dim (faint, SGR 2: Claude Code draws its ghost prompt suggestion so —
// ccdialog.InputBox reads it), and whether it has a background colour (Bg). A draft typed in Hermes's input
// box is drawn in the input's style, which has none at the pin (and an escape
// sequence typed or pasted into the box is drawn as text, `^[`, never
// obeyed), so a background is something a draft cannot draw.
type Cell struct {
	R      rune
	Italic bool
	Dim    bool
	Bg     bool
}

// Screen is a pane as `tmux capture-pane -p -e` prints it: its rows, top to
// bottom, as cells.
type Screen struct {
	Rows [][]Cell
}

// ParseScreen reads a `capture-pane -p -e` capture. Of SGR only italic (3
// on; 23, 0 or an empty SGR off), dim (2 on; 22, 0 or an empty SGR off) and whether a background colour is set (40-47,
// 100-107, 48;…; 49, 0 or an empty SGR off) are kept; every other escape
// sequence is dropped. A sequence it cannot read ends at the next letter,
// never later.
func ParseScreen(capture string) Screen {
	var s Screen
	// 🔴 THE STYLE IS CARRIED ACROSS ROWS: tmux's capture states an
	// attribute where it CHANGES (cmd-capture-pane.c keeps its last cell
	// across lines), so a row that continues an italic run starts with no
	// escape at all.
	var st style
	for _, line := range strings.Split(strings.TrimRight(capture, "\n"), "\n") {
		var row []Cell
		row, st = parseRow(line, st)
		s.Rows = append(s.Rows, row)
	}
	return s
}

// style is the SGR state ParseScreen keeps.
type style struct{ italic, dim, bg bool }

// parseRow is one captured line's cells, starting with st and returning the
// style it ends with.
func parseRow(line string, st style) ([]Cell, style) {
	var row []Cell
	rs := []rune(strings.TrimRight(line, "\r"))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r != 0x1b {
			if r == '\t' {
				r = ' '
			}
			row = append(row, Cell{R: r, Italic: st.italic, Dim: st.dim, Bg: st.bg})
			continue
		}
		if i+1 >= len(rs) {
			break
		}
		switch rs[i+1] {
		case '[':
			j := i + 2
			for j < len(rs) && !(rs[j] >= 0x40 && rs[j] <= 0x7e) {
				j++
			}
			if j < len(rs) && rs[j] == 'm' {
				st = sgrStyle(string(rs[i+2:j]), st)
			}
			i = j
		case ']':
			// OSC (a hyperlink): to BEL or ST.
			j := i + 2
			for j < len(rs) && rs[j] != 0x07 && !(rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\') {
				j++
			}
			if j < len(rs) && rs[j] == 0x1b {
				j++
			}
			i = j
		default:
			i++
		}
	}
	return row, st
}

// sgrStyle applies one SGR's parameters to st.
func sgrStyle(params string, st style) style {
	if params == "" {
		return style{}
	}
	ps := strings.Split(params, ";")
	for k := 0; k < len(ps); k++ {
		n, err := strconv.Atoi(ps[k])
		if err != nil {
			continue
		}
		switch {
		case n == 0:
			st = style{}
		case n == 23:
			st.italic = false
		case n == 3:
			st.italic = true
		case n == 22:
			st.dim = false
		case n == 2:
			st.dim = true
		case n == 49:
			st.bg = false
		case n >= 40 && n <= 47, n >= 100 && n <= 107:
			st.bg = true
		case n == 38, n == 48, n == 58:
			if n == 48 {
				st.bg = true
			}
			// An extended colour: skip its arguments (5;n or 2;r;g;b).
			if k+1 < len(ps) {
				if ps[k+1] == "5" {
					k += 2
				} else if ps[k+1] == "2" {
					k += 4
				}
			}
		}
	}
	return st
}
