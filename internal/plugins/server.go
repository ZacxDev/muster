package plugins

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// SERVER-INSTALLED APP PLUGINS (docs/design/meetings.md, "Core seam:
// server-installed app plugins"; migration 0172; docs/plugins.md#server-apps).
//
// An ADMIN installs an app once for the whole server: a plugins row with
// scope 'server' and NO OWNER, holding `app` and optionally `tasks`. Its token
// acts as NOBODY. It learns a person only from a hand-off code THAT PERSON'S
// OWN BROWSER minted on muster's page (MintHandoff's server arm) and handed
// it in the frame; each redeem records a GRANT (plugin_server_users): for
// ServerActFor the token may knock and file tasks for that person, and for
// nobody else, by naming the PAIRWISE subject the redeem gave it
// (Muster-Act-For, internal/api). It has no route that lists, searches or
// counts people.
//
// 🔴 A SERVER ROW IS INVISIBLE TO EVERY PRINCIPAL (its owner_id is NULL, and
// no row-level security arm admits a NULL owner), so every read of one here is
// a deliberate authz.System read with an explicit filter, each in the System
// ledger (internal/authz ledger_test.go), and each write of a grant names its
// owner_id explicitly.

const (
	// ServerActFor is how long a redeem lets a server app act for its person
	// (fork 3): the grant's act_until is the later of its current value and
	// the redeem's time plus this.
	ServerActFor = 8 * time.Hour
	// MaxServerApps is how many live server apps one server may have, apart
	// from each person's own MaxAppPlugins, so the sidebar cannot be flooded.
	MaxServerApps = 5
	// MaxOwnersCheck is how many subjects one POST /plugin/v1/owners/check
	// may ask about.
	MaxOwnersCheck = 200
)

// ErrSignInUnknown: the minting session records no sign-in time, so a server
// code would carry nothing a Sign out everywhere could be compared with. The
// mint answers 401 "sign in again", as pairing a phone does.
var ErrSignInUnknown = errors.New("plugins: this session records no sign-in time")

// ErrServerAppOrigin: an install's origin is not one of the deployment's
// MUSTER_PLUGIN_ORIGINS, or its entry URL is not an https URL under it. It
// also matches ErrInvalid, with ValidViewerLink's own reason.
var ErrServerAppOrigin = errors.New("plugins: a server app's origin and entry URL must be one of this server's MUSTER_PLUGIN_ORIGINS and an https URL under it")

// subjectRE is a subject's shape: 22 base64url characters (128 bits).
const subjectLen = 22

// ValidSubject reports whether s is shaped like a subject a redeem gives.
func ValidSubject(s string) bool {
	if len(s) != subjectLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == 16
}

// newSubject is a new pairwise subject: 128 random bits, base64url. Random,
// never derived from the user id, so it says nothing about who the person
// is, and a person's subjects under two apps cannot be joined — unless an
// admin LINKED the two apps (server_pairs.go: installed one --link-to the
// other; the two keep what they shared after an unlink, and neither may then
// be linked to a third). 🔴 ITS ONE CALLER IS subjectFor,
// which decides whether a pair's subject is taken instead (internal/api
// TestOneFunctionChoosesASubject).
func newSubject() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// serverGrantLive is THE ONE PREDICATE "may server app $1 act, at $2, for the
// person this grant row names" (g = plugin_server_users, p = plugins, u =
// users): serverGrantInTime AND serverGrantStanding, over a row the statement
// has already keyed on the app (LiveServerGrants' WHERE g.plugin_id = $1, the
// one place the app is matched, so dropping it is one mutant). One
// line per gate, so a mutation sweep drops each alone. LiveServerGrants is its
// ONLY reader (internal/api TestTheServerGrantGateIsOneFunction pins that from
// the Go AST); it evaluates the two halves apart, so an owners/check can tell
// a grant that only ran out of time (expired) from one that was ended
// (revoked) — from the SAME lines the act-for door passes on.
//
// 🔴 THE CUTOFFS ARE COMPARED WITH THE SIGN-IN THAT MINTED THE CODE (signin_at,
// copied from the code at the redeem), never with the redeem's time or now():
// GoTrue mode applies Sign out everywhere from each process's memory, so a
// stolen session from before a sign-out can still mint on another pod; its
// code's sign-in time is before the cutoff, and is refused here on every call
// whatever any process remembers. Whole seconds, at-or-before refused: the
// paired-device rule (internal/devices LiveSQL).
const serverGrantLive = `(` + serverGrantInTime + `) AND (` + serverGrantStanding + `)`

// serverGrantInTime is the grant's clock: a redeem in the last
// ServerActFor (fork 3). False alone is EXPIRED, not revoked.
const serverGrantInTime = `g.act_until > $2`

