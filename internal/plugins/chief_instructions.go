package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fence"
)

// A PLUGIN'S INSTRUCTION SECTION FOR ITS OWNER'S CHIEF (migration 0141,
// docs/plugins.md#chief-instructions).
//
// A plugin with CapChiefExtension stores ONE markdown section (PUT
// /plugin/v1/chief/instructions); its owner approves that exact text, by its
// sha256, on /plugins; and only then is it rendered into the owner's chief's
// CLAUDE.md, at the chief's next provision or restart, under a fixed core
// wrapper that says it grants nothing (internal/agentspec PluginSectionsBlock).
//
// 🔴 THE SCHEMA HOLDS THE TWO RULES THAT MAKE IT SAFE, not only this file:
// the stored sha IS the text's (plugin_chief_instructions_sha_is_text), and an
// approval names this row's CURRENT sha or nothing
// (plugin_chief_instructions_approved_is_current) — so new text is pending
// again in the very write that stores it.

// MaxChiefInstructionsBytes caps a section (the 0141 CHECK): 4 KiB.
const MaxChiefInstructionsBytes = 4096

// ChiefInstructions is one plugin's stored section, as its owner and the
// plugin see it.
type ChiefInstructions struct {
	PluginID    int64      `json:"plugin_id"`
	Text        string     `json:"markdown"`
	SHA         string     `json:"sha"`
	ApprovedSHA *string    `json:"approved_sha"`
	ApprovedAt  *time.Time `json:"approved_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Approved reports whether the owner approved THIS text.
func (c ChiefInstructions) Approved() bool { return c.ApprovedSHA != nil && *c.ApprovedSHA == c.SHA }

// ChiefSection is one approved section as the chief's prose renders it: the
// plugin's name, when its owner approved it, and the text.
type ChiefSection struct {
	Plugin     string
	ApprovedAt time.Time
	Text       string
}

// ErrStaleApproval: the owner approved a sha that is no longer the plugin's
// text (it changed after the page was drawn), or there is no text (409).
var ErrStaleApproval = errors.New("plugins: the plugin's chief instructions changed since you read them")

// ChiefInstructionsSHA is the stored hash of text: sha256, lower-case hex
// (0141's plugin_chief_instructions_sha_is_text computes the same).
func ChiefInstructionsSHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ValidChiefInstructions checks a section before it is stored: 1 to
// MaxChiefInstructionsBytes bytes of UTF-8, not blank, with no control or
// invisible character but a line feed and a tab (fence.Unsafe). It is refused
// rather than cleaned, so the hash the owner approves is of exactly what the
// plugin sent.
//
// 🔴 NO `@` IMPORT: an `@` is refused unless the character right before it is
// a letter, a digit, or one of `.`, `_`, `-`, `+` (importAtAllowedAfter). So `@`
// at the start of the text, after any whitespace or after punctuation such as
// `(` or a backtick is refused, and an address like `ops@example.test` is kept.
// The section is written into the chief's CLAUDE.md, and Claude Code expands an
// `@path` import there EVEN INSIDE the `> ` quote that frames plugin text
// (measured on a live claude: a quoted `> @file` made the model read the file).
// An import would let a plugin pull any file the chief can read into its
// standing context, past the owner's approval of the text. It is REFUSED rather
// than escaped: an escape is a guess about one CLI version's import parser,
// which can change under us, and the approved sha must be of the exact bytes
// the chief reads; a refusal leaves no such text to mis-escape. The rule is
// wider than the parser's "start or after whitespace" on purpose, for the same
// reason.
func ValidChiefInstructions(text string) error {
	switch {
	case len(text) > MaxChiefInstructionsBytes:
		return invalid("markdown is over %d bytes", MaxChiefInstructionsBytes)
	case !utf8.ValidString(text):
		return invalid("markdown is not valid UTF-8")
	}
	blank := true
	prev := rune(-1) // the rune before r; -1 at the start of the text
	for _, r := range text {
		before := prev
		prev = r
		if r == '@' && !importAtAllowedAfter(before) {
			return invalid("markdown may not carry an @ that Claude Code could read as a file import " +
				"(an @ at the start, or after a space, line break or punctuation); an address like name@example.test is fine")
		}
		if r == '\n' || r == '\t' {
			continue
		}
		if fence.Unsafe(r) {
			return invalid("markdown may not carry control or invisible characters (a line feed and a tab are allowed)")
		}
		if r != ' ' {
			blank = false
		}
	}
	if blank {
		return invalid("markdown is required")
	}
	return nil
}

// importAtAllowedAfter reports whether an `@` may follow r in a section: only
// inside a word, as in an e-mail address (ValidChiefInstructions).
func importAtAllowedAfter(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' || r == '+'
}

const chiefInstructionsCols = `plugin_id, text, sha, approved_sha, approved_at, updated_at`

func scanChiefInstructions(row pgx.Row) (ChiefInstructions, error) {
	var c ChiefInstructions
	err := row.Scan(&c.PluginID, &c.Text, &c.SHA, &c.ApprovedSHA, &c.ApprovedAt, &c.UpdatedAt)
	return c, err
}

// PutChiefInstructions stores pluginID's section as ctx's principal (the
// plugin's own token, authz.ViaPlugin), replacing any earlier one. Text that
// differs from the stored text is PENDING: its approval is cleared in the same
// write. The same text again changes nothing (changed false), its approval
// kept. ErrNotFound for a plugin that is not live, not the principal's, or
// lacks CapChiefExtension (it lost it after the door admitted it).
//
// Audited plugin.chief_instructions.stored {sha, old_sha} when it changed.
func (s *PGStore) PutChiefInstructions(ctx context.Context, pluginID int64, text string) (c ChiefInstructions, changed bool, err error) {
	if err := ValidChiefInstructions(text); err != nil {
		return ChiefInstructions{}, false, err
	}
	sha := ChiefInstructionsSHA(text)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var caps []Capability
		err := tx.QueryRow(ctx, `SELECT capabilities FROM plugins WHERE id = $1 AND revoked_at IS NULL AND `+ownedByPrincipal+` FOR SHARE`, pluginID).Scan(&caps)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !(Plugin{Capabilities: caps}).Has(CapChiefExtension) {
			return ErrNotFound
		}
		old, err := scanChiefInstructions(tx.QueryRow(ctx, `SELECT `+chiefInstructionsCols+` FROM plugin_chief_instructions WHERE plugin_id = $1 FOR UPDATE`, pluginID))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		case old.SHA == sha:
			c = old
			return nil
		}
		if c, err = scanChiefInstructions(tx.QueryRow(ctx, `
			INSERT INTO plugin_chief_instructions (plugin_id, text, sha) VALUES ($1, $2, $3)
			ON CONFLICT (plugin_id) DO UPDATE SET text = EXCLUDED.text, sha = EXCLUDED.sha,
				approved_sha = NULL, approved_at = NULL, updated_at = now()
			RETURNING `+chiefInstructionsCols, pluginID, text, sha)); err != nil {
			return err
		}
		changed = true
		return audit(ctx, tx, "plugin.chief_instructions.stored", Object(pluginID), map[string]any{"sha": sha, "old_sha": old.SHA})
	})
	return c, changed, err
}

// ChiefInstructionsOf reads pluginID's section as ctx's principal: the
// owner's /plugins card (the plugin learns its state from its PUT). ok is false when it has
// none (or the plugin is not the principal's own).
func (s *PGStore) ChiefInstructionsOf(ctx context.Context, pluginID int64) (c ChiefInstructions, ok bool, err error) {
	c, err = scanChiefInstructions(s.pool.QueryRow(ctx, `SELECT `+chiefInstructionsCols+` FROM plugin_chief_instructions
		WHERE plugin_id = $1 AND `+ownedByPrincipal, pluginID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ChiefInstructions{}, false, nil
	}
	return c, err == nil, err
}

// ApproveChiefInstructions records the owner's approval of pluginID's section
// whose sha is sha, as ctx's principal (the owner's /plugins session). 🔴 IT
// APPROVES ONLY THE TEXT THE OWNER WAS SHOWN: a sha that is not the stored
// one — the plugin changed it after the page was drawn — is ErrStaleApproval,
// and nothing is approved. ErrNotFound for a plugin that is not the
// principal's own, is revoked, or lacks CapChiefExtension. Approving it twice
// keeps the first time.
//
// Audited plugin.chief_instructions.approved {sha}.
func (s *PGStore) ApproveChiefInstructions(ctx context.Context, pluginID int64, sha string) (ChiefInstructions, error) {
	var c ChiefInstructions
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var caps []Capability
		err := tx.QueryRow(ctx, `SELECT capabilities FROM plugins WHERE id = $1 AND revoked_at IS NULL AND `+ownedByPrincipal+` FOR SHARE`, pluginID).Scan(&caps)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !(Plugin{Capabilities: caps}).Has(CapChiefExtension) {
			return ErrNotFound
		}
		cur, err := scanChiefInstructions(tx.QueryRow(ctx, `SELECT `+chiefInstructionsCols+` FROM plugin_chief_instructions WHERE plugin_id = $1 FOR UPDATE`, pluginID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleApproval
		}
		if err != nil {
			return err
		}
		if cur.SHA != sha {
			return ErrStaleApproval
		}
		if cur.Approved() {
			c = cur
			return nil
		}
		if c, err = scanChiefInstructions(tx.QueryRow(ctx, `UPDATE plugin_chief_instructions SET approved_sha = sha, approved_at = now()
			WHERE plugin_id = $1 AND sha = $2 RETURNING `+chiefInstructionsCols, pluginID, sha)); err != nil {
			return err
		}
		return audit(ctx, tx, "plugin.chief_instructions.approved", Object(pluginID), map[string]any{"sha": sha})
	})
	return c, err
}

// ApprovedChiefSections is the sections a chief owned by owner renders: each
// of owner's LIVE plugins that still has CapChiefExtension, whose stored text
// is the text owner approved — oldest plugin first, so the order is stable
// across restarts. A team's chief passes the team's id, so it gets the team's
// own plugins and never a member's personal one.
//
// 🔴 IT RUNS UNDER authz.System WITH AN EXPLICIT OWNER FILTER on both tables:
// the chief's spec is built by the provisioner, from loops and requests alike,
// with no principal that is the chief's owner (the shape of
// credentials.Plane.usesEnvironment). Nothing but owner's rows is read, and
// only the rendered fields are handed on. System-ledger entry.
func (s *PGStore) ApprovedChiefSections(ctx context.Context, owner int64) ([]ChiefSection, error) {
	if owner <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(authz.System(ctx), `
		SELECT p.name, c.approved_at, c.text
		  FROM plugin_chief_instructions c
		  JOIN plugins p ON p.id = c.plugin_id AND p.owner_id = c.owner_id
		 WHERE c.owner_id = $1 AND p.owner_id = $1
		   AND p.revoked_at IS NULL AND $2 = ANY (p.capabilities)
		   AND c.approved_sha = c.sha
		 ORDER BY p.id`, owner, string(CapChiefExtension))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChiefSection, error) {
		var sec ChiefSection
		err := r.Scan(&sec.Plugin, &sec.ApprovedAt, &sec.Text)
		return sec, err
	})
}
