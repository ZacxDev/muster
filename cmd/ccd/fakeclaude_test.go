package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// fakeClaude is a stand-in for the interactive CLI, run INSIDE a real tmux pane
// by the tmuxit suite (CCD_TEST_AS=claude). It imitates exactly the behaviour ccd
// depends on, as measured on the real CLI:
//
//   - it blocks on a first-run screen unless .claude.json has
//     hasCompletedOnboarding, and on a trust screen unless projects[<cwd>] has
//     hasTrustDialogAccepted — and fires NO hooks while blocked;
//   - it enables bracketed paste, treats CR inside a paste as a newline (tmux
//     turns LF into CR on paste) and CR outside one as submit;
//   - it runs the hook COMMANDS from $CLAUDE_CONFIG_DIR/settings.json with
//     payloads shaped like the recorded ones (SessionStart, UserPromptSubmit,
//     Stop/StopFailure), so the image's real `ccd hook` wiring is exercised;
//   - it writes transcript records shaped like the recorded ones, the prompt
//     verbatim as received;
//   - a prompt whose first byte is `!` or `/` runs LOCALLY (shell mode / a slash
//     command) with no UserPromptSubmit and no model turn — as the real CLI does.
//
// Its reply states the prompt's length and SHA-256, so a test can check the
// bytes that ARRIVED from both ends: the transcript and the reply.
//
// ⚠ WHAT IT DOES NOT IMITATE: the real CLI expands a TAB to four spaces; this
// fake keeps it. That is a property of the CLI, not of ccd's paste, so the suite
// sends no tabs and the PR records the real CLI's behaviour instead.
func fakeClaude() int {
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	cwd, _ := os.Getwd()
	st := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(cfg, ".claude.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	// stty acts on ITS stdin, which must be the pane's tty (the default is /dev/null).
	stty := exec.Command("stty", "raw", "-echo")
	stty.Stdin = os.Stdin
	if err := stty.Run(); err != nil {
		appendFile(filepath.Join(cfg, "fake-hooks.log"), "stty raw failed: "+err.Error()+"\n")
	}
	if st["hasCompletedOnboarding"] != true {
		fmt.Print("Welcome. Choose the text style that looks best with your terminal\r\n")
		blockForever()
	}
	projects, _ := st["projects"].(map[string]any)
	proj, _ := projects[cwd].(map[string]any)
	if proj == nil || proj["hasTrustDialogAccepted"] != true {
		fmt.Print("Quick safety check: Is this a project you created or one you trust?\r\n")
		blockForever()
	}

	sid := randomID()
	transcript := filepath.Join(cfg, "projects", strings.ReplaceAll(cwd, "/", "-"), sid+".jsonl")
	_ = os.MkdirAll(filepath.Dir(transcript), 0o700)
	hooks := loadHooks(filepath.Join(cfg, "settings.json"))
	base := map[string]any{"session_id": sid, "transcript_path": transcript, "cwd": cwd}

	fire := func(event string, extra map[string]any) {
		payload := map[string]any{"hook_event_name": event}
		for k, v := range base {
			payload[k] = v
		}
		for k, v := range extra {
			payload[k] = v
		}
		b, _ := json.Marshal(payload)
		for _, c := range hooks[event] {
			cmd := exec.Command("sh", "-c", c)
			cmd.Stdin = bytes.NewReader(b)
			out, err := cmd.CombinedOutput()
			if err != nil || len(out) > 0 {
				// Diagnostics for the suite: a hook that failed or printed.
				appendFile(filepath.Join(cfg, "fake-hooks.log"), fmt.Sprintf("%s: %q err=%v out=%q PATH=%s\n", event, c, err, out, os.Getenv("PATH")))
			}
		}
	}
	write := func(rec map[string]any) {
		rec["sessionId"] = sid
		b, _ := json.Marshal(rec)
		f, err := os.OpenFile(transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write(append(b, '\n'))
	}
	assistant := func(block map[string]any, extra map[string]any) {
		rec := map[string]any{"type": "assistant", "isSidechain": false,
			"message": map[string]any{"role": "assistant", "content": []any{block}}}
		for k, v := range extra {
			rec[k] = v
		}
		write(rec)
	}

	fire("SessionStart", map[string]any{"source": "startup"})
	fmt.Print("\x1b[?2004h> ")

	submit := func(prompt string) {
		if strings.HasPrefix(prompt, "!") || strings.HasPrefix(prompt, "/") {
			// Measured on the real CLI: shell mode / a local slash command. No
			// UserPromptSubmit, nothing sent to the model.
			write(map[string]any{"type": "user", "isSidechain": false,
				"message": map[string]any{"role": "user", "content": "<bash-input>" + prompt[1:] + "</bash-input>"}})
			fmt.Print("\r\n! ran locally\r\n> ")
			return
		}
		pid := randomID()
		fire("UserPromptSubmit", map[string]any{"prompt_id": pid, "prompt": prompt, "permission_mode": "default"})
		write(map[string]any{"type": "user", "promptId": pid, "isSidechain": false,
			"message": map[string]any{"role": "user", "content": prompt}})
		if strings.Contains(prompt, "FAIL-AUTH") {
			msg := "Please run /login · API Error: 401 OAuth access token is invalid."
			assistant(map[string]any{"type": "text", "text": msg},
				map[string]any{"isApiErrorMessage": true, "error": "authentication_failed", "apiErrorStatus": 401})
			fire("StopFailure", map[string]any{"prompt_id": pid, "error": "authentication_failed", "last_assistant_message": msg})
			write(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 1})
			fmt.Print("\r\n" + msg + "\r\n> ")
			return
		}
		sum := sha256.Sum256([]byte(prompt))
		first := fmt.Sprintf("received %d bytes", len(prompt))
		second := "sha256 " + hex.EncodeToString(sum[:])
		assistant(map[string]any{"type": "thinking", "thinking": "PRIVATE REASONING", "signature": "s"}, nil)
		assistant(map[string]any{"type": "text", "text": first}, nil)
		assistant(map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "true"}}, nil)
		write(map[string]any{"type": "user", "promptId": pid, "isSidechain": false,
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": ""}}}})
		assistant(map[string]any{"type": "text", "text": second}, nil)
		fire("Stop", map[string]any{"prompt_id": pid, "stop_hook_active": false, "last_assistant_message": second})
		write(map[string]any{"type": "system", "subtype": "stop_hook_summary"})
		write(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 1})
		fmt.Print("\r\n" + first + "\r\n" + second + "\r\n> ")
	}

	pasteStart, pasteEnd := []byte("\x1b[200~"), []byte("\x1b[201~")
	var in, cur []byte
	inPaste := false
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return 0
		}
		in = append(in, buf[:n]...)
		for len(in) > 0 {
			switch {
			case bytes.HasPrefix(in, pasteStart):
				inPaste, in = true, in[len(pasteStart):]
				continue
			case bytes.HasPrefix(in, pasteEnd):
				inPaste, in = false, in[len(pasteEnd):]
				continue
			case in[0] == 0x1b && (bytes.HasPrefix(pasteStart, in) || bytes.HasPrefix(pasteEnd, in)):
				// A marker split across reads: wait for the rest.
			default:
				c := in[0]
				in = in[1:]
				switch {
				case inPaste && c == '\r':
					cur = append(cur, '\n')
				case inPaste:
					cur = append(cur, c)
				case c == '\r':
					if len(cur) > 0 {
						submit(string(cur))
						cur = nil
					}
				default:
					cur = append(cur, c)
				}
				continue
			}
			break
		}
	}
}

func appendFile(path, s string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(s)
}

func blockForever() {
	buf := make([]byte, 256)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			os.Exit(0)
		}
	}
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// loadHooks reads settings.json's hooks as event -> commands.
func loadHooks(path string) map[string][]string {
	out := map[string][]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(b, &s) != nil {
		return out
	}
	for ev, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Type == "command" {
					out[ev] = append(out[ev], h.Command)
				}
			}
		}
	}
	return out
}
