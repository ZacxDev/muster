package plugins

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// The `app` capability (docs/design/meetings.md, "The core change: the `app`
// capability"; migration 0168): a plugin with a tab in its owner's sidebar.
// The tab's label and icon are the OWNER's, chosen on /plugins at mint.

const (
	// MaxAppLabelRunes is the longest tab label, in characters.
	MaxAppLabelRunes = 24
	// MaxAppPlugins is how many live `app` plugins one owner may have, so the
	// sidebar cannot be flooded: a sixth is refused at mint.
	MaxAppPlugins = 5
)

// AppIcon is one entry of the fixed icon list: the KEY is what a plugin row
// stores (plugins.app_icon), the emoji is what the sidebar draws.
type AppIcon struct {
	Key, Emoji, Name string
}

// AppIcons is the whole list the /plugins form offers, in its order. A fixed
// list, so muster never validates an arbitrary grapheme cluster, and the
// sidebar keeps its one-emoji-per-tab style. Keys are lower-case letters
// (0168's plugins_app_tab).
var AppIcons = []AppIcon{
	{"video", "📹", "video camera"},
	{"mic", "🎙️", "microphone"},
	{"notes", "🗒️", "notepad"},
	{"clipboard", "📋", "clipboard"},
	{"puzzle", "🧩", "puzzle piece"},
	{"calendar", "📅", "calendar"},
	{"chart", "📊", "chart"},
	{"chat", "💬", "speech bubble"},
	{"folder", "📁", "folder"},
	{"book", "📖", "book"},
	{"bell", "🔔", "bell"},
	{"globe", "🌐", "globe"},
	{"wrench", "🔧", "wrench"},
	{"camera", "📷", "camera"},
	{"inbox", "📥", "inbox"},
	{"star", "⭐", "star"},
}

// AppIconEmoji is the emoji for a stored icon key, and false for a key not in
// AppIcons.
func AppIconEmoji(key string) (string, bool) {
	for _, i := range AppIcons {
		if i.Key == key {
			return i.Emoji, true
		}
	}
	return "", false
}

// ValidAppIcon refuses a key not in AppIcons.
func ValidAppIcon(key string) error {
	if _, ok := AppIconEmoji(key); !ok {
		return invalid("choose a tab icon from the list")
	}
	return nil
}

// appLabelPunct is the punctuation a tab label may carry besides letters,
// digits and single spaces.
const appLabelPunct = "-.&'"

// ValidAppLabel checks an already-trimmed tab label: 1 to MaxAppLabelRunes
// characters, each a letter, a digit, a space or one of - . & ', with no space
// at either end and no two in a row. So no markup, no control or bidi
// character and no invisible padding can reach the sidebar (it is rendered as
// text in any case).
func ValidAppLabel(label string) error {
	n := utf8.RuneCountInString(label)
	if label == "" || n > MaxAppLabelRunes || !utf8.ValidString(label) {
		return invalid("a tab label is 1-%d characters", MaxAppLabelRunes)
	}
	if label != strings.TrimSpace(label) || strings.Contains(label, "  ") {
		return invalid("a tab label has no space at either end and no two spaces in a row")
	}
	for _, r := range label {
		// Hangul fillers (U+115F, U+1160, U+3164, U+FFA0) are letters to
		// Unicode yet draw nothing: a label of them is an invisible tab.
		// Other_Default_Ignorable_Code_Point holds them. Unicode derives
		// Default_Ignorable_Code_Point from it plus format characters (Cf) and
		// variation selectors (Mn), neither a letter nor a digit, so the loop
		// below refuses those: each character left is visible, and there is at
		// least one (trimmed, not empty).
		if unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) {
			return invalid("a tab label may not carry invisible characters")
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ' && !strings.ContainsRune(appLabelPunct, r) {
			return invalid("a tab label is letters, digits, spaces and - . & ' only")
		}
	}
	return nil
}

// appMintChecks is the mint's `app` rules beyond the row's own CHECKs, in the
// caller's transaction: a person's plugin only (a team's tab is later work,
// decision 14), and at most MaxAppPlugins live ones per owner. The owner's
// app mints are serialised by an advisory lock, so two at once cannot both
// see four. 🔴 The lock only works under READ COMMITTED, where the count
// after the lock sees the other mint's commit; under REPEATABLE READ or
// SERIALIZABLE the snapshot could predate it. CreatePlugin begins its
// transaction READ COMMITTED explicitly, and this refuses any other level
// rather than depend on the server's default.
func (s *PGStore) appMintChecks(ctx context.Context, tx pgx.Tx, team bool) error {
	if team {
		return invalid("an app tab is a person's: a team's plugin cannot have one")
	}
	var iso string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
		return err
	}
	if iso != "read committed" {
		return fmt.Errorf("plugins: an app mint needs a read committed transaction, not %s", iso)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.plugin_app:' || coalesce(`+principalOwner+`, 0)::text, 0))`); err != nil {
		return err
	}
	var live int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM plugins
		WHERE `+ownedByPrincipal+` AND revoked_at IS NULL AND 'app' = ANY (capabilities)`).Scan(&live); err != nil {
		return err
	}
	if live >= MaxAppPlugins {
		return invalid("you already have %d app tabs; revoke one first", MaxAppPlugins)
	}
	return nil
}
