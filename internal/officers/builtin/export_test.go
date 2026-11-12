package builtin

import "github.com/jackc/pgx/v5/pgxpool"

// FaultDoubleClaim turns on the conformance suite's fault: a claim of an
// item another instance holds under a live lease succeeds. Test-only.
func (b *Backend) FaultDoubleClaim() { b.faultDoubleClaim = true }

// Pool is the store's pool. Test-only.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }
