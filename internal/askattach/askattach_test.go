package askattach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/workspacefile"
)

// THE CLIENT EVERY SENDER OF AN ASK'S FILE USES (muster-mirror's MCP proxy,
// `ccd ask-attach`): the path confined to the workspace after symlinks, the
// size capped, the type sniffed, and nothing sent for a file it refuses.

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89-pixels")

type fakeMuster struct {
	mu    sync.Mutex
	got   [][]byte
	names []string
	auth  []string
}

func (f *fakeMuster) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

func newFakeMuster(t *testing.T) (*fakeMuster, *httptest.Server) {
	t.Helper()
	f := &fakeMuster{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != AgentPath {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got, f.names, f.auth = append(f.got, b), append(f.names, r.URL.Query().Get("filename")), append(f.auth, r.Header.Get("X-Muster-Token"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Record{ID: "AAAAAAAAAAAAAAAAAAAAAA", Name: r.URL.Query().Get("filename"), Type: Sniff(b), Size: int64(len(b))})
	}))
	t.Cleanup(ts.Close)
	return f, ts
}

func target(ts *httptest.Server) Target {
	return Target{Base: ts.URL, Path: AgentPath, Header: http.Header{"X-Muster-Token": {"agent-token"}}}
}

func TestAWorkspaceImageArrivesWholeWithItsNameAndToken(t *testing.T) {
	f, ts := newFakeMuster(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "shot.png"), pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := Upload(context.Background(), nil, target(ts), root, "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != "AAAAAAAAAAAAAAAAAAAAAA" || rec.Type != "image/png" || f.n() != 1 || !bytes.Equal(f.got[0], pngBytes) ||
		f.names[0] != "shot.png" || f.auth[0] != "agent-token" {
		t.Fatalf("record %+v; muster got %d file(s) named %v with token %v", rec, f.n(), f.names, f.auth)
	}
}

func TestAFileOutsideTheWorkspaceIsRefusedBeforeAnythingIsSent(t *testing.T) {
	f, ts := newFakeMuster(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "work")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "secret.png")
	if err := os.WriteFile(secret, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink INSIDE the workspace that leads out of it.
	if err := os.Symlink(secret, filepath.Join(root, "innocent.png")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret.png", secret, "innocent.png", filepath.Join(root, "innocent.png")} {
		if _, err := Upload(context.Background(), nil, target(ts), root, p); !errors.Is(err, workspacefile.ErrOutside) {
			t.Errorf("%s: %v, want ErrOutside", p, err)
		}
	}
	if f.n() != 0 {
		t.Fatalf("a refused path still sent %d file(s)", f.n())
	}
}

func TestAMissingAnOversizedAnEmptyAndAnUnsupportedFileAreRefused(t *testing.T) {
	f, ts := newFakeMuster(t)
	root := t.TempDir()
	big := make([]byte, MaxBytes+1)
	copy(big, pngBytes)
	for name, data := range map[string][]byte{"big.png": big, "empty.txt": {}, "page.html": []byte("<!DOCTYPE html><html><script>x</script></html>")} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]string{
		"nope.png":  "cannot read nope.png",
		"big.png":   ErrTooLarge.Error(),
		"empty.txt": "empty.txt is empty",
		"page.html": "page.html is text/html; charset=utf-8, which an ask cannot carry",
	}
	for p, want := range cases {
		_, err := Upload(context.Background(), nil, target(ts), root, p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", p, err, want)
		}
	}
	if f.n() != 0 {
		t.Fatalf("a refused file still sent %d file(s)", f.n())
	}
	// Exactly at the cap is accepted (the boundary's other side).
	if err := os.WriteFile(filepath.Join(root, "max.png"), big[:MaxBytes], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Upload(context.Background(), nil, target(ts), root, "max.png"); err != nil || f.n() != 1 {
		t.Fatalf("a file of exactly %d bytes: %v (%d sent)", MaxBytes, err, f.n())
	}
}

func TestMustersRefusalIsSaidInItsOwnWords(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		_, _ = io.WriteString(w, `{"error":"the file is application/x-foo, which an ask cannot carry"}`)
	}))
	t.Cleanup(ts.Close)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.png"), pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Upload(context.Background(), nil, target(ts), root, "a.png")
	if err == nil || err.Error() != "muster refused a.png (415): the file is application/x-foo, which an ask cannot carry" {
		t.Fatalf("got %v", err)
	}
}
