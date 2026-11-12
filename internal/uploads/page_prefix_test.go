package uploads_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/uploads"
)

// TestAPagesFileIsKeptUnderItsPagesPrefix: a file sent through a page's link
// is stored at pages/<page id>/<public id> — outside uploads/, which the
// bucket's lifecycle rule expires — and an ordinary upload is still at
// uploads/<public id>. Revoking it deletes that object.
func TestAPagesFileIsKeptUnderItsPagesPrefix(t *testing.T) {
	svc, _, obj, _ := newService(t, 1<<20)
	got, err := svc.PutForPage(as(2), rd([]byte("hello")), "notes.txt", 42, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := "pages/42/" + got.PublicID; got.ObjectKey != want || !obj.Has(want) {
		t.Fatalf("a page's file is at %q (stored %v), want %q", got.ObjectKey, obj.Keys(), want)
	}
	plain, err := svc.Put(as(2), rd([]byte("hello")), "notes.txt")
	if err != nil || plain.ObjectKey != "uploads/"+plain.PublicID {
		t.Fatalf("control: an ordinary upload is at %q (%v)", plain.ObjectKey, err)
	}
	if _, err := svc.PutForPage(as(2), rd([]byte("hello")), "notes.txt", 0, 0); err == nil {
		t.Error("a page's file with no page was stored")
	}
	if _, err := svc.Revoke(as(2), got.PublicID); err != nil {
		t.Fatal(err)
	}
	if obj.Has(got.ObjectKey) {
		t.Errorf("revoking a page's file left %s", got.ObjectKey)
	}
}

// TestAPagesFileIsCappedByWhatItsPageHasLeft: PutForPage's max refuses one
// byte over it (ErrTooLarge, nothing stored) and takes exactly it; a max above
// the deployment's cap is the deployment's cap.
func TestAPagesFileIsCappedByWhatItsPageHasLeft(t *testing.T) {
	svc, _, obj, _ := newService(t, 1000)
	if _, err := svc.PutForPage(as(2), rd([]byte(strings.Repeat("a", 71))), "a.txt", 7, 70); !errors.Is(err, uploads.ErrTooLarge) {
		t.Fatalf("71 bytes under a max of 70: %v", err)
	}
	if n := len(obj.Keys()); n != 0 {
		t.Fatalf("a refused file left %d object(s)", n)
	}
	if _, err := svc.PutForPage(as(2), rd([]byte(strings.Repeat("a", 70))), "a.txt", 7, 70); err != nil {
		t.Fatalf("70 bytes under a max of 70: %v", err)
	}
	if _, err := svc.PutForPage(as(2), rd([]byte(strings.Repeat("a", 1001))), "a.txt", 7, 5000); !errors.Is(err, uploads.ErrTooLarge) {
		t.Fatalf("1001 bytes under the deployment's 1000 with a max of 5000: %v", err)
	}
}

// TestDeletingAPagesFilesTakesEveryObjectUnderItsPrefix: by the listing —
// an object no row names goes too — and nothing outside the prefix.
func TestDeletingAPagesFilesTakesEveryObjectUnderItsPrefix(t *testing.T) {
	svc, _, obj, _ := newService(t, 1000)
	obj.Seed("pages/7/AAAAAAAAAAAAAAAAAAAAAA", []byte("x"))
	obj.Seed("pages/7/BBBBBBBBBBBBBBBBBBBBBB", []byte("x"))
	obj.Seed("pages/70/CCCCCCCCCCCCCCCCCCCCCC", []byte("x"))
	obj.Seed("uploads/DDDDDDDDDDDDDDDDDDDDDD", []byte("x"))
	n, err := svc.DeletePageFiles(context.Background(), 7)
	if err != nil || n != 2 {
		t.Fatalf("DeletePageFiles: %d %v", n, err)
	}
	if obj.Has("pages/7/AAAAAAAAAAAAAAAAAAAAAA") || !obj.Has("pages/70/CCCCCCCCCCCCCCCCCCCCCC") || !obj.Has("uploads/DDDDDDDDDDDDDDDDDDDDDD") {
		t.Fatalf("left %v", obj.Keys())
	}
}

// TestTheProbeNeedsWriteAndDeleteUnderPages: it writes and deletes one
// object under pages/, and says which failed.
func TestTheProbeNeedsWriteAndDeleteUnderPages(t *testing.T) {
	svc, _, obj, _ := newService(t, 1000)
	if err := svc.ProbePages(context.Background()); err != nil {
		t.Fatalf("control: %v", err)
	}
	if n := len(obj.Keys()); n != 0 {
		t.Fatalf("the probe left %v", obj.Keys())
	}
	obj.FailDelete = errors.New("AccessDenied")
	if err := svc.ProbePages(context.Background()); err == nil || !strings.Contains(err.Error(), "delete pages/") {
		t.Fatalf("a credential that cannot delete: %v", err)
	}
	obj.FailDelete, obj.FailPut = nil, errors.New("AccessDenied")
	if err := svc.ProbePages(context.Background()); err == nil || !strings.Contains(err.Error(), "write pages/") {
		t.Fatalf("a credential that cannot write: %v", err)
	}
}
