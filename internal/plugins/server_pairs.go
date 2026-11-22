package plugins

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// LINKED SERVER APPS (docs/design/contacts.md, "The core change: linked server
// apps"; migration 0178; docs/plugins.md#linked-server-apps).
//
// An admin may LINK two installed server apps (`muster-server app link
// <source> <adopter>`), and a linked pair then knows each person by ONE
// subject: whichever of the two first needs a subject for a person draws it,
// and the other takes it. Every unlinked app keeps pairwise subjects.
//
//   - 🔴 ONE FUNCTION CHOOSES A SUBJECT: subjectFor. Both writers of a
//     plugin_server_users row (the redeem and Hide) call it, and it is the one
//     caller of newSubject (internal/api TestOneFunctionChoosesASubject pins
//     both from the Go AST), so no third writer can skip the pair.
//   - 🔴 THE ADOPTER MUST KNOW NOBODY AT LINK TIME (LinkServerApps refuses it
//     while the adopter has any plugin_server_users row), and nothing ever
//     rewrites a subject, so no person's data under either app is ever moved
//     under another subject or orphaned.
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
		  JOIN plugin_server_users g ON g.owner_id = $2
		   AND g.plugin_id = CASE WHEN p.low_id = $1 THEN p.high_id ELSE p.low_id END
		 WHERE $1 IN (p.low_id, p.high_id)`, appID, ownerID).Scan(&sub)
	switch {
	case err == nil:
		return sub, nil
	case errors.Is(err, pgx.ErrNoRows):
		return newSubject()
	default:
		return "", err
	}
}

// ServerPair is a linked pair as LinkServerApps made it: the source (whose
// subjects were kept), the adopter (which takes them), and how many people
// the source already knew.
type ServerPair struct {
	Source, Adopter Plugin
	// Known is how many plugin_server_users rows the source had.
	Known int
}

// LinkServerApps links live server apps source and adopter: from now on the
// two know each person by one subject (subjectFor). One System transaction,
// serialised with every install and link by the server-app advisory lock,
// holding both plugins rows FOR UPDATE, so an uninstall, a redeem or a Hide
// racing the link waits for it (they hold the app row FOR SHARE). Refused,
// each with its own words: an app linked to itself; an id with no row; a
// personal plugin; an uninstalled app; an adopter that already knows anyone
// (any plugin_server_users row); an app already in a pair. It writes NO
// plugin_server_users row. Audited plugin.server.linked as by. 🔴 THE CALLER
// IS THE COMMAND LINE ONLY: there is no page form and no /plugin/v1 route.
func (s *PGStore) LinkServerApps(ctx context.Context, source, adopter int64, by users.Actor) (ServerPair, error) {
	if source == adopter {
		return ServerPair{}, invalid("an app cannot be linked to itself")
	}
	sys := authz.System(ctx)
	var out ServerPair
	err := s.inTx(sys, func(tx pgx.Tx) error {
		if _, err := tx.Exec(sys, `SELECT pg_advisory_xact_lock(hashtextextended('muster.plugin_server_apps', 0))`); err != nil {
			return err
		}
		rows, err := tx.Query(sys, `SELECT `+pluginCols+` FROM plugins WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, source, adopter)
		if err != nil {
			return err
		}
		got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Plugin, error) { return scanPlugin(r) })
		if err != nil {
			return err
		}
		byID := map[int64]Plugin{}
		for _, p := range got {
			byID[p.ID] = p
		}
		for _, id := range []int64{source, adopter} {
			p, ok := byID[id]
			switch {
			case !ok:
				return invalid("there is no app %d", id)
			case !p.IsServer():
				return invalid("app %d is a personal plugin; only server apps can be linked", id)
			case p.RevokedAt != nil:
				return invalid("app %d (%s) is uninstalled", id, p.Name)
			}
		}
		out.Source, out.Adopter = byID[source], byID[adopter]
		var known int
		if err := tx.QueryRow(sys, `SELECT count(*) FROM plugin_server_users WHERE plugin_id = $1`, adopter).Scan(&known); err != nil {
			return err
		}
		if known > 0 {
			return invalid("%s already knows %d people; uninstall and reinstall it, or link it before anyone opens it", serverAppLabel(out.Adopter), known)
		}
		low, high := min(source, adopter), max(source, adopter)
		sp, err := tx.Begin(sys)
		if err != nil {
			return err
		}
		_, err = sp.Exec(sys, `INSERT INTO plugin_server_pairs (low_id, high_id, source_id) VALUES ($1, $2, $3)`, low, high, source)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			_ = sp.Rollback(sys)
			return s.alreadyPaired(sys, tx, source, adopter)
		}
		if err != nil {
			_ = sp.Rollback(sys)
			return err
		}
		if err := sp.Commit(sys); err != nil {
			return err
		}
		if err := tx.QueryRow(sys, `SELECT count(*) FROM plugin_server_users WHERE plugin_id = $1`, source).Scan(&out.Known); err != nil {
			return err
		}
		return users.AuditTx(sys, tx, by, "plugin.server.linked", Object(adopter), map[string]any{"source": source, "adopter": adopter})
	})
	if err != nil {
		return ServerPair{}, err
	}
	return out, nil
}

