package api

import (
	"bytes"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/pages"
)

// slowUpload starts one upload through link token whose body stops after the
// file's first bytes, so the handler sits inside the store's read holding its
// slot. finish sends the rest and closes it; the answer arrives on done.
func (f *pageFixture) slowUpload(t *testing.T, token, component string) (finish func(), done <-chan *httptest.ResponseRecorder) {
	t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	ch := make(chan *httptest.ResponseRecorder, 1)
	req := httptest.NewRequest(http.MethodPost, pages.LinkPath(token)+"/upload", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	go func() {
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, req)
		ch <- w
	}()
	_ = mw.WriteField("component", component)
	fw, _ := mw.CreateFormFile("file", "slow.bin")
	_, _ = fw.Write([]byte("the first bytes of a file that is still arriving"))
	return func() {
		_, _ = fw.Write([]byte(" and the rest"))
		_ = mw.Close()
		_ = pw.Close()
	}, ch
}

// slotsHeld is every upload slot in flight: the pages' own and the composer's.
func (f *pageFixture) slotsHeld() int {
	f.srv.pageState.uploadSlots.mu.Lock()
	n := f.srv.pageState.uploadSlots.total
	f.srv.pageState.uploadSlots.mu.Unlock()
	f.srv.uploadLimits.mu.Lock()
	defer f.srv.uploadLimits.mu.Unlock()
	return n + f.srv.uploadLimits.total
}

func pageWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// composerUpload is the owner's own attach in the composer (POST /uploads).
func (f *pageFixture) composerUpload(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", "mine.txt")
	_, _ = fw.Write([]byte("the owner's own file"))
	_ = mw.Close()
	return f.call(t, http.MethodPost, "/uploads", &b, merge(f.ownerHdr(), map[string]string{"Content-Type": mw.FormDataContentType()}))
}

// Y2. TestAStrangersUploadsNeverTakeTheOwnersSlots: two strangers' uploads
// stalled mid-body on two of the owner's links hold their slots; the owner's
// own composer attach is still answered, not "busy".
func TestAStrangersUploadsNeverTakeTheOwnersSlots(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Slots", formBlocks)
	a, b := f.liveLink(t, f.owner, id), f.liveLink(t, f.owner, id)
	finishA, doneA := f.slowUpload(t, a.Token, "u1")
	finishB, doneB := f.slowUpload(t, b.Token, "u1")
	pageWaitFor(t, "two page uploads in flight", func() bool { return f.slotsHeld() == 2 })
	if w := f.composerUpload(t); w.Code != http.StatusCreated {
		t.Errorf("the owner's composer upload while strangers' uploads stall: %d %s", w.Code, w.Body.String())
	}
	finishA()
	finishB()
	for _, d := range []<-chan *httptest.ResponseRecorder{doneA, doneB} {
		if w := <-d; w.Code != http.StatusOK {
			t.Errorf("a stalled page upload, finished: %d %s", w.Code, w.Body.String())
		}
	}
}

// Y3. TestAPagesFilesStopAtItsByteQuota: with a quota of 100 bytes, 70 bytes
// are taken; 31 more (one over) are refused, saying how much room is left;
// exactly 30 more fill it; then any byte is refused as full. Sizes are odd on
// purpose: none is a multiple of another or of a power of two.
func TestAPagesFilesStopAtItsByteQuota(t *testing.T) {
	f := newPageFixture(t)
	f.srv.ext.PagesUploadBytes = 100
	id := f.create(t, f.agent, "Quota", formBlocks)
	l := f.liveLink(t, f.owner, id)
	up := func(n int) *httptest.ResponseRecorder {
		return f.upload(t, l.Token, "u1", "f.bin", bytes.Repeat([]byte{'q'}, n))
	}
	if w := up(70); w.Code != http.StatusOK {
		t.Fatalf("70 of 100: %d %s", w.Code, w.Body.String())
	}
	if w := up(31); w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "too large for this page") ||
		!strings.Contains(w.Body.String(), "room for 30 B more") {
		t.Fatalf("31 more (101 of 100): %d %s", w.Code, w.Body.String())
	}
	if w := up(30); w.Code != http.StatusOK {
		t.Fatalf("30 more (exactly 100): %d %s", w.Code, w.Body.String())
	}
	if w := up(1); w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "cannot take any more files") {
		t.Fatalf("1 more (101 of 100, full): %d %s", w.Code, w.Body.String())
	}
	if n := len(f.objects.Keys()); n != 2 {
		t.Errorf("%d objects stored, want the 2 that fit", n)
	}
}

