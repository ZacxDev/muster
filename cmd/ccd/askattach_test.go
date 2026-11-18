package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askattach"
)

// `ccd ask-attach <path>`: a dispatched agent's workspace file reaches
// muster's upload store with the agent's own token, and the id it prints is
// what attention_ask's attachment_ids takes.
func TestAskAttachSendsTheWorkspaceFileWithTheAgentsTokenAndPrintsItsID(t *testing.T) {
	var got []byte
	var tok, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		tok, path = r.Header.Get("X-Muster-Token"), r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(askattach.Record{ID: "AAAAAAAAAAAAAAAAAAAAAA", Name: r.URL.Query().Get("filename"), Type: "text/plain; charset=utf-8", Size: int64(len(got))})
	}))
	t.Cleanup(ts.Close)
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(filepath.Join(root, "build.log"), []byte("FAIL: TestX\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{envConnectURL: ts.URL, envAgentToken: "agent-tok", "CCD_WORKSPACE": root}
	var out, errb bytes.Buffer
	if rc := askAttach([]string{"build.log"}, func(k string) string { return env[k] }, nil, &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	if string(got) != "FAIL: TestX\n" || tok != "agent-tok" || path != askattach.AgentPath {
		t.Fatalf("muster got %q with token %q at %s", got, tok, path)
	}
	if strings.TrimSpace(out.String()) != `{"id":"AAAAAAAAAAAAAAAAAAAAAA","name":"build.log","type":"text/plain; charset=utf-8","size":12}` {
		t.Fatalf("printed %s", out.String())
	}
	// A path out of the workspace is refused before anything is sent.
	got = nil
	errb.Reset()
	if rc := askAttach([]string{"../../etc/passwd"}, func(k string) string { return env[k] }, nil, &out, &errb); rc != 1 ||
		!strings.Contains(errb.String(), "outside your workspace") || got != nil {
		t.Fatalf("rc %d, stderr %q, sent %q", rc, errb.String(), got)
	}
	// Without the pod's environment it says what is missing.
	errb.Reset()
	if rc := askAttach([]string{"build.log"}, func(string) string { return "" }, nil, &out, &errb); rc != 1 || !strings.Contains(errb.String(), "must be set") {
		t.Fatalf("rc %d, stderr %q", rc, errb.String())
	}
}
