package mirrorhost

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THIS HOST'S SPAWN READINESS (internal/mirror/spawnready.go): what it tells
// muster in its hello, and again whenever it changes, about starting an
// officer instance here — its allow-list, scratch spawns, and whether claude
// is found and logged in.
//
// 🔴 IT READS, IT NEVER DECIDES. Every spawn still runs every check in
// agentspawn.go; the readiness only lets muster say why a start would fail
// before it asks.
//
// 🔴 THE LOGIN CHECK PRINTS NO SECRET. `claude auth status --json` answers
// {loggedIn, authMethod, …, email, orgName …}; only loggedIn is read, and
// nothing of the answer is logged or sent.

// spawnReadyEvery is how often a connection re-reads its readiness, sending
// it only when it changed (a login done in the meantime, a PATH fixed).
const spawnReadyEvery = time.Minute

// claudeAuthTimeout bounds the login check (measured: about 0.1 s).
const claudeAuthTimeout = 5 * time.Second

// spawnReadiness is this connection's readiness now; nil for a host that
// advertises none: a v1 tunnel, a read-only host, or the browser suite's
// fake spawn (whose fixture answers every spawn itself).
func (h *hostSide) spawnReadiness() *mirror.SpawnReadiness {
	if h.version < 2 || !h.c.cfg.Interactive || h.c.cfg.FakeSpawn != nil {
		return nil
	}
	sc := h.c.cfg.AgentSpawn
	r := &mirror.SpawnReadiness{Unavailable: agentSpawnUnavailable(h.c.cfg), Repos: slices.Clone(sc.Repos), Scratch: sc.Enabled,
		Claude: mirror.ClaudeMissing, Login: mirror.LoginUnknown}
	if r.Repos == nil {
		r.Repos = []string{}
	}
	ctx, cancel := context.WithTimeout(h.ctx, claudeAuthTimeout)
	defer cancel()
	bin, err := h.c.claudeBinary(ctx)
	if err != nil {
		r.ClaudeDetail = err.Error() + claudePathHint
		return r
	}
	r.Claude = mirror.ClaudeFound
	r.Login = claudeLogin(ctx, bin)
	return r
}

// claudePathHint follows a "claude is not found" detail: what to do about it.
const claudePathHint = "; set -claude-path (" + "MUSTER_MIRROR_CLAUDE_PATH" + ") to its absolute path, or put its directory on the tmux server's PATH (`tmux set-environment -g PATH …`)"

// claudeLogin is whether the claude at bin is logged in, by its own `auth
// status --json` (LoginUnknown when it cannot say: an older claude with no
// such subcommand, a timeout, an answer that does not parse).
func claudeLogin(ctx context.Context, bin string) string {
	out, err := exec.CommandContext(ctx, bin, "auth", "status", "--json").Output()
	if err != nil {
		return mirror.LoginUnknown
	}
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(string(out))), &st) != nil || st.LoggedIn == nil {
		return mirror.LoginUnknown
	}
	if *st.LoggedIn {
		return mirror.LoginYes
	}
	return mirror.LoginNo
}

// watchSpawnReady re-reads the readiness every spawnReadyEvery and sends it
// (TypeSpawnReady, through send: the connection's own) when it differs from
// what this connection last sent: the hello's at first. It ends with the
// connection.
func (h *hostSide) watchSpawnReady(sent *mirror.SpawnReadiness, send func(mirror.Msg) error) {
	if sent == nil {
		return
	}
	every := spawnReadyEvery
	if h.c.cfg.SpawnReadyEvery > 0 {
		every = h.c.cfg.SpawnReadyEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
		}
		now := h.spawnReadiness()
		if now == nil || now.Same(sent) {
			continue
		}
		if err := send(mirror.Msg{Type: mirror.TypeSpawnReady, SpawnReady: now}); err != nil {
			return
		}
		h.logReadyChange(sent, now)
		sent = now
	}
}

// logSpawnReady logs a readiness as the mirror's startup sees it: what would
// keep an officer from starting here.
func (h *hostSide) logSpawnReady(r *mirror.SpawnReadiness) {
	// (claude not found is agentSpawnRuntimes' own line.)
	if r == nil || r.Unavailable != "" || r.Claude != mirror.ClaudeFound {
		return
	}
	if r.Login == mirror.LoginNo {
		h.c.log.Printf("muster-mirror: -agent-spawn: Claude Code is not logged in for this user: a spawned agent would stop at its login screen; run `claude` once here and log in")
	}
}

// logReadyChange logs what changed between two readinesses.
func (h *hostSide) logReadyChange(was, now *mirror.SpawnReadiness) {
	switch {
	case was.Claude != now.Claude && now.Claude == mirror.ClaudeFound:
		h.c.log.Printf("muster-mirror: -agent-spawn: claude is found now")
	case was.Claude != now.Claude:
		h.c.log.Printf("muster-mirror: -agent-spawn: claude is not offered: %s", now.ClaudeDetail)
	}
	if was.Login != now.Login {
		h.c.log.Printf("muster-mirror: -agent-spawn: Claude Code's login now reads %s", now.Login)
	}
}
