package mirrorhost

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// TestASourceWhoseTokenFileAppearsLaterResolvesOnceItIsRead: a read-only
// daemon's PendingCredentials (no token file at its start) is "not paired" —
// no view, no request to the server — until the file appears and is re-read;
// then the same source, holding the same credential, resolves the pane.
func TestASourceWhoseTokenFileAppearsLaterResolvesOnceItIsRead(t *testing.T) {
	srv := opencodetest.New(ocFixturePassword)
	t.Cleanup(srv.Close)
	srv.PutSession("ses_fixtureA", "Fix the flaky fixture", "")
	file := filepath.Join(t.TempDir(), "opencode-token")
	creds := opencodehost.PendingCredentials(file)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	src := NewOpenCodeSource(ctx, creds, "not paired on this host (no token at the fixture path)", log.New(io.Discard, "", 0))
	procs, children, root := ocPaneTable(t, "opencode")
	rt := openCodeRuntime{proc: root, src: src, title: titled("OC | Fix the flaky fixture")}

	ap := rt.LocatePanes(ocPane, procs, children)["%1"]
	if ap.Gen != "" || ap.OpenCode == nil || ap.OpenCode.NoView != notPairedWhy || src.tokenHealth() != "" {
		t.Fatalf("before the file: %+v (%+v), health %q", ap, ap.OpenCode, src.tokenHealth())
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("before the file: %d request(s) to the server, want 0", n)
	}

	if err := opencodehost.WriteTokenFile(file, opencodehost.TokenFile{URL: srv.URL, Token: srv.IssueAt(time.Now().Add(20 * 24 * time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if changed, err := creds.Reload(); !changed || err != nil {
		t.Fatalf("Reload: %v %v", changed, err)
	}
	ap = rt.LocatePanes(ocPane, procs, children)["%1"]
	if ap.Gen != "ses_fixtureA" || ap.OpenCode == nil || ap.OpenCode.NoView != "" {
		t.Fatalf("after the file: %+v (%+v)", ap, ap.OpenCode)
	}
}
