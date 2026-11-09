package plugins

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// VIEWER HAND-OFF CODES (migration 0104; docs/plugins.md#viewer-hand-off).
//
// The owner's browser, signed in on muster's own origin, mints a code for one
// of its viewer plugins and a session view it is looking at; the browser then
// opens the plugin's entry URL with the code; the plugin redeems it with its
// own token and learns the owner, the target and whether the owner's step-up
// was fresh. A code is 32 random bytes, stored only as its sha256, valid for
// HandoffTTL and redeemable once, by the plugin it was minted for.

const (
	// HandoffTTL is how long a code may be redeemed after it is minted.
	HandoffTTL = 60 * time.Second
	// HandoffMintsPerMinute is how many codes one owner may mint in a minute.
	HandoffMintsPerMinute = 60
	// PurposeView is the purpose of a code minted from a session view's chip.
	PurposeView = "view"
	// handoffKeep is the age past which a mint deletes its owner's rows. It is
	// not a retention period: there is no background reaper, so an owner's
	// older rows stay until that owner's NEXT mint.
	handoffKeep = 24 * time.Hour
)

// ErrHandoffRateLimited: the owner minted HandoffMintsPerMinute codes in the
// last minute.
var ErrHandoffRateLimited = errors.New("plugins: too many hand-offs this minute")

// ErrViewerOriginRetired: the plugin's stored origin and entry URL no longer
// pass ValidViewerLink against the deployment's current MUSTER_PLUGIN_ORIGINS
// (the admin removed its pattern), so a code would send the owner's browser to
// a host the server no longer trusts.
var ErrViewerOriginRetired = errors.New("plugins: the plugin's origin is no longer allowed")

var purposeRE = regexp.MustCompile(`^[a-z_]{1,32}$`)

// HandoffTarget is the session view a code was minted on. Phase 1 knows only a
// mirrored window: its host, its tmux session (display only) and its tmux
// window id (`@3`), which with the owner is what a plugin matches on.
type HandoffTarget struct {
	Mirror *MirrorTarget `json:"mirror,omitempty"`
}

// MirrorTarget is one mirrored window, as muster's hub lists it.
type MirrorTarget struct {
	Host    string `json:"host"`
	Session string `json:"session"`
	Window  string `json:"window"`
}

// HandoffIn is what a mint records. SteppedUp is the minting session's own
// step-up state, read by the caller from the request's sign-in, never from
// anything the request says.
type HandoffIn struct {
	Target    HandoffTarget
	Purpose   string
	SteppedUp bool
	// Origins is the deployment's CURRENT MUSTER_PLUGIN_ORIGINS. The plugin's
	// stored origin and entry URL must still pass ValidViewerLink against it,
	// or the mint is ErrViewerOriginRetired.
	Origins []OriginPattern
}

// Handoff is a redeemed code.
type Handoff struct {
	OwnerID   int64         `json:"owner_id"`
	PluginID  int64         `json:"-"`
	Target    HandoffTarget `json:"target"`
	Purpose   string        `json:"purpose"`
	SteppedUp bool          `json:"stepped_up"`
	MintedAt  time.Time     `json:"minted_at"`
}

// newHandoffCode is 32 random bytes, base64url without padding (43 chars).
func newHandoffCode() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// handoffCodeShapeOK is a code's shape: 43 base64url characters.
func handoffCodeShapeOK(code string) bool {
	if len(code) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(code)
	return err == nil
}

func (s *PGStore) MintHandoff(ctx context.Context, pluginID int64, in HandoffIn, now time.Time) (string, Plugin, error) {
	if in.Target.Mirror == nil {
		return "", Plugin{}, invalid("a hand-off needs a target")
	}
	if !purposeRE.MatchString(in.Purpose) {
		return "", Plugin{}, invalid("a hand-off purpose is 1-32 of a-z and _")
	}
	target, err := json.Marshal(in.Target)
	if err != nil {
		return "", Plugin{}, err
	}
	code, err := newHandoffCode()
	if err != nil {
		return "", Plugin{}, err
	}
	var p Plugin
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// The principal's own live viewer plugin, locked FOR UPDATE: a
		// concurrent revoke settles first, and two mints for one plugin count
		// one after the other. Row-level security hides every other owner's.
		var err error
		p, err = scanPlugin(tx.QueryRow(ctx, `SELECT `+pluginCols+` FROM plugins
			WHERE id = $1 AND revoked_at IS NULL AND 'viewer' = ANY (capabilities) FOR UPDATE`, pluginID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if p.Origin == nil || p.EntryURL == nil {
			return ErrNotFound
		}
		if _, err := ValidViewerLink(in.Origins, *p.Origin, *p.EntryURL); err != nil {
			return ErrViewerOriginRetired
		}
		// The per-owner cap, counted from the owner's own rows. Serialised per
		// plugin by the lock above; mints racing on two of one owner's plugins
		// can exceed it by at most one per plugin.
		var recent int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM plugin_handoffs WHERE owner_id = $1 AND minted_at > $2`,
			p.OwnerID, now.Add(-time.Minute)).Scan(&recent); err != nil {
			return err
		}
		if recent >= HandoffMintsPerMinute {
			return ErrHandoffRateLimited
		}
		if _, err := tx.Exec(ctx, `DELETE FROM plugin_handoffs WHERE owner_id = $1 AND minted_at < $2`,
			p.OwnerID, now.Add(-handoffKeep)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO plugin_handoffs (owner_id, plugin_id, code_hash, target, purpose, stepped_up, minted_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			p.OwnerID, p.ID, HashToken(code), target, in.Purpose, in.SteppedUp, now, now.Add(HandoffTTL))
		return err
	})
	if err != nil {
		return "", Plugin{}, err
	}
	return code, p, nil
}

func (s *PGStore) RedeemHandoff(ctx context.Context, pluginID int64, code string, now time.Time) (Handoff, error) {
	if !handoffCodeShapeOK(code) {
		return Handoff{}, ErrNotFound
	}
	var h Handoff
	var target []byte
	err := s.pool.QueryRow(ctx, `
		UPDATE plugin_handoffs SET redeemed_at = $3
		 WHERE code_hash = $1 AND plugin_id = $2 AND redeemed_at IS NULL AND expires_at > $3
		RETURNING owner_id, plugin_id, target, purpose, stepped_up, minted_at`,
		HashToken(code), pluginID, now).Scan(&h.OwnerID, &h.PluginID, &target, &h.Purpose, &h.SteppedUp, &h.MintedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Handoff{}, ErrNotFound
	}
	if err != nil {
		return Handoff{}, err
	}
	if err := json.Unmarshal(target, &h.Target); err != nil {
		return Handoff{}, err
	}
	h.MintedAt = h.MintedAt.UTC() // the wire says UTC, whatever the connection's zone
	return h, nil
}