// serverGrantStanding is everything else that must hold for the grant to
// act: anything false here is REVOKED.
const serverGrantStanding = `g.hidden_at IS NULL
  AND g.signin_at IS NOT NULL
  AND p.revoked_at IS NULL AND p.scope = 'server'
  AND u.id IN (` + users.EnabledPersonIDs + `)
  AND (u.signed_out_at IS NULL OR floor(extract(epoch FROM g.signin_at)) > floor(extract(epoch FROM u.signed_out_at)))
  AND (u.sessions_not_before IS NULL OR floor(extract(epoch FROM g.signin_at)) > floor(extract(epoch FROM u.sessions_not_before)))`

// GrantState is what LiveServerGrants says of one subject.
type GrantState string

const (
	// GrantLive: the app may act for the person now (serverGrantLive).
	GrantLive GrantState = "live"
	// GrantExpired: the grant's ServerActFor ran out and NOTHING ELSE is
	// wrong; the person's next visit to the tab renews it. The plugin keeps
	// that person's open meeting.
	GrantExpired GrantState = "expired"
	// GrantRevoked: anything else — no such grant under this app, hidden,
	// disabled, signed out everywhere (or cut off) since the sign-in, not a
	// person, the app gone, or its origin retired. The plugin ends that
	// person's sessions. Never says which.
	GrantRevoked GrantState = "revoked"
)

// ServerGrant is one subject's answer: its state, and for GrantLive alone
// the person it names (0 otherwise, so no caller can act on another state).
type ServerGrant struct {
	State GrantState
	Owner int64
}

// LiveServerGrants is THE GATE on a server app acting for a person: every
// one of subjects, with its state under app at now and, when live, the
// person it names. Both consumers call it — the act-for door with one subject
// (acting only on GrantLive), POST /plugin/v1/owners/check with up to
// MaxOwnersCheck — so the two can never disagree about who is live.
//
// It first checks app's origin and entry URL against the deployment's
// CURRENT origins (ValidViewerLink): a retired origin answers every subject
// GrantRevoked. A personal plugin answers every subject GrantRevoked too. One
// statement, under authz.System (the person is learned from the row, as the
// token is in Authenticate), keyed on the AUTHENTICATED app's id and never on
// anything else the request says, computing serverGrantInTime and
// serverGrantStanding per row: live is both, expired is standing alone.
func (s *PGStore) LiveServerGrants(ctx context.Context, app Plugin, origins []OriginPattern, subjects []string, now time.Time) (map[string]ServerGrant, error) {
	out := make(map[string]ServerGrant, len(subjects))
	for _, sub := range subjects {
		out[sub] = ServerGrant{State: GrantRevoked}
	}
	if !app.IsServer() || app.Origin == nil || app.EntryURL == nil || len(subjects) == 0 {
		return out, nil
	}
	if _, err := ValidViewerLink(origins, *app.Origin, *app.EntryURL); err != nil {
		return out, nil
	}
	rows, err := s.pool.Query(authz.System(ctx), `SELECT g.subject, g.owner_id,
		       coalesce((`+serverGrantLive+`), false), coalesce((`+serverGrantStanding+`), false)
		  FROM plugin_server_users g
		  JOIN plugins p ON p.id = g.plugin_id JOIN users u ON u.id = g.owner_id
		 WHERE g.plugin_id = $1 AND g.subject = ANY ($3::text[])`, app.ID, now, subjects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sub string
		var owner int64
		var live, standing bool
		if err := rows.Scan(&sub, &owner, &live, &standing); err != nil {
			return nil, err
		}
		switch {
		case live:
			out[sub] = ServerGrant{State: GrantLive, Owner: owner}
		case standing:
			out[sub] = ServerGrant{State: GrantExpired}
		}
	}
	return out, rows.Err()
}

