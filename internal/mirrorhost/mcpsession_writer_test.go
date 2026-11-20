package mirrorhost

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTheNotifyingWriterKeepsALongCallAlive: muster keeps a long call alive
// with leading whitespace. Answered as an event stream, that whitespace is an
// SSE comment (sent at once, so the connection stays open) and never part of
// the response's data; the response follows the notification whole. A non-200
// answer is relayed untouched, with no notification.
func TestTheNotifyingWriterKeepsALongCallAlive(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := &listChangedWriter{w: rec}
	lw.Header().Set("Content-Type", "application/json")
	lw.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(lw, "\n \n")
	if got := rec.Body.String(); got != listChangedEvent+": keep-alive\n\n" {
		t.Fatalf("before the answer: %q", got)
	}
	_, _ = io.WriteString(lw, `{"jsonrpc":"2.0","id":7,`)
	_, _ = io.WriteString(lw, `"result":{}}`+"\n")
	if !lw.finish() {
		t.Fatal("finish reported no notification")
	}
	want := listChangedEvent + ": keep-alive\n\n" + "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\n"
	if got := rec.Body.String(); got != want || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream %q (%q), want %q", got, rec.Header().Get("Content-Type"), want)
	}

	// A refusal is relayed as it is.
	rec = httptest.NewRecorder()
	lw = &listChangedWriter{w: rec}
	lw.Header().Set("Content-Type", "application/json")
	lw.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(lw, `{"error":"muster could not be reached"}`)
	if lw.finish() || rec.Code != http.StatusBadGateway || rec.Body.String() != `{"error":"muster could not be reached"}` {
		t.Fatalf("a 502 was not relayed untouched: %d %q", rec.Code, rec.Body)
	}
}
