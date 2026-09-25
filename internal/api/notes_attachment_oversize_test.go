package api

import (
	"bytes"
	"context"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// THE OVERSIZE ATTACHMENT MUST REACH THE USER, NOT THE LOG.
//
// The defect this pins: handleNoteCreate used to `continue` past any attachment
// over maxAttachmentBytes, log a line, and still fall through to
// renderNotesPanel — a 200. htmx swaps a 200, the create form's
// `hx-on::after-request` closes the modal on `event.detail.successful`, and
// resyncScript's toast listener is bound to `htmx:responseError`. A 200 is not
// an error, so the task saved, the modal closed, and the attachment was gone
// with nothing on screen.
//
// 🔴 THE ASSERTIONS ARE ON STATE, NOT ON WORDING. The behaviour that matters is
// (a) the caller is told the request failed — a non-2xx, which is the ONLY
// thing htmx surfaces — and (b) nothing was written, so the obvious retry does
// not duplicate the task. Both are checked against the store's recorded calls
// and the response status. The one string assertion is the FILENAME, which the
// user needs in order to know which file to remove; it is a value the test
// supplies, not a phrase the handler chooses, so a reword cannot walk it.
// ---------------------------------------------------------------------------

// attachCaptureStore records the writes handleNoteCreate performs and serves the
// minimum reads renderNotesPanel needs.
//
// The embedded notes.Store is nil on purpose: any method this test does not
// override panics rather than returning a plausible zero value, so a handler
// change that starts calling something else fails loudly instead of passing
// against an empty answer it never asked for.
type attachCaptureStore struct {
	notes.Store

	created  []notes.Note
	attached []notes.Attachment
}

func (s *attachCaptureStore) Create(_ context.Context, n notes.Note) (notes.Note, error) {
	s.created = append(s.created, n)
	n.ID = int64(len(s.created))
	return n, nil
}

func (s *attachCaptureStore) AddAttachment(_ context.Context, noteID int64, a notes.Attachment) (notes.Attachment, error) {
	a.NoteID = noteID
	a.ID = int64(len(s.attached) + 1)
	s.attached = append(s.attached, a)
	return a, nil
}

func (s *attachCaptureStore) ListPage(context.Context, notes.ListFilter) (notes.Page, error) {
	return notes.Page{}, nil
}

func (s *attachCaptureStore) TagVocabulary(context.Context) ([]notes.TagCount, error) {
	return nil, nil
}

// attachmentCreateRequest builds the exact multipart POST /tasks the create
// modal sends: a `body` field plus one `attachments` file part of `size` bytes.
// The part is written byte-for-byte so mime/multipart's own FileHeader.Size —
// the field the handler branches on — is real rather than asserted.
func attachmentCreateRequest(t *testing.T, filename string, size int) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("body", "a task that carries an attachment"); err != nil {
		t.Fatalf("write body field: %v", err)
	}
	part, err := mw.CreateFormFile("attachments", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	// A repeating non-zero pattern rather than zeroes, so a truncation shows up
	// as a length difference and not as bytes that happen to compare equal.
	chunk := bytes.Repeat([]byte("muster-attachment-payload"), 4096)
	for written := 0; written < size; {
		n := len(chunk)
		if remaining := size - written; remaining < n {
			n = remaining
		}
		if _, err := part.Write(chunk[:n]); err != nil {
			t.Fatalf("write attachment bytes: %v", err)
		}
		written += n
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func newAttachmentTestServer(store *attachCaptureStore) *Server {
	s := New(nil, AuthConfig{}, log.New(&bytes.Buffer{}, "", 0))
	s.ext = Extensions{Notes: store}
	return s
}

func TestNoteCreateRefusesAnOversizeAttachmentInsteadOfDroppingIt(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		// The boundary: one byte over is over.
		{name: "one byte over the cap", size: maxAttachmentBytes + 1},
		// And well over, so a guard whose threshold drifted by a page still fails
		// here. Deliberately not a multiple of the cap.
		{name: "comfortably over the cap", size: maxAttachmentBytes + 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &attachCaptureStore{}
			s := newAttachmentTestServer(store)
			rec := httptest.NewRecorder()

			s.handleNoteCreate(rec, attachmentCreateRequest(t, "quarterly-capture.mp4", tc.size))

			// (a) The caller must LEARN. htmx only surfaces a non-2xx: a 200 is
			// swapped, closes the modal, and fires no toast at all.
			if rec.Code < 400 {
				t.Errorf("oversize attachment (%d bytes, cap %d) answered HTTP %d; "+
					"anything below 400 is swapped by htmx as a success and the user is told nothing",
					tc.size, maxAttachmentBytes, rec.Code)
			}
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("oversize attachment answered HTTP %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
			}

			// (b) NOTHING may be written. A task saved behind a failed response is
			// the retry-duplicates-it trap.
			if len(store.created) != 0 {
				t.Errorf("oversize attachment still created %d note(s); the request must be atomic, "+
					"or the user's obvious retry duplicates the task", len(store.created))
			}
			if len(store.attached) != 0 {
				t.Errorf("oversize attachment still stored %d attachment(s), want 0", len(store.attached))
			}

			// (c) The user has to know WHICH file. The filename is a value this
			// test supplied, not a phrase the handler picked.
			body := rec.Body.String()
			if !strings.Contains(body, "quarterly-capture.mp4") {
				t.Errorf("response body does not name the rejected file, so the user cannot tell which "+
					"attachment to remove; body = %q", body)
			}
			// resyncScript's htmx:responseError listener discards a body beginning
			// with '<' and shows a generic message instead, so an HTML error page
			// would lose the filename on the way to the toast.
			if trimmed := strings.TrimSpace(body); strings.HasPrefix(trimmed, "<") {
				t.Errorf("error body starts with '<'; resyncScript replaces such a body with a generic "+
					"message, dropping the filename. body = %q", body)
			}
		})
	}
}

// The positive control. Without it, every zero above is indistinguishable from a
// test wired to nothing: a handler that refused EVERY create would satisfy all
// of the assertions in the case above.
func TestNoteCreateStillStoresAnAttachmentAtTheCap(t *testing.T) {
	store := &attachCaptureStore{}
	s := newAttachmentTestServer(store)
	rec := httptest.NewRecorder()

	s.handleNoteCreate(rec, attachmentCreateRequest(t, "exactly-at-the-cap.bin", maxAttachmentBytes))

	if rec.Code != http.StatusOK {
		t.Fatalf("an attachment of exactly %d bytes (the cap) answered HTTP %d, want 200; "+
			"the limit is inclusive and this is the boundary in the other direction",
			maxAttachmentBytes, rec.Code)
	}
	if len(store.created) != 1 {
		t.Fatalf("created %d note(s), want 1 — the harness never reached the create path, "+
			"so the zeroes asserted for the oversize case prove nothing", len(store.created))
	}
	if len(store.attached) != 1 {
		t.Fatalf("stored %d attachment(s), want 1", len(store.attached))
	}
	got := store.attached[0]
	if got.Filename != "exactly-at-the-cap.bin" {
		t.Errorf("stored attachment filename = %q, want %q", got.Filename, "exactly-at-the-cap.bin")
	}
	if len(got.Data) != maxAttachmentBytes {
		t.Errorf("stored %d bytes, want %d — the payload was truncated on the way in", len(got.Data), maxAttachmentBytes)
	}
	if got.SizeBytes != int64(maxAttachmentBytes) {
		t.Errorf("stored SizeBytes = %d, want %d", got.SizeBytes, maxAttachmentBytes)
	}
}
