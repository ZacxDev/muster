package podagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// `ccd ask-image`'s pod side: a workspace file, confined to the workspace,
// sniffed and capped, streamed to muster's AskImagePath with the agent's token.
//
// The cap is pinned as the literal 5 MiB (5<<20), not askimage.MaxBytes, so a
// moved bound is a failing test rather than a moved target.
// ---------------------------------------------------------------------------

const askCap = 5 << 20

// askPNGFile is a PNG of exactly n bytes (its signature, then padding).
func askPNGFile(n int) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, n-8)...)
}

// fakeMuster records what reaches muster's ask-image route and answers 201.
type fakeMuster struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedUpload
}

type recordedUpload struct {
	path, filename, ct, token string
	body                      []byte
}

func newFakeMuster(t *testing.T) *fakeMuster {
	t.Helper()
	f := &fakeMuster{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedUpload{path: r.URL.Path, filename: r.URL.Query().Get("filename"), ct: r.Header.Get("Content-Type"), token: r.Header.Get("X-Muster-Token"), body: b})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"att-77","filename":"shot.png","content_type":"image/png","size":1,"sha256":"x"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMuster) uploads() []recordedUpload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedUpload(nil), f.reqs...)
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAWorkspacePathMustResolveInsideTheWorkspace: a `..` escape, an absolute
// path elsewhere, a symlink to a file outside and a path through a symlinked
// directory outside are each ErrOutsideWorkspace; a nested file, and a file
// reached through a symlink that stays inside, resolve.
func TestAWorkspacePathMustResolveInsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(root, "shots", "header.png"), askPNGFile(64))
	write(t, filepath.Join(outside, "secret.png"), askPNGFile(64))
	if err := os.Symlink(filepath.Join(outside, "secret.png"), filepath.Join(root, "link-out.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "shots", "header.png"), filepath.Join(root, "link-in.png")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"../" + filepath.Base(outside) + "/secret.png",
		"shots/../../" + filepath.Base(outside) + "/secret.png",
		filepath.Join(outside, "secret.png"),
		"link-out.png",
		filepath.Join(root, "link-out.png"),
		"dir-out/secret.png",
	} {
		if got, err := ResolveWorkspaceFile(root, p); !errors.Is(err, ErrOutsideWorkspace) {
			t.Errorf("%s: resolved to %q (err %v), want ErrOutsideWorkspace", p, got, err)
		}
	}
	realHeader, _ := filepath.EvalSymlinks(filepath.Join(root, "shots", "header.png"))
	for _, p := range []string{"shots/header.png", filepath.Join(root, "shots", "header.png"), "link-in.png"} {
		if got, err := ResolveWorkspaceFile(root, p); err != nil || got != realHeader {
			t.Errorf("%s: %q, %v; want %s", p, got, err, realHeader)
		}
	}
}

// TestAnAskImageUploadIsSniffedCappedAndCarriesTheAgentsToken: exactly 5 MiB
// of PNG is streamed whole to muster's route with the agent's own token, the
// sniffed Content-Type and the file's base name, and HQ's id comes back; 5 MiB
// + 1 byte, a text file named .png, and a path escaping the workspace are
// refused with nothing sent.
func TestAnAskImageUploadIsSniffedCappedAndCarriesTheAgentsToken(t *testing.T) {
	root := t.TempDir()
	m := newFakeMuster(t)
	ctx := context.Background()
	exact := askPNGFile(askCap)
	write(t, filepath.Join(root, "exact.png"), exact)
	write(t, filepath.Join(root, "over.png"), askPNGFile(askCap+1))
	write(t, filepath.Join(root, "notes.png"), []byte("these are notes, not a picture"))

	up, err := UploadAskImage(ctx, nil, m.URL, "agent-own-token-5f1a", root, "exact.png")
	if err != nil {
		t.Fatalf("exactly 5 MiB: %v", err)
	}
	if up.ID != "att-77" {
		t.Errorf("id %q, want HQ's att-77", up.ID)
	}
	got := m.uploads()
	if len(got) != 1 {
		t.Fatalf("%d uploads reached muster, want 1", len(got))
	}
	u := got[0]
	if u.path != "/agent/officer/ask-images" || u.filename != "exact.png" || u.ct != "image/png" || u.token != "agent-own-token-5f1a" {
		t.Errorf("upload: path %q filename %q type %q token %q", u.path, u.filename, u.ct, u.token)
	}
	if !bytes.Equal(u.body, exact) {
		t.Errorf("muster received %d bytes, want the file's exact %d", len(u.body), len(exact))
	}

	for _, tc := range []struct{ path, want string }{
		{"over.png", "over 5 MiB"},
		{"notes.png", "not a PNG, JPEG or WebP"},
		{"../escape.png", ErrOutsideWorkspace.Error()},
	} {
		if _, err := UploadAskImage(ctx, nil, m.URL, "agent-own-token-5f1a", root, tc.path); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error saying %q", tc.path, err, tc.want)
		}
	}
	if n := len(m.uploads()); n != 1 {
		t.Errorf("a refused file reached muster (%d uploads, want still 1)", n)
	}
	if _, err := UploadAskImage(ctx, nil, "", "", root, "exact.png"); err == nil || !strings.Contains(err.Error(), EnvConnectURL) {
		t.Errorf("no muster URL or token: %v, want a refusal naming %s", err, EnvConnectURL)
	}
}

// TestTheUploadCapHoldsWhileStreaming: capReader passes exactly the cap and
// fails on the byte after it, so a file that grows after its Stat cannot send
// more than the cap.
func TestTheUploadCapHoldsWhileStreaming(t *testing.T) {
	for _, tc := range []struct {
		size     int
		tooLarge bool
	}{{askCap, false}, {askCap + 1, true}, {askCap + 4096, true}} {
		r := &capReader{r: bytes.NewReader(make([]byte, tc.size)), left: askCap}
		n, err := io.Copy(io.Discard, r)
		if tc.tooLarge != errors.Is(err, errTooLarge) {
			t.Errorf("%d bytes: copied %d, err %v; want too-large %v", tc.size, n, err, tc.tooLarge)
		}
		if n > askCap+1 {
			t.Errorf("%d bytes: %d passed the cap", tc.size, n)
		}
	}
}

// TestAnUploadIsSentToTheConfiguredMusterOnly: the request goes to base's own
// host, the path and query built here (a filename with URL syntax in it stays
// one query value).
func TestAnUploadIsSentToTheConfiguredMusterOnly(t *testing.T) {
	root := t.TempDir()
	m := newFakeMuster(t)
	write(t, filepath.Join(root, "a&b=c.png"), askPNGFile(100))
	if _, err := UploadAskImage(context.Background(), nil, m.URL+"/", "tok-0123", root, "a&b=c.png"); err != nil {
		t.Fatal(err)
	}
	u := m.uploads()[0]
	if u.path != "/agent/officer/ask-images" || u.filename != "a&b=c.png" {
		t.Fatalf("path %q filename %q", u.path, u.filename)
	}
}
