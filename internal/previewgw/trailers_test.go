package previewgw

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// Every name an app may not send as a trailer, and that the check is on the
// name's case-insensitive form; an app's other trailers pass.
func TestDropAppNonFinalNames(t *testing.T) {
	for name, want := range map[string]bool{
		"X-Muster-Preview-Error":    true,
		"x-muster-owner":            true,
		"X-MUSTER-anything":         true,
		"Clear-Site-Data":           true,
		"strict-transport-security": true,
		"Referrer-Policy":           true,
		"X-Content-Type-Options":    true,
		"content-security-policy":   true,
		"Location":                  true,
		"set-cookie":                true,
		"X-App-Trailer":             false,
		"Server-Timing":             false,
		"X-Muster":                  false, // no dash: not the gateway's prefix
	} {
		if got := dropAppNonFinal(name); got != want {
			t.Errorf("dropAppNonFinal(%q) = %v; want %v", name, got, want)
		}
	}
}

// 🔴 An event-stream response with trailers must not race the keepalive
// reader. The transport writes resp.Trailer inside the Read that reaches
// EOF — for an event stream, on sseBody's goroutine — while ReverseProxy
// reads resp.Trailer to build the Trailer: line and, after the body, to copy
// it, and the gateway strips it. A concurrent map read and write is a fatal
// runtime error that takes every preview down with it. Run under -race: a
// browser that reads to the end (the app's own trailer arrives, a forged one
// does not), through the whole gateway.
func TestEventStreamTrailersDoNotRaceTheReader(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4375, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Trailer", ErrorCodeHeader+", X-App-Trailer")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: x\n\n")
		w.(http.Flusher).Flush()
		w.Header().Set(ErrorCodeHeader, codePreviewClosed)
		w.Header().Set("X-App-Trailer", "t17")
		w.Header().Set(http.TrailerPrefix+"X-Muster-Owner", "2")
	}))
	for i := 0; i < 20; i++ {
		req, err := http.NewRequest("GET", e.public.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = pHost(id)
		for k, vs := range own(id, cookie) {
			req.Header[k] = vs
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(b) != "data: x\n\n" {
			t.Fatalf("HTTP %d %q; want the app's stream", resp.StatusCode, b)
		}
		if got := resp.Trailer.Get("X-App-Trailer"); got != "t17" {
			t.Fatalf("the app's own trailer on an event stream %q; want t17", got)
		}
		for _, k := range []string{ErrorCodeHeader, "X-Muster-Owner"} {
			if v := resp.Trailer.Values(k); len(v) != 0 {
				t.Fatalf("the app's %s trailer %q reached the browser on an event stream", k, v)
			}
		}
	}
}

// trailerWritingBody is an upstream body that, like net/http's transport,
// writes resp.Trailer inside the Read that returns io.EOF.
type trailerWritingBody struct {
	resp  *http.Response
	reads int
}

func (b *trailerWritingBody) Read(p []byte) (int, error) {
	b.reads++
	if b.reads == 1 {
		return copy(p, "data: x\n\n"), nil
	}
	b.resp.Trailer.Set(ErrorCodeHeader, codePreviewClosed)
	b.resp.Trailer["X-Muster-Owner"] = []string{"2"}
	return 0, io.EOF
}

func (b *trailerWritingBody) Close() error { return nil }

// 🔴 A browser that leaves mid-stream: ReverseProxy closes the body before
// it reached EOF, while sseBody's goroutine may be inside the upstream Read
// writing resp.Trailer. Nothing orders that write with Close, so Close must
// not touch the map (ReverseProxy copies no trailers after an aborted body).
// Under -race this is deterministic: the upstream's write happens during the
// sleep, unsynchronised with Close.
func TestClosingAnEventStreamBeforeItsEndDoesNotTouchItsTrailers(t *testing.T) {
	for i := 0; i < 5; i++ {
		resp := &http.Response{Trailer: http.Header{}}
		src := &trailerWritingBody{resp: resp}
		body := &trailerFilter{ReadCloser: newSSEBody(src, time.Hour), resp: resp}
		buf := make([]byte, 64)
		if n, err := body.Read(buf); err != nil || string(buf[:n]) != "data: x\n\n" {
			t.Fatalf("first read %q %v", buf[:n], err)
		}
		time.Sleep(20 * time.Millisecond) // the goroutine reaches EOF, writing resp.Trailer
		_ = body.Close()
	}
}
