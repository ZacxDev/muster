package officers

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// HOST SPAWNS (migration 0125; docs/design/officers-on-hosts.md): one row per
// host-spawned session muster asked a mirror host to start for an instance of
// a RuntimeHost officer.
//
// 🔴 STATE COMES FROM THIS TABLE AND THE HOST'S LISTING, NOT FROM MEMORY — the
// wake loop's own rule. A restarted muster reads which spawn is each
// instance's latest here, and whether its window is alive from the host's
// listing, so it never starts a second window beside a live one.
//
// 🔴 THE ID IS A HOST DISPATCH ID ON THE WIRE. A row's id travels as the
// spawn's `dispatch` (the window's @muster-dispatch, the worktree directory,
// the branch muster/<id>-<slug>), so it is drawn from host_dispatches' own
// sequence: it can never equal a chief dispatch's id on any host.
//
// 🔴 THREE WRITERS, EACH WITH ITS NAMED CALLER (a ledger test asserts them over
// internal/api): InsertHostSpawn (the fleet's Start), and RecordHostSpawnWindow
// and RecordHostSpawnRefusal (the one reply application, for the waiter and
// the hub's late-reply hook alike). The two records write only a row whose
// window and refusal are both still empty, on the row's own host: a late or
// repeated reply never moves a recorded window.
// ---------------------------------------------------------------------------

// HostSpawn is one row.
type HostSpawn struct {
	ID int64
	// Officer is the officer id, Instance the instance agent name
	// (InstanceName) the spawn is for, Host the mirror host it was sent to.
	Officer, Instance, Host string
	// Window and AgentSession are what the host's reply named: the tmux window
	// id and the Claude session id it chose. "" until the reply.
	Window, AgentSession string
	// Refusal is the host's refusal ("code: detail"), "" for none.
	Refusal   string
	CreatedAt time.Time
}

// ErrHostSpawnNotFound: no row of the principal's matches.
var ErrHostSpawnNotFound = errors.New("officers: no such host spawn")

// HostSpawnStore keeps the host spawns. Every call acts as the principal on
// ctx: row-level security confines it to that owner's rows.
type HostSpawnStore interface {
	// InsertHostSpawn records a spawn about to be sent and returns its row.
	InsertHostSpawn(ctx context.Context, officer, instance, host string) (HostSpawn, error)
	// RecordHostSpawnWindow records the host's reply on row id of host,
	// reporting whether it wrote (false: a window or refusal is already
	// recorded, or the row is not host's).
	RecordHostSpawnWindow(ctx context.Context, id int64, host, window, session string) (bool, error)
	// RecordHostSpawnRefusal records the host's refusal on row id of host,
	// under the same rule.
	RecordHostSpawnRefusal(ctx context.Context, id int64, host, refusal string) (bool, error)
	// LatestHostSpawns is officer's newest row per instance name.
	LatestHostSpawns(ctx context.Context, officer string) (map[string]HostSpawn, error)
	// HostSpawnByID is row id.
	HostSpawnByID(ctx context.Context, id int64) (HostSpawn, error)
	// HostSpawnForSession is the newest row of host whose recorded Claude
	// session is session: how a mirrored caller is found (never by its
	// window's tag, which anything in the window can set).
	HostSpawnForSession(ctx context.Context, host, session string) (HostSpawn, error)
	// HostSpawnIDs is which of ids are host spawns of the principal's on host.
	HostSpawnIDs(ctx context.Context, host string, ids []int64) (map[int64]bool, error)
}

// PGHostSpawnStore is HostSpawnStore over muster's Postgres.
type PGHostSpawnStore struct{ pool *pgxpool.Pool }

// NewPGHostSpawnStore is the store over pool.
func NewPGHostSpawnStore(pool *pgxpool.Pool) *PGHostSpawnStore { return &PGHostSpawnStore{pool: pool} }

const hostSpawnCols = `id, officer, instance, host, tmux_window, agent_session, refusal, created_at`

func scanHostSpawn(row pgx.Row) (HostSpawn, error) {
	var h HostSpawn
	err := row.Scan(&h.ID, &h.Officer, &h.Instance, &h.Host, &h.Window, &h.AgentSession, &h.Refusal, &h.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return HostSpawn{}, ErrHostSpawnNotFound
	}
	return h, err
}

// InsertHostSpawn implements HostSpawnStore.
func (s *PGHostSpawnStore) InsertHostSpawn(ctx context.Context, officer, instance, host string) (HostSpawn, error) {
	return scanHostSpawn(s.pool.QueryRow(ctx, `INSERT INTO officer_host_spawns (officer, instance, host) VALUES ($1, $2, $3)
		RETURNING `+hostSpawnCols, officer, instance, host))
}

