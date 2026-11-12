package uploads_test

import "testing"

// TestAPagesFileIsKeptUnderItsPagesPrefix: a file sent through a page's link
// is stored at pages/<page id>/<public id> — outside uploads/, which the
// bucket's lifecycle rule expires — and an ordinary upload is still at
// uploads/<public id>. Revoking it deletes that object.
func TestAPagesFileIsKeptUnderItsPagesPrefix(t *testing.T) {
	svc, _, obj, _ := newService(t, 1<<20)
	got, err := svc.PutForPage(as(2), rd([]byte("hello")), "notes.txt", 42)
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
	if _, err := svc.PutForPage(as(2), rd([]byte("hello")), "notes.txt", 0); err == nil {
		t.Error("a page's file with no page was stored")
	}
	if _, err := svc.Revoke(as(2), got.PublicID); err != nil {
		t.Fatal(err)
	}
	if obj.Has(got.ObjectKey) {
		t.Errorf("revoking a page's file left %s", got.ObjectKey)
	}
}
