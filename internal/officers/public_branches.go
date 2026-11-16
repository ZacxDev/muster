package officers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// A DRAFT OFFICER'S PUBLIC BRANCHES (docs/officers.md "Public repositories").
//
// In a ship repository marked public, a draft officer's branch names neither
// the officer nor its item (NewPublicBranch). This record is where muster
// keeps both: one row per (officer, repository, item), naming the branch
// muster minted for it. The push gate, the draft gate's body contract, its
// rate limits, the orphan report and the sweep read an officer's public
// branches here, never from the name.
//
// 🔴 THE RECORD IS THE ONLY PROOF OF OWNERSHIP: a public branch muster holds
// no row for, for this officer, is not the officer's — a push to it, a pull
// request from it and a branch deletion of it are refused (fail closed: a
// lookup that errors is a refusal too).
//
// Kept in muster's database (PGPublicBranchStore, migration 0138) when it has
// one, so a restart and a second replica see the same record; else in this
// process's memory (MemPublicBranchStore), which a restart forgets — the
// officer then cannot push to a public branch minted before it.
// ---------------------------------------------------------------------------

// PublicBranchStore keeps the public branches, per officer. ctx acts as the
// officer's owner (the database store is under row-level security).
type PublicBranchStore interface {
	// PublicBranchMint records branch as officer's for item in repo and
	// returns it — or, when officer already has a branch for item in repo,
	// returns THAT one and records nothing (one branch per item and
	// repository).
	PublicBranchMint(ctx context.Context, officer, repo string, item int, branch string) (string, error)
	// PublicBranchItem is the item officer's branch in repo is for; ok false
	// when muster holds no such record.
	PublicBranchItem(ctx context.Context, officer, repo, branch string) (item int, ok bool, err error)
	// PublicBranches lists officer's branches in repo.
	PublicBranches(ctx context.Context, officer, repo string) ([]string, error)
}

// MemPublicBranchStore is PublicBranchStore in this process's memory.
type MemPublicBranchStore struct {
	mu     sync.Mutex
	byItem map[string]string // officer repo #item -> branch
	byName map[string]int    // officer repo branch -> item
}

// NewMemPublicBranchStore is an empty memory store.
func NewMemPublicBranchStore() *MemPublicBranchStore {
	return &MemPublicBranchStore{byItem: map[string]string{}, byName: map[string]int{}}
}

func pbItemKey(officer, repo string, item int) string {
	return officer + " " + strings.ToLower(repo) + " #" + strconv.Itoa(item)
}

func pbNameKey(officer, repo, branch string) string {
	return officer + " " + strings.ToLower(repo) + " " + branch
}

// PublicBranchMint implements PublicBranchStore.
func (m *MemPublicBranchStore) PublicBranchMint(_ context.Context, officer, repo string, item int, branch string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.byItem[pbItemKey(officer, repo, item)]; ok {
		return b, nil
	}
	if _, taken := m.byName[pbNameKey(officer, repo, branch)]; taken {
		return "", errors.New("officers: that public branch name is already recorded for another item")
	}
	m.byItem[pbItemKey(officer, repo, item)] = branch
	m.byName[pbNameKey(officer, repo, branch)] = item
	return branch, nil
}

// PublicBranchItem implements PublicBranchStore.
func (m *MemPublicBranchStore) PublicBranchItem(_ context.Context, officer, repo, branch string) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.byName[pbNameKey(officer, repo, branch)]
	return n, ok, nil
}

// PublicBranches implements PublicBranchStore.
func (m *MemPublicBranchStore) PublicBranches(_ context.Context, officer, repo string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := officer + " " + strings.ToLower(repo) + " "
	var out []string
	for k := range m.byName {
		if b, ok := strings.CutPrefix(k, prefix); ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// PGPublicBranchStore is PublicBranchStore on officer_public_branches
// (migration 0138), under row-level security: ctx must act as the officer's
// owner.
type PGPublicBranchStore struct{ pool *pgxpool.Pool }

// NewPGPublicBranchStore is the store over pool.
func NewPGPublicBranchStore(pool *pgxpool.Pool) *PGPublicBranchStore {
	return &PGPublicBranchStore{pool: pool}
}

// PublicBranchMint implements PublicBranchStore: an INSERT that keeps an
// existing row for the item (the unique key), then the row that holds.
func (s *PGPublicBranchStore) PublicBranchMint(ctx context.Context, officer, repo string, item int, branch string) (string, error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO officer_public_branches (officer, repo, item, branch) VALUES ($1, lower($2), $3, $4)
		ON CONFLICT (owner_id, officer, repo, item) DO NOTHING`, officer, repo, item, branch); err != nil {
		return "", err
	}
	var got string
	err := s.pool.QueryRow(ctx, `SELECT branch FROM officer_public_branches WHERE officer = $1 AND repo = lower($2) AND item = $3`,
		officer, repo, item).Scan(&got)
	return got, err
}

// PublicBranchItem implements PublicBranchStore.
func (s *PGPublicBranchStore) PublicBranchItem(ctx context.Context, officer, repo, branch string) (int, bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT item FROM officer_public_branches WHERE officer = $1 AND repo = lower($2) AND branch = $3`,
		officer, repo, branch).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// PublicBranches implements PublicBranchStore.
func (s *PGPublicBranchStore) PublicBranches(ctx context.Context, officer, repo string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT branch FROM officer_public_branches WHERE officer = $1 AND repo = lower($2) ORDER BY branch`, officer, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
