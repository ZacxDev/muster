package plugins

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// LINKED SERVER APPS (docs/design/contacts.md, "The core change: linked server
// apps"; migration 0178; docs/plugins.md#linked-server-apps).
//
// An admin may install a server app LINKED to one already installed
// (`muster-server app install … --link-to <id>`), and the pair then knows
// each person by ONE subject: whichever of the two first needs a subject for
// a person draws it, and the other takes it. Every unlinked app keeps
// pairwise subjects.
//
//   - 🔴 ONE FUNCTION CHOOSES A SUBJECT: subjectFor. Both writers of a
//     plugin_server_users row (the redeem and Hide) call it, and it is the one
//     caller of newSubject (internal/api TestOneFunctionChoosesASubject pins
//     both from the Go AST), so no third writer can skip the pair.
//   - 🔴 A PAIR IS MADE ONLY AT INSTALL, in the install's own transaction
//     (InstallServerApp with LinkTo), so the new app knows nobody when it is
//     linked, and nothing ever rewrites a subject: no person's data under
//     either app is ever moved under another subject or orphaned. Two apps
//     already in use are never linked, and an app whose subjects are already
//     shared (a former pair's, after an unlink) is never a source: a person's
//     subject is shared only between the two apps of one link.
//   - EVERYTHING ELSE STAYS PER APP: the grant, Hide, owners/check, the act-for
//     door and the task ledger are keyed on (plugin_id, …) as before. A shared
//     subject lets the two apps join their own records; it does not let one
//     app's token act through the other's grant (LiveServerGrants keys on the
//     authenticated app's id).
//   - plugin_server_pairs has no owner (forced row-level security, scope_all
//     only): every statement on it here runs as authz.System.

// subjectFor is THE ONE CHOICE of the subject app appID gives person ownerID
// when it first needs one (the redeem's grant upsert, Hide's row for a person
// who never opened the app): the subject the person already has under app's
// linked PARTNER, or, when app is in no pair or the partner has no row for
// the person, a new random one (newSubject). tx must be an authz.System
// transaction. When the row already exists, its writer keeps that row's
// subject and ignores this one.
//
// 🔴 THE RACE: two first opens for one person, one per app of a pair, at the
// same moment, would each find no partner row and draw two subjects. So
// subjectFor first takes a transaction-scoped advisory lock on the PERSON:
// the second waits until the first commits, and its read (a new statement, so
// a new snapshot under READ COMMITTED) then finds the first's row. The lock
// is keyed on the person alone, not the pair, so the "is app paired" read is
// the same one statement as the partner read, made under the lock, with no
// separate path for an unlinked app; it serialises only one person's own
// concurrent first opens.
func subjectFor(ctx context.Context, tx pgx.Tx, appID, ownerID int64) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.plugin_server_subject:' || $1::bigint, 0))`, ownerID); err != nil {
		return "", err
	}
	var sub string
	err := tx.QueryRow(ctx, `SELECT g.subject FROM plugin_server_pairs p
		  JOIN plugin_server_users g ON g.plugin_id = p.partner_id AND g.owner_id = $2
		 WHERE p.plugin_id = $1`, appID, ownerID).Scan(&sub)
	switch {
	case err == nil:
		return sub, nil
	case errors.Is(err, pgx.ErrNoRows):
		return newSubject()
	default:
		return "", err
	}
}

