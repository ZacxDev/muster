package api

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/sessionwake"
)

// wireWakes gives server s the stores and host session_wake_pg_test.go runs
// against.
func wireWakes(s *Server, m Mirror, w *sessionwake.PGStore, a *attention.PGStore, p *pages.PGStore) {
	s.ext.Mirror, s.ext.SessionWakes, s.ext.Attention, s.ext.Pages = m, w, a, p
}

// wakesOn is the probes' (session_wake_probe_pg_test.go) one line that turns
// session wakes on: the host and stores, and the wakes store. A build without
// session wakes replaces this file with one that sets only the host and stores.
func wakesOn(s *Server, pool *pgxpool.Pool, m Mirror, a *attention.PGStore, p *pages.PGStore) {
	wireWakes(s, m, sessionwake.NewPG(pool), a, p)
}

// probeSweep is the attention sweep's wake steps.
func probeSweep(s *Server) {
	s.wakeDraftOutcomesDue(context.Background())
	s.deliverDueWakes(context.Background())
}
