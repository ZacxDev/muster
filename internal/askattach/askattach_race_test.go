package askattach

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// THE FILE OPENED IS THE FILE RESOLVED (round 1 Y2): between the path's
// resolution (workspacefile.Resolve) and its open, the file can be swapped. A
// symlink swapped in is not followed, a FIFO swapped in neither blocks nor is
// sent, and anything that is not the very file resolved is refused.

func swapAfterResolve(t *testing.T, swap func(real string)) {
	t.Helper()
	afterResolve = swap
	t.Cleanup(func() { afterResolve = nil })
}

func TestASymlinkSwappedInAfterResolveIsRefusedAndNothingIsSent(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(root, "shot.png"), pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	swapAfterResolve(t, func(real string) {
		_ = os.Remove(real)
		if err := os.Symlink(secret, real); err != nil {
			t.Error(err)
		}
	})
	_, err := Upload(context.Background(), nil, target(ts), root, "shot.png")
	if err == nil || f.n() != 0 {
		t.Fatalf("a symlink swapped in after resolve: err %v, %d file(s) sent", err, f.n())
	}
}

func TestAFileReplacedAfterResolveIsRefused(t *testing.T) {
	f, ts := newFakeMuster(t)
	root := t.TempDir()
	p := filepath.Join(root, "shot.png")
	if err := os.WriteFile(p, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	swapAfterResolve(t, func(real string) {
		// Another inode under the same name: the old one kept alive by a
		// second link, so the filesystem cannot hand its number to the new
		// file; the new one renamed over the name.
		if err := os.Link(real, real+".kept"); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(real+".new", pngBytes, 0o600); err != nil {
			t.Error(err)
		}
		if err := os.Rename(real+".new", real); err != nil {
			t.Error(err)
		}
	})
	_, err := Upload(context.Background(), nil, target(ts), root, "shot.png")
	if err == nil || !strings.Contains(err.Error(), "changed") || f.n() != 0 {
		t.Fatalf("a file replaced after resolve: err %v, %d file(s) sent", err, f.n())
	}
}

func TestAFIFOSwappedInAfterResolveIsRefusedWithoutBlocking(t *testing.T) {
	f, ts := newFakeMuster(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "shot.png"), pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	swapAfterResolve(t, func(real string) {
		_ = os.Remove(real)
		if err := syscall.Mkfifo(real, 0o600); err != nil {
			t.Error(err)
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := Upload(context.Background(), nil, target(ts), root, "shot.png")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || f.n() != 0 {
			t.Fatalf("a FIFO swapped in after resolve: err %v, %d file(s) sent", err, f.n())
		}
	case <-time.After(5 * time.Second):
		// Unblock the stuck open so the goroutine ends, then fail.
		if w, err := os.OpenFile(filepath.Join(root, "shot.png"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("opening a FIFO swapped in after resolve blocked")
	}
}