// mintServerHandoff is MintHandoff's SERVER ARM: pluginID is not one of the
// principal's own plugins, so it may be a live server app. The whole
// transaction runs as authz.System (a server row is hidden from every
// principal), and every statement names its owner explicitly: the code row is
// OWNED BY minter, the person, taken from the request before the
// transaction. Refused ErrNotFound unless the minter is a PERSON (never a
// team's acting principal), the app is live and has `app`, and the person has
// not hidden it, and its origin still passes the CURRENT origins (a retired
// one is the same ErrNotFound); ErrSignInUnknown for a session with no
// sign-in time.
func (s *PGStore) mintServerHandoff(ctx context.Context, minter authz.Principal, pluginID int64, in HandoffIn, now time.Time, h handoffRow) (Plugin, error) {
	if minter.Team || minter.Via == authz.ViaSystem || minter.UserID <= 0 {
		return Plugin{}, ErrNotFound
	}
	var p Plugin
	err := s.inTx(authz.System(ctx), func(tx pgx.Tx) error {
		sys := authz.System(ctx)
		var err error
		p, err = scanPlugin(tx.QueryRow(sys, `SELECT `+pluginCols+` FROM plugins
			WHERE id = $1 AND scope = 'server' AND revoked_at IS NULL AND 'app' = ANY (capabilities) FOR UPDATE`, pluginID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var hidden bool
		if err := tx.QueryRow(sys, `SELECT EXISTS (SELECT 1 FROM plugin_server_users
			WHERE plugin_id = $1 AND owner_id = $2 AND hidden_at IS NOT NULL)`, p.ID, minter.UserID).Scan(&hidden); err != nil {
			return err
		}
		if hidden || p.Origin == nil || p.EntryURL == nil {
			return ErrNotFound
		}
		// A retired origin is the same 404 as no app (the design's mint arm):
		// a person cannot act on "revoke it and mint another".
		if _, err := ValidViewerLink(in.Origins, *p.Origin, *p.EntryURL); err != nil {
			return ErrNotFound
		}
		if in.SignedInAt.IsZero() {
			return ErrSignInUnknown
		}
		signin := in.SignedInAt
		h.server, h.signedInAt = &p.ID, &signin
		return insertHandoff(sys, tx, minter.UserID, h, now)
	})
	return p, err
}

// ServerOwner is a server app's view of a person (fork 2): the pairwise
// subject and the account's display name ("" when it has none). Never the
// muster user id, never the email.
type ServerOwner struct {
	Subject     string `json:"subject"`
	DisplayName string `json:"display_name"`
}

// ServerHandoff is a server app's redeemed code: who opened the tab, as the
// app knows them, and until when it may act for them. The JSON is the wire
// answer of POST /plugin/v1/handoffs/redeem for a server token.
type ServerHandoff struct {
	Owner     ServerOwner   `json:"owner"`
	Scope     string        `json:"scope"`
	Target    HandoffTarget `json:"target"`
	Purpose   string        `json:"purpose"`
	SteppedUp bool          `json:"stepped_up"`
	MintedAt  time.Time     `json:"minted_at"`
	ActUntil  time.Time     `json:"act_until"`
	// ownerID is the person, for muster's own use (the grant metric). Not on
	// the wire.
	ownerID int64
}

// RedeemServerHandoff spends a code minted for server app app and records
// the grant: one transaction, entirely as authz.System (the code is how the
// person is learned), every write naming owner_id explicitly.
//
//  1. The code is spent: unknown, used, expired, a personal plugin's (its
//     server_plugin_id is NULL) or another server app's is ErrNotFound.
//  2. 🔴 THE PERSON IS RE-CHECKED ON A FRESH READ, in the same transaction:
//     an enabled person, not hidden from this app, and the code's
//     signed_in_at strictly after both cutoffs in whole seconds. A person
//     hidden or disabled between the mint and the redeem, or a code minted by
//     a session a later Sign out everywhere ended, is the same ErrNotFound —
//     and the code is spent all the same.
//  3. The grant is upserted: the subject kept if the row exists, else
//     subjectFor's (the linked partner's subject for the person, or a new
//     random one); signin_at set to THE CODE'S signed_in_at, never now();
//     act_until the later of its current value and now + ServerActFor.
func (s *PGStore) RedeemServerHandoff(ctx context.Context, app Plugin, code string, now time.Time) (ServerHandoff, error) {
	if !app.IsServer() || !handoffCodeShapeOK(code) {
		return ServerHandoff{}, ErrNotFound
	}
	var h ServerHandoff
	refused := false
	sys := authz.System(ctx)
	err := s.inTx(sys, func(tx pgx.Tx) error {
		// 🔴 THE APP IS RE-CHECKED AND HELD (audit round 1): its row is locked
		// FOR SHARE, so an uninstall racing this redeem waits for it — and then
		// deletes the grant it made — or wins first, and this redeem is the
		// one 404 and writes nothing. The door's Authenticate is a read from
		// before this transaction began.
		var held int64
		switch err := tx.QueryRow(sys, `SELECT id FROM plugins WHERE id = $1 AND scope = 'server' AND revoked_at IS NULL FOR SHARE`,
			app.ID).Scan(&held); {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		}
		var target []byte
		var signin *time.Time
		err := tx.QueryRow(sys, `UPDATE plugin_handoffs SET redeemed_at = $3
			 WHERE code_hash = $1 AND server_plugin_id = $2 AND redeemed_at IS NULL AND expires_at > $3
			RETURNING owner_id, target, purpose, stepped_up, minted_at, signed_in_at`,
			HashToken(code), app.ID, now).Scan(&h.ownerID, &target, &h.Purpose, &h.SteppedUp, &h.MintedAt, &signin)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if signin == nil {
			refused = true // no row 0172 allows; spent, and refused
			return nil
		}
		if err := json.Unmarshal(target, &h.Target); err != nil {
			return err
		}
		err = tx.QueryRow(sys, `SELECT coalesce(u.display_name, '') FROM users u
			 WHERE u.id = $1 AND u.id IN (`+users.EnabledPersonIDs+`)
			   AND NOT EXISTS (SELECT 1 FROM plugin_server_users h
			                    WHERE h.plugin_id = $2 AND h.owner_id = u.id AND h.hidden_at IS NOT NULL)
			   AND (u.signed_out_at IS NULL OR floor(extract(epoch FROM $3::timestamptz)) > floor(extract(epoch FROM u.signed_out_at)))
			   AND (u.sessions_not_before IS NULL OR floor(extract(epoch FROM $3::timestamptz)) > floor(extract(epoch FROM u.sessions_not_before)))`,
			h.ownerID, app.ID, *signin).Scan(&h.Owner.DisplayName)
		if errors.Is(err, pgx.ErrNoRows) {
			refused = true // the code stays spent: the commit below keeps it so
			return nil
		}
		if err != nil {
			return err
		}
		sub, err := subjectFor(sys, tx, app.ID, h.ownerID)
		if err != nil {
			return err
		}
		return tx.QueryRow(sys, `INSERT INTO plugin_server_users (plugin_id, owner_id, subject, signin_at, granted_at, act_until)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (plugin_id, owner_id) DO UPDATE
			   SET signin_at = EXCLUDED.signin_at, granted_at = EXCLUDED.granted_at,
			       act_until = greatest(plugin_server_users.act_until, EXCLUDED.act_until)
			RETURNING subject, act_until`,
			app.ID, h.ownerID, sub, *signin, now, now.Add(ServerActFor)).Scan(&h.Owner.Subject, &h.ActUntil)
	})
	if err != nil {
		return ServerHandoff{}, err
	}
	if refused {
		return ServerHandoff{}, ErrNotFound
	}
	h.Scope = ScopeServer
	h.MintedAt, h.ActUntil = h.MintedAt.UTC(), h.ActUntil.UTC()
	return h, nil
}

// OwnerID is the person a server code was redeemed for: muster's own use (a
// metric, a log line), never the wire's.
func (h ServerHandoff) OwnerID() int64 { return h.ownerID }

// ServerAppIn is what an install records.
type ServerAppIn struct {
	// Name is the slug (ValidName), unique among live server apps.
	Name string
	// AppLabel and AppIcon are the tab's (ValidAppLabel, AppIcons): the
	// ADMIN's words, which titles every person's knock.
	AppLabel, AppIcon string
	// Origin and EntryURL must pass ValidViewerLink against Origins.
	Origin, EntryURL string
	Origins          []OriginPattern
	// Tasks adds CapTasks ("Files tasks"). No other capability is offered: a
	// server app has no person's chief to act on.
	Tasks bool
	// LinkTo, when not 0, is a live server app the new one is LINKED to
	// (server_pairs.go linkAtInstall), in the install's own transaction: the
	// two then know each person by one subject. The command line's
	// `app install --link-to` only.
	LinkTo int64
}

// serverAppActor is the audit actor and installed_by of a server-app write:
// the admin's user id from the page, or no user from the command line
// (users.Actor{Via: "cli"}, as SetUserDisabledAs audits a CLI disable). 🔴
// installed_by is BOOKKEEPING ONLY: nothing reads it to decide anything.
func serverAppActor(by users.Actor) (installedBy *int64, via string) {
	if by.UserID > 0 {
		id := by.UserID
		return &id, "page"
	}
	return nil, "cli"
}

// InstallServerApp installs a server app and returns its token ONCE. Under
// authz.System (the row has no owner), serialised by an advisory lock so two
// installs cannot both see MaxServerApps-1. With in.LinkTo the new app is
// linked to that app in the same transaction (linkAtInstall): a refused link
// installs nothing. Audited plugin.server.installed as by (with linked_to).
// The caller gates who may install (an admin with a fresh step-up, or the
// command line).
func (s *PGStore) InstallServerApp(ctx context.Context, in ServerAppIn, by users.Actor) (string, Plugin, error) {
	label := strings.TrimSpace(in.AppLabel)
	if err := ValidName(in.Name); err != nil {
		return "", Plugin{}, err
	}
	if err := ValidAppLabel(label); err != nil {
		return "", Plugin{}, err
	}
	if err := ValidAppIcon(in.AppIcon); err != nil {
		return "", Plugin{}, err
	}
	if _, err := ValidViewerLink(in.Origins, in.Origin, in.EntryURL); err != nil {
		return "", Plugin{}, fmt.Errorf("%w: %w", ErrServerAppOrigin, err)
	}
	caps := []Capability{CapApp}
	if in.Tasks {
		caps = append(caps, CapTasks)
	}
	tok, err := NewToken()
	if err != nil {
		return "", Plugin{}, err
	}
	installedBy, via := serverAppActor(by)
	sys := authz.System(ctx)
	var p Plugin
	err = s.inTx(sys, func(tx pgx.Tx) error {
		if _, err := tx.Exec(sys, `SELECT pg_advisory_xact_lock(hashtextextended('muster.plugin_server_apps', 0))`); err != nil {
			return err
		}
		var live int
		if err := tx.QueryRow(sys, `SELECT count(*) FROM plugins WHERE scope = 'server' AND revoked_at IS NULL`).Scan(&live); err != nil {
			return err
		}
		if live >= MaxServerApps {
			return invalid("this server already has %d apps; uninstall one first", MaxServerApps)
		}
		sp, err := tx.Begin(sys)
		if err != nil {
			return err
		}
		p, err = scanPlugin(sp.QueryRow(sys, `
			INSERT INTO plugins (owner_id, scope, name, label, token_hash, data_scope, capabilities, origin, entry_url, app_label, app_icon,
			                     installed_by, installed_via)
			VALUES (NULL, 'server', $1, $2, $3, $4, $5, $6, $7, $2, $8, $9, $10) RETURNING `+pluginCols,
			in.Name, label, HashToken(tok), ViewerOnlyDataScope, caps, in.Origin, in.EntryURL, in.AppIcon, installedBy, via))
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "plugins_server_name_live" {
			_ = sp.Rollback(sys)
			return invalid("this server already has an app named %q; uninstall it first", in.Name)
		}
		if err != nil {
			_ = sp.Rollback(sys)
			return err
		}
		if err := sp.Commit(sys); err != nil {
			return err
		}
		detail := serverAppDetail(p)
		if in.LinkTo != 0 {
			if err := linkAtInstall(sys, tx, p.ID, in.LinkTo); err != nil {
				return err
			}
			detail["linked_to"] = in.LinkTo
		}
		return users.AuditTx(sys, tx, by, "plugin.server.installed", Object(p.ID), detail)
	})
	if err != nil {
		return "", Plugin{}, err
	}
	return tok, p, nil
}

// serverAppDetail is a server-app audit row's detail: name, origin, entry
// URL, capabilities.
func serverAppDetail(p Plugin) map[string]any {
	d := map[string]any{"name": p.Name, "capabilities": p.Capabilities}
	if p.Origin != nil {
		d["origin"] = *p.Origin
	}
	if p.EntryURL != nil {
		d["entry_url"] = *p.EntryURL
	}
	return d
}

// RotateServerApp gives live server app id a NEW token, killing the old one
// at once, and returns it once. 🔴 THE SAME ROW (fork 7): its id, its
// subjects and its grants stay, so the plugin's archive stays reachable.
// ErrNotFound for anything but a live server app. Audited
// plugin.server.rotated as by.
func (s *PGStore) RotateServerApp(ctx context.Context, id int64, by users.Actor) (string, Plugin, error) {
	tok, err := NewToken()
	if err != nil {
		return "", Plugin{}, err
	}
	sys := authz.System(ctx)
	var p Plugin
	err = s.inTx(sys, func(tx pgx.Tx) error {
		var err error
		p, err = scanPlugin(tx.QueryRow(sys, `UPDATE plugins SET token_hash = $2
			WHERE id = $1 AND scope = 'server' AND revoked_at IS NULL RETURNING `+pluginCols, id, HashToken(tok)))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return users.AuditTx(sys, tx, by, "plugin.server.rotated", Object(p.ID), serverAppDetail(p))
	})
	if err != nil {
		return "", Plugin{}, err
	}
	return tok, p, nil
}

// UninstallServerApp revokes live server app id and, IN THE SAME
// TRANSACTION, deletes its grants (plugin_server_users), its task ledger
// (plugin_server_tasks: the tasks themselves stay in each person's list, with
// the comments the app wrote), its unspent codes and its linked pair
// (plugin_server_pairs: one DELETE, the mutual key cascading to the mirror;
// the partner keeps its subjects and is linked to nothing, and a reinstall
// is a new id, linked only if installed --link-to). Revocation is an UPDATE,
// so these deletes are explicit: the ON DELETE CASCADE foreign keys fire only
// if the row itself is ever deleted. From then on the token is 401 on every
// route. Uninstall is FINAL (fork 7): a reinstall is a new row with new
// subjects. ErrNotFound for anything but a live server app. Audited
// plugin.server.uninstalled as by. The caller withdraws the knocks still out
// (internal/api uninstallServerApp).
func (s *PGStore) UninstallServerApp(ctx context.Context, id int64, by users.Actor) (Plugin, error) {
	sys := authz.System(ctx)
	var p Plugin
	err := s.inTx(sys, func(tx pgx.Tx) error {
		var err error
		p, err = scanPlugin(tx.QueryRow(sys, `UPDATE plugins SET revoked_at = now()
			WHERE id = $1 AND scope = 'server' AND revoked_at IS NULL RETURNING `+pluginCols, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM plugin_server_users WHERE plugin_id = $1`,
			`DELETE FROM plugin_server_tasks WHERE plugin_id = $1`,
			`DELETE FROM plugin_handoffs WHERE server_plugin_id = $1 AND redeemed_at IS NULL`,
		} {
			if _, err := tx.Exec(sys, q, id); err != nil {
				return err
			}
		}
		detail := serverAppDetail(p)
		var partner int64
		switch err := tx.QueryRow(sys, `DELETE FROM plugin_server_pairs WHERE plugin_id = $1 RETURNING partner_id`, id).Scan(&partner); {
		case err == nil:
			detail["unlinked_from"] = partner
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		return users.AuditTx(sys, tx, by, "plugin.server.uninstalled", Object(p.ID), detail)
	})
	return p, err
}

// SetServerAppHidden hides (hidden true) or shows server app id for the
// principal, a PERSON (fork 4), and reports whether it changed anything.
//
// 🔴 HIDE ENDS THE GRANT AT ONCE: an upsert on (plugin_id, owner_id) setting
// hidden_at AND act_until to now. When the person never opened the app there
// is no row yet, so Hide inserts one with subjectFor's subject (a new random
// one, or a linked partner's for the person), signin_at NULL, act_until and hidden_at now — a row serverGrantLive can never pass
// (signin_at IS NULL alone fails it), whose subject is sent nowhere.
// SHOW CLEARS hidden_at ONLY: the tab is back, but the app may not act for
// the person until a FRESH redeem (they open the tab again). Under
// authz.System with the principal's own id as owner_id (the server row is
// hidden from the person). ErrNotFound for anything but a live server app, or
// a principal that is not a person. Audited plugin.server.hidden / .shown.
func (s *PGStore) SetServerAppHidden(ctx context.Context, id int64, hidden bool, now time.Time) (bool, error) {
	who, ok := authz.From(ctx)
	if !ok || who.UserID <= 0 || who.Team || who.All() {
		return false, ErrNotFound
	}
	sys := authz.System(ctx)
	changed := false
	err := s.inTx(sys, func(tx pgx.Tx) error {
		var live bool
		if err := tx.QueryRow(sys, `SELECT EXISTS (SELECT 1 FROM plugins WHERE id = $1 AND scope = 'server' AND revoked_at IS NULL)`,
			id).Scan(&live); err != nil {
			return err
		}
		if !live {
			return ErrNotFound
		}
		var tag pgconn.CommandTag
		var err error
		if hidden {
			sub, serr := subjectFor(sys, tx, id, who.UserID)
			if serr != nil {
				return serr
			}
			tag, err = tx.Exec(sys, `INSERT INTO plugin_server_users (plugin_id, owner_id, subject, signin_at, act_until, hidden_at)
				VALUES ($1, $2, $3, NULL, $4, $4)
				ON CONFLICT (plugin_id, owner_id) DO UPDATE
				   SET hidden_at = $4, act_until = least(plugin_server_users.act_until, $4)
				 WHERE plugin_server_users.hidden_at IS NULL`, id, who.UserID, sub, now)
		} else {
			tag, err = tx.Exec(sys, `UPDATE plugin_server_users SET hidden_at = NULL
				WHERE plugin_id = $1 AND owner_id = $2 AND hidden_at IS NOT NULL`, id, who.UserID)
		}
		if err != nil {
			return err
		}
		if changed = tag.RowsAffected() > 0; !changed {
			return nil
		}
		action := "plugin.server.shown"
		if hidden {
			action = "plugin.server.hidden"
		}
		return users.AuditTx(sys, tx, users.Actor{UserID: who.UserID, Via: who.Via}, action, Object(id), nil)
	})
	return changed, err
}

// serverAppAtEntry is fork 9's read: the tab label of a LIVE server app whose
// entry URL is entry, and true. 🔴 AN EXPLICIT authz.System READ (System
// ledger), ON ITS OWN CONNECTION: a server row is hidden from the minting
// person, so under their own principal — which the mint transaction's
// connection carries, whatever context a statement on it is given — this
// would see none and allow every mint.
func (s *PGStore) serverAppAtEntry(ctx context.Context, entry string) (string, bool, error) {
	var label string
	err := s.pool.QueryRow(authz.System(ctx), `SELECT coalesce(app_label, name) FROM plugins
		WHERE scope = 'server' AND revoked_at IS NULL AND entry_url = $1 ORDER BY id LIMIT 1`, entry).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return label, err == nil, err
}

// ServerTaskLedger is one (server app, person, key) entry of
// plugin_server_tasks: what a server app's POST /plugin/v1/tasks resolves a key
// through (notes.Store FileOnceByLedger), so a task the person or another
// plugin tagged with the same key is never found, commented on or revealed.
// It is written under the PERSON's principal (the act-for request's), and
// row-level security confines it to that person's rows.
type ServerTaskLedger struct {
	App, Owner int64
	Key        string
}

// LockKey is the advisory lock that serialises one (app, person, key), so
// two retries file one task.
func (l ServerTaskLedger) LockKey() string {
	return "muster.plugin_server_task:" + strconv.FormatInt(l.App, 10) + ":" + strconv.FormatInt(l.Owner, 10) + ":" + l.Key
}

// TaskKey is the key the ledger is for: the new task's tag.
func (l ServerTaskLedger) TaskKey() string { return l.Key }

// Find is the task this app filed under the key for this person, if any.
func (l ServerTaskLedger) Find(ctx context.Context, tx pgx.Tx) (int64, bool, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT task_id FROM plugin_server_tasks WHERE plugin_id = $1 AND owner_id = $2 AND key = $3`,
		l.App, l.Owner, l.Key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// Record points the key at task id: inserted, or RE-POINTED when the earlier
// task was completed, dismissed or merged away (the upsert). 🔴 The task must
// be the ledger row's person's (the simple FK cannot say so): a task of
// anyone else writes nothing and is an error.
func (l ServerTaskLedger) Record(ctx context.Context, tx pgx.Tx, id int64) error {
	tag, err := tx.Exec(ctx, `INSERT INTO plugin_server_tasks (plugin_id, owner_id, key, task_id)
		SELECT $1, $2, $3, n.id FROM notes n WHERE n.id = $4 AND n.owner_id = $2
		ON CONFLICT (plugin_id, owner_id, key) DO UPDATE SET task_id = EXCLUDED.task_id`, l.App, l.Owner, l.Key, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("plugins: a server task ledger row must name its own person's task")
	}
	return nil
}

// serverAppMintRefusal is fork 9's mint rule (CreatePlugin calls it before its
// transaction: it takes a pool connection of its own, which must never be
// taken while a transaction holds a lock others queue on) for a personal `app` plugin: an
// entry URL a live server app already has is refused, so one person does not
// get two tabs into one plugin under two identities. nil when allowed.
func (s *PGStore) serverAppMintRefusal(ctx context.Context, caps []Capability, entry string) error {
	if !slices.Contains(caps, CapApp) || entry == "" {
		return nil
	}
	label, taken, err := s.serverAppAtEntry(ctx, entry)
	if err != nil {
		return err
	}
	if taken {
		return invalid("this server already offers %s to everyone", label)
	}
	return nil
}

// ServerAppInfo is a live server app as /admin/apps lists it: the row, who
// installed it and how, and how many people hold a LIVE grant now (a count,
// never names), counted by THE gate (LiveServerGrants) so the page cannot
// disagree with act-for.
type ServerAppInfo struct {
	App          Plugin
	InstalledBy  *int64
	InstalledVia string
	LiveGrants   int
	// TokenHash is the row's current token_hash: never shown, it binds the
	// page's one-time form nonce to the token a Rotate replaces.
	TokenHash string
	// LinkedWith is the other app of its linked pair (server_pairs.go), nil
	// when it is in none.
	LinkedWith *LinkedApp
}

// ServerApps is every live server app, oldest first, for an admin's page or
// the command line's list. Under authz.System (a server row is hidden from
// every principal); the caller gates who may read it.
func (s *PGStore) ServerApps(ctx context.Context, origins []OriginPattern, now time.Time) ([]ServerAppInfo, error) {
	sys := authz.System(ctx)
	rows, err := s.pool.Query(sys, `SELECT `+pluginCols+`, installed_by, coalesce(installed_via, ''), token_hash FROM plugins
		WHERE scope = 'server' AND revoked_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []ServerAppInfo
	for rows.Next() {
		var a ServerAppInfo
		if err := rows.Scan(append(pluginDest(&a.App), &a.InstalledBy, &a.InstalledVia, &a.TokenHash)...); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pairs, err := s.serverPairs(ctx)
	if err != nil {
		return nil, err
	}
	apps := make([]Plugin, len(out))
	for i := range out {
		apps[i] = out[i].App
	}
	for i := range out {
		n, err := s.liveGrantCount(ctx, out[i].App, origins, now)
		if err != nil {
			return nil, err
		}
		out[i].LiveGrants = n
		out[i].LinkedWith = linkedFor(out[i].App.ID, pairs, apps)
	}
	return out, nil
}

// liveGrantCount is how many of app's grants THE gate calls live at now: every
// subject that ever redeemed, asked in batches of MaxOwnersCheck.
func (s *PGStore) liveGrantCount(ctx context.Context, app Plugin, origins []OriginPattern, now time.Time) (int, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT subject FROM plugin_server_users WHERE plugin_id = $1 AND signin_at IS NOT NULL ORDER BY id`, app.ID)
	if err != nil {
		return 0, err
	}
	subjects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	for len(subjects) > 0 {
		batch := subjects[:min(len(subjects), MaxOwnersCheck)]
		subjects = subjects[len(batch):]
		got, err := s.LiveServerGrants(ctx, app, origins, batch, now)
		if err != nil {
			return 0, err
		}
		for _, g := range got {
			if g.State == GrantLive {
				n++
			}
		}
	}
	return n, nil
}

// VisibleServerApps is the live server apps ctx's principal gets a tab for,
// oldest first: a PERSON's (never a team's acting principal, decision 14),
// minus any the person hid. Origins are the caller's check, as for a
// personal app plugin. Under authz.System, keyed on the principal's own id.
func (s *PGStore) VisibleServerApps(ctx context.Context) ([]Plugin, error) {
	who, ok := authz.From(ctx)
	if !ok || who.UserID <= 0 || who.Team || who.All() {
		return nil, nil
	}
	rows, err := s.pool.Query(authz.System(ctx), `SELECT `+pluginCols+` FROM plugins p
		WHERE p.scope = 'server' AND p.revoked_at IS NULL AND 'app' = ANY (p.capabilities)
		  AND NOT EXISTS (SELECT 1 FROM plugin_server_users h WHERE h.plugin_id = p.id AND h.owner_id = $1 AND h.hidden_at IS NOT NULL)
		ORDER BY p.id LIMIT $2`, who.UserID, MaxServerApps)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Plugin, error) { return scanPlugin(r) })
}

// AccountServerApp is one live server app as a person's Account → Apps lists
// it: the row, whether the person hid it, and until when it may act for them
// (nil: no live grant now, by THE gate).
type AccountServerApp struct {
	App       Plugin
	Hidden    bool
	LiveUntil *time.Time
	// LinkedWith is the other app of its linked pair, nil when it is in none:
	// the two know the person as the same person.
	LinkedWith *LinkedApp
}

// AccountServerApps is every live server app, oldest first, with ctx's
// principal's own state under each. nil for anyone but a person.
func (s *PGStore) AccountServerApps(ctx context.Context, origins []OriginPattern, now time.Time) ([]AccountServerApp, error) {
	who, ok := authz.From(ctx)
	if !ok || who.UserID <= 0 || who.Team || who.All() {
		return nil, nil
	}
	sys := authz.System(ctx)
	rows, err := s.pool.Query(sys, `SELECT `+prefixed("p", pluginCols)+`, coalesce(g.subject, ''), g.hidden_at IS NOT NULL, g.act_until
		  FROM plugins p LEFT JOIN plugin_server_users g ON g.plugin_id = p.id AND g.owner_id = $1
		 WHERE p.scope = 'server' AND p.revoked_at IS NULL ORDER BY p.id`, who.UserID)
	if err != nil {
		return nil, err
	}
	type row struct {
		a       AccountServerApp
		subject string
		until   *time.Time
	}
	var got []row
	for rows.Next() {
		var x row
		if err := rows.Scan(append(pluginDest(&x.a.App), &x.subject, &x.a.Hidden, &x.until)...); err != nil {
			rows.Close()
			return nil, err
		}
		got = append(got, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pairs, err := s.serverPairs(ctx)
	if err != nil {
		return nil, err
	}
	apps := make([]Plugin, len(got))
	for i := range got {
		apps[i] = got[i].a.App
	}
	out := make([]AccountServerApp, 0, len(got))
	for _, x := range got {
		x.a.LinkedWith = linkedFor(x.a.App.ID, pairs, apps)
		if x.subject != "" && !x.a.Hidden {
			live, err := s.LiveServerGrants(ctx, x.a.App, origins, []string{x.subject}, now)
			if err != nil {
				return nil, err
			}
			if live[x.subject].State == GrantLive && x.until != nil {
				u := x.until.UTC()
				x.a.LiveUntil = &u
			}
		}
		out = append(out, x.a)
	}
	return out, nil
}