// RecordHostSpawnWindow implements HostSpawnStore.
func (s *PGHostSpawnStore) RecordHostSpawnWindow(ctx context.Context, id int64, host, window, session string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE officer_host_spawns SET tmux_window = $3, agent_session = $4
		WHERE id = $1 AND host = $2 AND tmux_window = '' AND refusal = ''`, id, host, window, session)
	return tag.RowsAffected() == 1, err
}

// RecordHostSpawnRefusal implements HostSpawnStore.
func (s *PGHostSpawnStore) RecordHostSpawnRefusal(ctx context.Context, id int64, host, refusal string) (bool, error) {
	if len(refusal) > 500 {
		refusal = refusal[:500]
	}
	tag, err := s.pool.Exec(ctx, `UPDATE officer_host_spawns SET refusal = $3
		WHERE id = $1 AND host = $2 AND tmux_window = '' AND refusal = ''`, id, host, refusal)
	return tag.RowsAffected() == 1, err
}

// LatestHostSpawns implements HostSpawnStore.
func (s *PGHostSpawnStore) LatestHostSpawns(ctx context.Context, officer string) (map[string]HostSpawn, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (instance) `+hostSpawnCols+` FROM officer_host_spawns
		WHERE officer = $1 ORDER BY instance, id DESC`, officer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]HostSpawn{}
	for rows.Next() {
		h, err := scanHostSpawn(rows)
		if err != nil {
			return nil, err
		}
		out[h.Instance] = h
	}
	return out, rows.Err()
}

// HostSpawnByID implements HostSpawnStore.
func (s *PGHostSpawnStore) HostSpawnByID(ctx context.Context, id int64) (HostSpawn, error) {
	return scanHostSpawn(s.pool.QueryRow(ctx, `SELECT `+hostSpawnCols+` FROM officer_host_spawns WHERE id = $1`, id))
}

// HostSpawnForSession implements HostSpawnStore.
func (s *PGHostSpawnStore) HostSpawnForSession(ctx context.Context, host, session string) (HostSpawn, error) {
	if session == "" {
		return HostSpawn{}, ErrHostSpawnNotFound
	}
	return scanHostSpawn(s.pool.QueryRow(ctx, `SELECT `+hostSpawnCols+` FROM officer_host_spawns
		WHERE host = $1 AND agent_session = $2 ORDER BY id DESC LIMIT 1`, host, session))
}

// HostSpawnIDs implements HostSpawnStore.
func (s *PGHostSpawnStore) HostSpawnIDs(ctx context.Context, host string, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM officer_host_spawns WHERE host = $1 AND id = ANY($2)`, host, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// MemHostSpawnStore is HostSpawnStore in memory, for tests (one owner).
// NextID is the next id it hands out (it starts at 1000, well clear of the
// small ids a test's chief dispatches use).
type MemHostSpawnStore struct {
	mu     sync.Mutex
	rows   []HostSpawn
	NextID int64
	Now    func() time.Time
}

// NewMemHostSpawnStore returns an empty store.
func NewMemHostSpawnStore() *MemHostSpawnStore { return &MemHostSpawnStore{NextID: 1000} }

func (m *MemHostSpawnStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// InsertHostSpawn implements HostSpawnStore.
func (m *MemHostSpawnStore) InsertHostSpawn(_ context.Context, officer, instance, host string) (HostSpawn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := HostSpawn{ID: m.NextID, Officer: officer, Instance: instance, Host: host, CreatedAt: m.now()}
	m.NextID++
	m.rows = append(m.rows, h)
	return h, nil
}

func (m *MemHostSpawnStore) record(id int64, host string, set func(*HostSpawn)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		r := &m.rows[i]
		if r.ID == id && r.Host == host && r.Window == "" && r.Refusal == "" {
			set(r)
			return true
		}
	}
	return false
}

// RecordHostSpawnWindow implements HostSpawnStore.
func (m *MemHostSpawnStore) RecordHostSpawnWindow(_ context.Context, id int64, host, window, session string) (bool, error) {
	return m.record(id, host, func(r *HostSpawn) { r.Window, r.AgentSession = window, session }), nil
}

// RecordHostSpawnRefusal implements HostSpawnStore.
func (m *MemHostSpawnStore) RecordHostSpawnRefusal(_ context.Context, id int64, host, refusal string) (bool, error) {
	return m.record(id, host, func(r *HostSpawn) { r.Refusal = refusal }), nil
}

// LatestHostSpawns implements HostSpawnStore.
func (m *MemHostSpawnStore) LatestHostSpawns(_ context.Context, officer string) (map[string]HostSpawn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]HostSpawn{}
	for _, r := range m.rows {
		if r.Officer == officer && r.ID > out[r.Instance].ID {
			out[r.Instance] = r
		}
	}
	return out, nil
}

// HostSpawnByID implements HostSpawnStore.
func (m *MemHostSpawnStore) HostSpawnByID(_ context.Context, id int64) (HostSpawn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return HostSpawn{}, ErrHostSpawnNotFound
}

// HostSpawnForSession implements HostSpawnStore.
func (m *MemHostSpawnStore) HostSpawnForSession(_ context.Context, host, session string) (HostSpawn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best HostSpawn
	for _, r := range m.rows {
		if session != "" && r.Host == host && r.AgentSession == session && r.ID > best.ID {
			best = r
		}
	}
	if best.ID == 0 {
		return HostSpawn{}, ErrHostSpawnNotFound
	}
	return best, nil
}

// HostSpawnIDs implements HostSpawnStore.
func (m *MemHostSpawnStore) HostSpawnIDs(_ context.Context, host string, ids []int64) (map[int64]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int64]bool{}
	for _, r := range m.rows {
		for _, id := range ids {
			if r.ID == id && r.Host == host {
				out[id] = true
			}
		}
	}
	return out, nil
}

// All is every row, by id (tests).
func (m *MemHostSpawnStore) All() []HostSpawn {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]HostSpawn(nil), m.rows...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
