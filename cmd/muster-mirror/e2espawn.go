//go:build e2emirror

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// THE CHIEF'S AGENT SPAWN, PLAYED (docs/design/chief-in-agent-view.md phase 2's
// browser spec): with MUSTER_MIRROR_E2E_SPAWN_RUNTIMES (comma-separated, e.g.
// "opencode") this per-window fake ADVERTISES those runtimes for agent-spawn
// and answers each one itself (mirrorhost.Config.FakeSpawn) — no git, no
// worktree, no tmux:
//
//   - every request is appended to MUSTER_MIRROR_E2E_SPAWN_LOG as one JSON
//     line {dispatch, runtime, repo, base, model, name, prompt}, so a spec
//     reads EXACTLY what muster sent the host;
//   - an opencode spawn puts a session on the fake opencode server
//     (MUSTER_MIRROR_E2E_OPENCODE_DIR's) titled with the request's name, whose
//     first message is the request's prompt, and lists a new opencode window
//     in session "agents" TAGGED WITH THE DISPATCH (WindowInfo.Dispatch, what
//     the real host reads from @muster-dispatch) — so the server's reconciler
//     sees it running on the next listing;
//   - it answers agent-spawned with that window.
//
// Only opencode is played; a claude request is refused (unavailable).

var e2eSpawned struct {
	mu      sync.Mutex
	windows []e2eSpawnedWindow
}

type e2eSpawnedWindow struct {
	id, title string
	dispatch  int64
}

func init() {
	rts := os.Getenv("MUSTER_MIRROR_E2E_SPAWN_RUNTIMES")
	if rts == "" {
		return
	}
	e2eSpawnRuntimes = strings.Split(rts, ",")
	e2eSpawn = e2eFakeSpawn
}

func e2eFakeSpawn(_ context.Context, dispatch int64, req mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
	if log := os.Getenv("MUSTER_MIRROR_E2E_SPAWN_LOG"); log != "" {
		line, _ := json.Marshal(map[string]any{"dispatch": dispatch, "runtime": req.Runtime, "repo": req.Repo, "base": req.Base,
			"model": req.Model, "name": req.Name, "prompt": req.Prompt})
		f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	}
	if req.Runtime != mirror.SpawnRuntimeOpenCode || openCodeSourceE2E() == nil {
		return mirror.AgentSpawned{}, &mirror.SpawnRefusedError{Code: mirror.SpawnUnavailable, Detail: "the e2e fake plays opencode spawns only"}
	}
	d := strconv.FormatInt(dispatch, 10)
	sid := "ses_0e2espawn" + d
	title := strings.TrimSpace(req.Name)
	if title == "" {
		title = "Dispatch " + d
	}
	title += " #" + d
	e2eOpenCode.mu.Lock()
	srv := e2eOpenCode.srv
	e2eOpenCode.mu.Unlock()
	if srv == nil {
		return mirror.AgentSpawned{}, errors.New("the e2e fake opencode server is not up")
	}
	srv.PutSession(sid, title, "")
	srv.PutMessage(sid, map[string]any{"id": "msg_0e2espawn" + d, "type": "user", "text": req.Prompt, "files": []any{}, "agents": []any{},
		"time": map[string]any{"created": time.Now().UnixMilli()}})
	win := "@" + d
	e2eSpawned.mu.Lock()
	e2eSpawned.windows = append(e2eSpawned.windows, e2eSpawnedWindow{id: win, title: title, dispatch: dispatch})
	e2eSpawned.mu.Unlock()
	return mirror.AgentSpawned{Session: "agents", Window: win, Worktree: "/srv/e2e-fixture/agents/" + d, Branch: "muster/" + d, AgentSession: sid}, nil
}

// spawnedWindowsE2E is the windows the fake spawn opened, as listed (after
// MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE's).
func spawnedWindowsE2E(activity int64) []mirrorhost.WindowInfo {
	e2eSpawned.mu.Lock()
	ws := append([]e2eSpawnedWindow(nil), e2eSpawned.windows...)
	e2eSpawned.mu.Unlock()
	var out []mirrorhost.WindowInfo
	for i, w := range ws {
		pane := windowFakePane("agents", w.id)
		ap := mirrorhost.ResolveOpenCodePaneStarted(openCodeSourceE2E(), pane, 1, "", "OC | "+w.title)
		out = append(out, mirrorhost.WindowInfo{Session: "agents", ID: w.id, Index: i + 1, Command: "opencode", Activity: activity,
			Pane: pane, PanePID: 1, Agent: &ap, Dispatch: w.dispatch})
	}
	return out
}
