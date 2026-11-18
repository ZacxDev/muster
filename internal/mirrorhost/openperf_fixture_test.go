package mirrorhost

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE OPEN'S PERFORMANCE FIXTURES (openperf_*_test.go): a synthetic
// transcript shaped like a long Claude Code session — prompts, markdown
// replies, Bash / Read / Edit calls and their results, thinking — and nothing
// from anybody's real one. Seeded, so a benchmark reads the same bytes on
// every run.

// perfTranscript writes a transcript of at least size bytes to path and
// returns how many rows it holds.
func perfTranscript(tb testing.TB, path string, size int) int {
	tb.Helper()
	rng := rand.New(rand.NewSource(7))
	words := strings.Fields("the handler reads a frame from the socket and the hub routes it to the watch " +
		"that asked so every batch lands on the stream it belongs to while a slow reader is ended " +
		"with an overrun instead of stalling the connection for everyone else on the host")
	prose := func(n int) string {
		var sb strings.Builder
		for sb.Len() < n {
			sb.WriteString(words[rng.Intn(len(words))])
			if rng.Intn(9) == 0 {
				sb.WriteString(". ")
			} else {
				sb.WriteByte(' ')
			}
		}
		return sb.String()
	}
	reply := func() string {
		return "## What changed\n\n" + prose(600) + "\n\n- `internal/mirror/hub.go`: " + prose(120) +
			"\n- run `go test -race -count=1 ./internal/...` -- " + prose(80) +
			"\n\n```go\nfunc (h *Hub) route(m Msg) error {\n\tif m.ID == 0 {\n\t\treturn fmt.Errorf(\"id=%d\", m.ID)\n\t}\n\treturn nil\n}\n```\n\n" + prose(300)
	}
	code := func(n int) string {
		var sb strings.Builder
		for i := 0; sb.Len() < n; i++ {
			fmt.Fprintf(&sb, "%5d\tfunc step%d(ctx context.Context, key string) (int, error) { return len(key) + %d, nil }\n", i+1, i, i)
		}
		return sb.String()
	}
	log := func(n int) string {
		var sb strings.Builder
		for i := 0; sb.Len() < n; i++ {
			fmt.Fprintf(&sb, "=== RUN   TestTheHubRoutesBatch%d\n--- PASS: TestTheHubRoutesBatch%d (0.0%ds)\n", i, i, i%10)
		}
		return sb.String() + "PASS\nok  \texample.test/hub\t1.234s\n"
	}
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	rows, written := 0, 0
	row := func(v any) {
		b, _ := json.Marshal(v)
		b = append(b, '\n')
		n, err := f.Write(b)
		if err != nil {
			tb.Fatal(err)
		}
		written += n
		rows++
	}
	for i := 0; written < size; i++ {
		row(map[string]any{"type": "user", "promptId": fmt.Sprintf("p%d", i), "message": map[string]any{"content": "please " + prose(200)}})
		for j := 0; j < 4; j++ {
			id := fmt.Sprintf("tu%d-%d", i, j)
			var name, out string
			var input map[string]any
			switch j % 3 {
			case 0:
				name, out = "Bash", log(1500+rng.Intn(6000))
				input = map[string]any{"command": "go test -race ./internal/mirror/... -run TestHub", "description": "run the hub's tests"}
			case 1:
				name, out = "Read", code(2000+rng.Intn(8000))
				input = map[string]any{"file_path": "/src/project/internal/mirror/hub.go"}
			default:
				name, out = "Edit", "The file /src/project/internal/mirror/hub.go has been updated."
				input = map[string]any{"file_path": "/src/project/internal/mirror/hub.go", "old_string": code(300), "new_string": code(320)}
			}
			row(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
				map[string]any{"type": "thinking", "thinking": prose(400)},
				map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
			row(map[string]any{"type": "user", "promptId": fmt.Sprintf("p%d", i), "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": out, "is_error": false}}}})
		}
		row(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": reply()}}}})
	}
	return rows
}

// perfClaudeHome is a Claude Code config dir holding one session's sessions
// file and transcript: what ClaudeLocator resolves pid's pane to. It returns
// the transcript's path.
func perfClaudeHome(tb testing.TB, home string, pid int, sessionID, cwd string, size int) string {
	tb.Helper()
	dir := filepath.Join(home, "projects", ProjectSlug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tb.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		tb.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	perfTranscript(tb, path, size)
	sf, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": sessionID, "cwd": cwd, "status": "idle", "statusUpdatedAt": 946684800000})
	if err := os.WriteFile(filepath.Join(home, "sessions", fmt.Sprint(pid)+".json"), sf, 0o600); err != nil {
		tb.Fatal(err)
	}
	return path
}
