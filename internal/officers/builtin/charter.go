package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// THE OFFICER PAGE'S CHARTER (its "Instructions" section): what one officer's
// type's charter says, which version it is, which version each of the
// officer's instances last read (officer_defs.charter_briefed, migration
// 0140), how many officers share the type — and the page's write of it.
//
// 🔴 THE CHARTER IS THE TYPE'S, NOT THE OFFICER'S. officer_types holds it and
// every officer_defs row naming that type works from it, so an edit made on
// one officer's page reaches all of them. Charter.SharedBy is how many there
// are, and the page says so beside the editor.
// ---------------------------------------------------------------------------

// MaxCharterRunes bounds a charter in CHARACTERS: officer_types' CHECK
// (migration 0118) is length(charter) <= 32768, and Postgres's length() counts
// characters, not bytes. The editor's maxlength is the same number (a
// browser counts UTF-16 units there, so it is never looser).
const MaxCharterRunes = 32768

// Charter is one officer's type's charter as its page shows it.
type Charter struct {
	// TypeKey is the officer's type, whose charter this is.
	TypeKey string
	// Text and Version are the type's charter and charter_version.
	Text    string
	Version int
	// Briefed is, per instance name, the charter_version that instance was
	// last handed by officer_brief; an instance absent has read none.
	Briefed map[string]int
	// SharedBy is how many of the owner's officers name this type (itself
	// included: 1 is "only this one").
	SharedBy int
}

// CharterOf is owner's officer's type's charter (Charter); ErrNotFound when
// owner declares no such officer in the built-in store. Read under ctx's
// principal (row-level security).
func (s *Store) CharterOf(ctx context.Context, owner int64, officer string) (Charter, error) {
	var c Charter
	err := s.pool.QueryRow(ctx, `SELECT t.key, t.charter, t.charter_version, d.charter_briefed,
			(SELECT count(*) FROM officer_defs d2 WHERE d2.owner_id = d.owner_id AND d2.type_key = d.type_key)
		FROM officer_defs d JOIN officer_types t ON t.owner_id = d.owner_id AND t.key = d.type_key
		WHERE d.owner_id = $1 AND d.officer = $2`, owner, officer).Scan(&c.TypeKey, &c.Text, &c.Version, &c.Briefed, &c.SharedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Charter{}, ErrNotFound
	}
	if err != nil {
		return Charter{}, err
	}
	if c.Briefed == nil {
		c.Briefed = map[string]int{}
	}
	return c, nil
}

// CheckCharter is the store's rule for a charter the page writes: not blank,
// valid UTF-8, at most MaxCharterRunes characters. nil when it is one.
func CheckCharter(text string) error {
	switch {
	case strings.TrimSpace(text) == "":
		return fmt.Errorf("%w: write its instructions: they cannot be empty", ErrInvalid)
	case !utf8.ValidString(text):
		return fmt.Errorf("%w: the instructions are not valid text", ErrInvalid)
	case utf8.RuneCountInString(text) > MaxCharterRunes:
		return fmt.Errorf("%w: the instructions are over %d characters; shorten them", ErrInvalid, MaxCharterRunes)
	}
	return nil
}

// SetCharter writes owner's officer's type's charter, and ONLY its charter
// (its name and policy are left as they are, unlike PutType, which replaces
// all three). The version goes up by one when the text changed and not
// otherwise — PutType's own rule, in the same statement as the write.
// Answers the version now stored and whether the text changed; ErrNotFound
// for an officer owner does not declare here, ErrInvalid for a charter
// CheckCharter refuses.
func (s *Store) SetCharter(ctx context.Context, owner int64, officer, text string) (version int, changed bool, err error) {
	if err := CheckCharter(text); err != nil {
		return 0, false, err
	}
	err = s.pool.QueryRow(ctx, `UPDATE officer_types t
		SET charter = $3,
		    charter_version = t.charter_version + CASE WHEN old.charter = $3 THEN 0 ELSE 1 END,
		    updated_at = CASE WHEN old.charter = $3 THEN t.updated_at ELSE now() END
		FROM (SELECT t0.owner_id, t0.key, t0.charter FROM officer_types t0
		        JOIN officer_defs d ON d.owner_id = t0.owner_id AND d.type_key = t0.key
		       WHERE d.owner_id = $1 AND d.officer = $2 FOR UPDATE OF t0) old
		WHERE t.owner_id = old.owner_id AND t.key = old.key
		RETURNING t.charter_version, old.charter <> $3`, owner, officer, text).Scan(&version, &changed)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	if err != nil {
		return 0, false, classify(err)
	}
	return version, changed, nil
}

// ForgetBriefs clears what every instance of owner's officer last read
// (officer_defs.charter_briefed = {}): each is then an instance that has read
// no charter, which officer_claim and officer_complete refuse until it calls
// officer_brief (CharterCurrent) — and whose next FIRST turn muster starts in
// a new conversation (internal/api OfficerTurnContext, "Save and refresh").
// ErrNotFound for an officer owner does not declare here.
func (s *Store) ForgetBriefs(ctx context.Context, owner int64, officer string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE officer_defs SET charter_briefed = '{}'::jsonb WHERE owner_id = $1 AND officer = $2`, owner, officer)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// HasBriefed reports whether instance of owner's officer has a recorded brief
// (officer_defs.charter_briefed holds its name). ok false: owner declares no
// such officer in the built-in store.
func (s *Store) HasBriefed(ctx context.Context, owner int64, officer, instance string) (briefed, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT charter_briefed ? $3 FROM officer_defs WHERE owner_id = $1 AND officer = $2`, owner, officer, instance).Scan(&briefed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return briefed, true, nil
}
