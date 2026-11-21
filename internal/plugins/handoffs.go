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

	"github.com/ZacxDev/muster/internal/authz"
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
	// PurposeView is the purpose of a code minted from a session view's chip
	// (a mirror target); PurposeApp of one minted by an app tab's page (an
	// app target, docs/design/meetings.md). The purpose follows the TARGET
	// (HandoffTarget.purpose), never anything a request says.
	PurposeView = "view"
	PurposeApp  = "app"
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

// HandoffTarget is what a code was minted for: EXACTLY ONE of a mirrored
// window (its host, its tmux session, display only, and its tmux window id
// `@3`, which with the owner is what a plugin matches on) and an app tab's
// page (App).
type HandoffTarget struct {
	Mirror *MirrorTarget `json:"mirror,omitempty"`
	App    *AppTarget    `json:"app,omitempty"`
}

// AppTarget is an app tab's page (`/apps/{id}`): {"app": {}}, or {"app":
// {"ref": "<ref>"}} when the page was opened with ?ref= (a notification's
// tap). muster passes Ref through and does not interpret it; the plugin treats
// it as a hint. Only a Ref ValidAppRef accepts is ever stored.
type AppTarget struct {
	Ref string `json:"ref,omitempty"`
}

var appRefRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidAppRef reports whether ref may ride an app target: 1-64 of
// A-Z a-z 0-9 _ -. Anything else is dropped by the caller, not refused.
func ValidAppRef(ref string) bool { return appRefRE.MatchString(ref) }

// purpose is the code's purpose and the capability its plugin must hold, from
// the target alone: a mirror target mints `view` and needs viewer, an app
// target mints `app` and needs app. A target with neither or both is invalid,
// and so is an app target carrying a ref ValidAppRef refuses.
func (t HandoffTarget) purpose() (string, Capability, error) {
	switch {
	case t.Mirror != nil && t.App == nil:
		return PurposeView, CapViewer, nil
	case t.App != nil && t.Mirror == nil:
		if t.App.Ref != "" && !ValidAppRef(t.App.Ref) {
			return "", "", invalid("an app target's ref is 1-64 of A-Z a-z 0-9 _ -")
		}
		return PurposeApp, CapApp, nil
	}
	return "", "", invalid("a hand-off has exactly one target: mirror or app")
}

// MirrorTarget is one mirrored window, as muster's hub lists it.
type MirrorTarget struct {
	Host    string `json:"host"`
	Session string `json:"session"`
	Window  string `json:"window"`
}

// HandoffIn is what a mint records. SteppedUp is the minting session's own
// step-up state, read by the caller from the request's sign-in, never from
// anything the request says. There is no purpose field: it is the target's
// (HandoffTarget.purpose).
type HandoffIn struct {
	Target    HandoffTarget
	SteppedUp bool
	// Origins is the deployment's CURRENT MUSTER_PLUGIN_ORIGINS. The plugin's
	// stored origin and entry URL must still pass ValidViewerLink against it,
	// or the mint is ErrViewerOriginRetired.
	Origins []OriginPattern
	// SignedInAt is the minting session's own sign-in time (stepup.AuthTime),
	// read by the caller from the request's sign-in, never from the request
	// body. A SERVER app's code records it (plugin_handoffs.signed_in_at),
	// and its redeem's grant copies it (server.go); zero refuses a server
	// mint with ErrSignInUnknown. A personal code does not use it.
	SignedInAt time.Time
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
	purpose, need, err := in.Target.purpose()
	if err != nil {
		return "", Plugin{}, err
	}
	target, err := json.Marshal(in.Target)
	if err != nil {
		return "", Plugin{}, err
	}
	code, err := newHandoffCode()
	if err != nil {
		return "", Plugin{}, err
	}
	// 🔴 THE CAP AND THE PRUNE ARE KEYED ON THE MINTING PRINCIPAL (core PR
	// 1h): the person, or a team's acting principal minting for its own
	// plugin. For a personal plugin read under ownedByPrincipal that is the
	// row's owner, the same number as before; for a server app (no owner) it
	// is the only owner there is, so two people minting for one app keep
	// their own budgets and their own rows.
	minter, ok := authz.From(ctx)
	if !ok || minter.UserID <= 0 || minter.All() {
		return "", Plugin{}, ErrNotFound
	}
	var p Plugin
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// The principal's own live plugin holding the TARGET's capability
		// (viewer for a mirror, app for an app page), locked FOR UPDATE: a
		// concurrent revoke settles first, and two mints for one plugin count
		// one after the other. Row-level security hides every other owner's.
		var err error
		p, err = scanPlugin(tx.QueryRow(ctx, `SELECT `+pluginCols+` FROM plugins
			WHERE id = $1 AND revoked_at IS NULL AND $2::text = ANY (capabilities)
			  AND `+ownedByPrincipal+` FOR UPDATE`, pluginID, string(need)))
		if errors.Is(err, pgx.ErrNoRows) {
			return errServerArm
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
		return insertHandoff(ctx, tx, minter.UserID, handoffRow{plugin: &p.ID, code: code, target: target,
			purpose: purpose, steppedUp: in.SteppedUp}, now)
	})
	if errors.Is(err, errServerArm) {
		// Not the principal's own: a SERVER app's, or nothing (server.go).
		if purpose != PurposeApp {
			return "", Plugin{}, ErrNotFound
		}
		p, err = s.mintServerHandoff(ctx, minter, pluginID, in, now, handoffRow{code: code, target: target,
			purpose: purpose, steppedUp: in.SteppedUp})
	}
	if err != nil {
		return "", Plugin{}, err
	}
	return code, p, nil
}

// errServerArm is MintHandoff's own signal that the principal has no such
// plugin of its own, so the server arm is tried. Never returned.
var errServerArm = errors.New("plugins: not the principal's own plugin")

// handoffRow is one code row as insertHandoff writes it: EXACTLY ONE of
// plugin (a personal plugin's id) and server (a server app's id), and for a
// server code the minting session's sign-in time.
type handoffRow struct {
	plugin, server *int64
	code           string
	target         []byte
	purpose        string
	steppedUp      bool
	signedInAt     *time.Time
}

// insertHandoff is the mint's cap, prune and insert in tx, ALL KEYED ON
// minter (the minting principal's user id, never a plugin row's owner):
// HandoffMintsPerMinute codes a minute, the minter's rows older than
// handoffKeep deleted, and the new row OWNED BY minter. Serialised per plugin
// by the caller's row lock; mints racing on two plugins can exceed the cap by
// at most one per plugin.
func insertHandoff(ctx context.Context, tx pgx.Tx, minter int64, h handoffRow, now time.Time) error {
	var recent int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM plugin_handoffs WHERE owner_id = $1 AND minted_at > $2`,
		minter, now.Add(-time.Minute)).Scan(&recent); err != nil {
		return err
	}
	if recent >= HandoffMintsPerMinute {
		return ErrHandoffRateLimited
	}
	if _, err := tx.Exec(ctx, `DELETE FROM plugin_handoffs WHERE owner_id = $1 AND minted_at < $2`,
		minter, now.Add(-handoffKeep)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO plugin_handoffs (owner_id, plugin_id, server_plugin_id, code_hash, target, purpose, stepped_up,
			minted_at, expires_at, signed_in_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		minter, h.plugin, h.server, HashToken(h.code), h.target, h.purpose, h.steppedUp, now, now.Add(HandoffTTL), h.signedInAt)
	return err
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
	// (A server code's plugin_id is NULL, so no personal token's id matches it:
	// server.go's RedeemServerHandoff spends those, by server_plugin_id.)
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