// Y1a. TestDeletingAPageLeavesNoObjectAnUploadRacedIn: an upload already past
// the link door when the page is deleted — its bytes still arriving — is
// finished at the worst moment, right after the page's prefix is swept and
// before the page row goes. Its link was revoked first, so it is not recorded
// and takes its object back. And an object under the prefix that no row
// names (one that landed in the window) is deleted by the listing. Nothing is
// left under pages/<id>/.
func TestDeletingAPageLeavesNoObjectAnUploadRacedIn(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Race", formBlocks)
	sid := strconv.FormatInt(id, 10)
	l := f.liveLink(t, f.owner, id)
	prefix := "pages/" + sid + "/"
	f.objects.Seed(prefix+strings.Repeat("R", 22), []byte("landed in the window"))
	finish, done := f.slowUpload(t, l.Token, "u1")
	pageWaitFor(t, "the upload in flight", func() bool { return f.slotsHeld() == 1 })
	var raced *httptest.ResponseRecorder
	f.objects.AfterDeletePrefix = func() {
		f.objects.AfterDeletePrefix = nil
		finish()
		raced = <-done
	}
	if w := f.call(t, http.MethodPost, "/pages/"+sid+"/delete", nil, f.ownerHdr()); w.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if raced == nil {
		t.Fatal("the hook never ran: the delete did not sweep the prefix")
	}
	if raced.Code == http.StatusOK {
		t.Errorf("an upload finished during the delete was recorded: %s", raced.Body.String())
	}
	for _, k := range f.objects.Keys() {
		if strings.HasPrefix(k, prefix) {
			t.Errorf("deleting the page left %s", k)
		}
	}
}

// Y1b. TestAnUploadThatCannotBeTakenBackIsLoggedAndCounted: a file stored but
// refused at recording (the per-link file cap) whose object cannot be deleted
// is logged — ids only — and counted on
// muster_page_upload_rollback_failures_total.
func TestAnUploadThatCannotBeTakenBackIsLoggedAndCounted(t *testing.T) {
	f := newPageFixture(t)
	var logged bytes.Buffer
	f.srv.logger = log.New(&logged, "", 0)
	id := f.create(t, f.agent, "Rollback", formBlocks)
	l := f.liveLink(t, f.owner, id)
	defer func(n int) { pages.MaxUploadsPerLink = n }(pages.MaxUploadsPerLink)
	pages.MaxUploadsPerLink = 0
	before := testutil.ToFloat64(metrics.PageUploadRollbackFailures)
	f.objects.FailDelete = errSentinel("store unreachable")
	w := f.upload(t, l.Token, "u1", "f.bin", []byte("refused at recording"))
	f.objects.FailDelete = nil
	if w.Code == http.StatusOK {
		t.Fatalf("the upload was recorded: %s", w.Body.String())
	}
	if got := testutil.ToFloat64(metrics.PageUploadRollbackFailures) - before; got != 1 {
		t.Errorf("rollback failures counted %v, want 1", got)
	}
	if !strings.Contains(logged.String(), "could not be taken back") || !strings.Contains(logged.String(), "store unreachable") ||
		strings.Contains(logged.String(), l.Token) {
		t.Errorf("the log: %s", logged.String())
	}
	// The control: with a store that deletes, the same refusal counts nothing.
	before = testutil.ToFloat64(metrics.PageUploadRollbackFailures)
	_ = f.upload(t, l.Token, "u1", "f.bin", []byte("refused at recording"))
	if got := testutil.ToFloat64(metrics.PageUploadRollbackFailures) - before; got != 0 {
		t.Errorf("a taken-back upload counted %v", got)
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// G1. TestAThrottledAddressStillOpensALiveLink: past the failed-lookup limit
// an address's misses are 429, and its live link is still served.
func TestAThrottledAddressStillOpensALiveLink(t *testing.T) {
	f := newPageFixture(t)
	defer func(m int) { pageMissLimit = m }(pageMissLimit)
	pageMissLimit = 3
	id := f.create(t, f.agent, "Throttled", formBlocks)
	l := f.liveLink(t, f.owner, id)
	for i := 0; i < 3; i++ {
		_ = f.get(t, pages.LinkPath(strings.Repeat("Y", 43)), nil)
	}
	if w := f.get(t, pages.LinkPath(strings.Repeat("Y", 43)), nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("control: the address is throttled: %d", w.Code)
	}
	if w := f.get(t, pages.LinkPath(l.Token), nil); w.Code != http.StatusOK {
		t.Fatalf("a live link from the throttled address: %d", w.Code)
	}
}

// G2. TestTheContentIsServedOnlyIntoAFrame: a browser opening a page's
// content on its own (Sec-Fetch-Dest: document) gets the one 404; framed
// (iframe), or by a client that says nothing, it is served sandboxed. The
// owner's preview content is the same.
func TestTheContentIsServedOnlyIntoAFrame(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Framed", `[{"type":"markdown","text":"framed-canary"}]`)
	l := f.liveLink(t, f.owner, id)
	for _, p := range []struct {
		path string
		hdr  map[string]string
	}{{pages.LinkPath(l.Token) + "/content", map[string]string{}}, {"/pages/" + strconv.FormatInt(id, 10) + "/versions/1/content", f.ownerHdr()}} {
		top := f.get(t, p.path, merge(p.hdr, map[string]string{"Sec-Fetch-Dest": "document"}))
		if top.Code != http.StatusNotFound || strings.Contains(top.Body.String(), "framed-canary") {
			t.Errorf("%s opened on its own: %d %s", p.path, top.Code, top.Body.String())
		}
		for _, dest := range []string{"iframe", ""} {
			h := merge(p.hdr, map[string]string{})
			if dest != "" {
				h["Sec-Fetch-Dest"] = dest
			}
			w := f.get(t, p.path, h)
			assertContentHeaders(t, w, p.path+" dest="+dest)
			if !strings.Contains(w.Body.String(), "framed-canary") {
				t.Errorf("%s dest=%q: not the page", p.path, dest)
			}
		}
	}
}