// alreadyPaired is LinkServerApps' refusal once the insert met
// plugin_server_pairs_low, _high, _pkey or _one_pair: which of the two apps is
// already in a pair, and with which app.
func (s *PGStore) alreadyPaired(ctx context.Context, tx pgx.Tx, source, adopter int64) error {
	for _, id := range []int64{source, adopter} {
		var partner int64
		err := tx.QueryRow(ctx, `SELECT CASE WHEN low_id = $1 THEN high_id ELSE low_id END FROM plugin_server_pairs
			WHERE $1 IN (low_id, high_id)`, id).Scan(&partner)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		return invalid("app %d is already linked with app %d; unlink it first", id, partner)
	}
	return invalid("one of the two apps is already linked; unlink it first")
}

// UnlinkServerApp ends the pair server app id is in, and returns the other
// app of it. It deletes the pair row and NOTHING ELSE: people already known to
// both keep the one subject under both (what the two apps have learned cannot
// be unlearned, and re-keying would only orphan their data); a person who
// first opens either app afterwards gets an independent subject under each.
// Both plugins rows are held FOR UPDATE, as at the link. Refused "app N is not
// linked" when it is in no pair. Audited plugin.server.unlinked as by.
func (s *PGStore) UnlinkServerApp(ctx context.Context, id int64, by users.Actor) (int64, error) {
	sys := authz.System(ctx)
	var partner int64
	err := s.inTx(sys, func(tx pgx.Tx) error {
		if _, err := tx.Exec(sys, `SELECT pg_advisory_xact_lock(hashtextextended('muster.plugin_server_apps', 0))`); err != nil {
			return err
		}
		var low, high, source int64
		err := tx.QueryRow(sys, `SELECT low_id, high_id, source_id FROM plugin_server_pairs WHERE $1 IN (low_id, high_id)`, id).Scan(&low, &high, &source)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("app %d is not linked", id)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(sys, `SELECT 1 FROM plugins WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, low, high); err != nil {
			return err
		}
		if _, err := tx.Exec(sys, `DELETE FROM plugin_server_pairs WHERE low_id = $1 AND high_id = $2`, low, high); err != nil {
			return err
		}
		partner = low
		if low == id {
			partner = high
		}
		return users.AuditTx(sys, tx, by, "plugin.server.unlinked", Object(id), map[string]any{"apps": []int64{low, high}, "source": source})
	})
	return partner, err
}

// serverPairSide is one app's side of its pair, for the lists: the other app
// and whether this one is the pair's source.
type serverPairSide struct {
	partner int64
	source  bool
}

// serverPairs is every pair, by each of its two apps. Under authz.System (the
// table has no owner); at most MaxServerApps/2 rows.
func (s *PGStore) serverPairs(ctx context.Context) (map[int64]serverPairSide, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT low_id, high_id, source_id FROM plugin_server_pairs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]serverPairSide{}
	for rows.Next() {
		var low, high, source int64
		if err := rows.Scan(&low, &high, &source); err != nil {
			return nil, err
		}
		out[low] = serverPairSide{partner: high, source: source == low}
		out[high] = serverPairSide{partner: low, source: source == high}
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
	// Source: THIS app is the pair's source (its subjects were kept).
	Source bool
}

// linkedFor is app id's LinkedApp from pairs, naming the partner from apps
// (every live server app; a pair's apps are both live, since an uninstall
// deletes the pair). nil when id is in no pair.
func linkedFor(id int64, pairs map[int64]serverPairSide, apps []Plugin) *LinkedApp {
	side, ok := pairs[id]
	if !ok {
		return nil
	}
	l := &LinkedApp{ID: side.partner, Label: "app " + strconv.FormatInt(side.partner, 10), Source: side.source}
	for _, p := range apps {
		if p.ID == side.partner {
			l.Label = serverAppLabel(p)
		}
	}
	return l
}
