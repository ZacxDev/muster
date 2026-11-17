package plugins

import (
	"errors"
	"strings"
	"testing"
)

// TestAChiefSectionMayNotCarryAnImport pins ValidChiefInstructions' rule: an
// `@` is refused unless the character right before it is a letter, a digit,
// or one of `.`, `_`, `-`, `+`. Claude Code expands an `@path` import in
// CLAUDE.md even inside the `> ` quote the section renders in, so each shape
// below that Claude Code could read as an import is refused with the import
// error; an address-like `a@b.c` and the same texts with no `@` are accepted
// (the controls, so a refusal is the `@`'s and not something else's).
func TestAChiefSectionMayNotCarryAnImport(t *testing.T) {
	importErr := "could read as a file import"
	for _, text := range []string{
		"@notes/x.md",                       // the whole text is one import
		"see @x",                            // after a space
		"Read the quill board: @x then act", // mid-line, after a space
		"First line.\n@x",                   // at the start of a line
		"tabbed\t@x",                        // after a tab
		"in parens (@x)",                    // after punctuation
		"in a span `@x`",                    // after a backtick
	} {
		err := ValidChiefInstructions(text)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), importErr) {
			t.Errorf("%q: %v; want refused as an import", text, err)
		}
		// Control: the same text without its `@` is accepted.
		if err := ValidChiefInstructions(strings.ReplaceAll(text, "@", "")); err != nil {
			t.Errorf("control %q without the @: %v; want accepted", text, err)
		}
	}
	for _, text := range []string{"a@b.c", "Write to ops.desk@example.test about it.", "tag v1.2-rc+3@build"} {
		if err := ValidChiefInstructions(text); err != nil {
			t.Errorf("%q: %v; want an address-like @ accepted", text, err)
		}
	}
}