// linkAtInstall is InstallServerApp's --link-to: inside the install's own
// transaction tx (holding the server-app advisory lock), after the new app's
// row is inserted, it pairs app with live server app source. The source row
// is held FOR SHARE, so an uninstall racing the install waits for it (or wins
// first, and this refuses). Refused, each in its own words: no such row, a
// personal plugin, an uninstalled app, an app whose subjects another live
// server app already holds (a former pair's, after an unlink), an app
// already in a pair (the pair's primary key). Any refusal fails the whole install: neither the app nor a
// pair is left. The new app has no plugin_server_users row: it was created a
// moment ago in this same transaction.
func linkAtInstall(ctx context.Context, tx pgx.Tx, app, source int64) error {
	var scope string
	var revoked *time.Time
	err := tx.QueryRow(ctx, `SELECT scope, revoked_at FROM plugins WHERE id = $1 FOR SHARE`, source).Scan(&scope, &revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return invalid("there is no app %d to link to", source)
	case err != nil:
		return err
	case scope != ScopeServer:
		return invalid("app %d is a personal plugin; only server apps can be linked", source)
	case revoked != nil:
		return invalid("app %d is uninstalled", source)
	}
	// 🔴 A SOURCE WHOSE SUBJECTS ARE ALREADY SHARED is refused: after an
	// unlink, the source and its former partner still hold one subject per
	// person they both knew, and a new app linked to the source would take
	// those subjects too — joining it with the former partner, which nobody
	// linked to it and Account → Apps would never name. So "pairwise unless
	// linked" holds only if no subject of the source is held by any other
	// live server app.
	var sharedWith int64
	err = tx.QueryRow(ctx, `SELECT o.plugin_id FROM plugin_server_users s
		  JOIN plugin_server_users o ON o.subject = s.subject AND o.plugin_id <> s.plugin_id
		  JOIN plugins p ON p.id = o.plugin_id AND p.scope = 'server' AND p.revoked_at IS NULL
		 WHERE s.plugin_id = $1 ORDER BY o.plugin_id LIMIT 1`, source).Scan(&sharedWith)
	switch {
	case err == nil:
		return invalid("app %d already shares its people's subjects with app %d from an earlier link, so a new app linked to it "+
			"would share them with app %d too; install the new app without --link-to instead", source, sharedWith, sharedWith)
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	_, err = sp.Exec(ctx, `INSERT INTO plugin_server_pairs (plugin_id, partner_id) VALUES ($1, $2), ($2, $1)`, app, source)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		_ = sp.Rollback(ctx)
		var partner int64
		if qerr := tx.QueryRow(ctx, `SELECT partner_id FROM plugin_server_pairs WHERE plugin_id = $1`, source).Scan(&partner); qerr != nil {
			return qerr
		}
		return invalid("app %d is already linked with app %d; unlink it first", source, partner)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

// UnlinkServerApp ends the pair server app id is in, and returns the other
// app of it. It deletes the pair (one row; plugin_server_pairs_mutual
// cascades to its mirror) and NOTHING ELSE: people already known to both
// keep the one subject under both (what the two apps have learned cannot be
// unlearned, and re-keying would only orphan their data); a person who first
// opens either app afterwards gets an independent subject under each.
// Refused "app N is not linked" when it is in no pair. Audited
// plugin.server.unlinked as by.
func (s *PGStore) UnlinkServerApp(ctx context.Context, id int64, by users.Actor) (int64, error) {
	sys := authz.System(ctx)
	var partner int64
	err := s.inTx(sys, func(tx pgx.Tx) error {
		err := tx.QueryRow(sys, `DELETE FROM plugin_server_pairs WHERE plugin_id = $1 RETURNING partner_id`, id).Scan(&partner)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("app %d is not linked", id)
		}
		if err != nil {
			return err
		}
		return users.AuditTx(sys, tx, by, "plugin.server.unlinked", Object(id), map[string]any{"partner": partner})
	})
	return partner, err
}

// serverPairs is every app in a pair, with its partner. Under authz.System
// (the table has no owner); at most MaxServerApps rows.
func (s *PGStore) serverPairs(ctx context.Context) (map[int64]int64, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT plugin_id, partner_id FROM plugin_server_pairs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var app, partner int64
		if err := rows.Scan(&app, &partner); err != nil {
			return nil, err
		}
		out[app] = partner
	}
	return out, rows.Err()
}

// serverAppLabel is a server app's tab label, or its name when it has none.
func serverAppLabel(p Plugin) string {
	if p.AppLabel != nil && *p.AppLabel != "" {
		return *p.AppLabel
	}
	return p.Name
}

// LinkedApp is the other app of a server app's pair, as the lists show it.
type LinkedApp struct {
	ID    int64
	Label string
}

// linkedFor is app id's LinkedApp from pairs, naming the partner from apps
// (every live server app; a pair's apps are both live, since an uninstall
// deletes the pair). nil when id is in no pair.
func linkedFor(id int64, pairs map[int64]int64, apps []Plugin) *LinkedApp {
	partner, ok := pairs[id]
	if !ok {
		return nil
	}
	l := &LinkedApp{ID: partner, Label: "app " + strconv.FormatInt(partner, 10)}
	for _, p := range apps {
		if p.ID == partner {
			l.Label = serverAppLabel(p)
		}
	}
	return l
}
