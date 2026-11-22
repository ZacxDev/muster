package api

import (
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/sessionwake"
)

// wireWakes gives server s the stores and host session_wake_pg_test.go runs
// against.
func wireWakes(s *Server, m Mirror, w *sessionwake.PGStore, a *attention.PGStore, p *pages.PGStore) {
	s.ext.Mirror, s.ext.SessionWakes, s.ext.Attention, s.ext.Pages = m, w, a, p
}
