package previewgw

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"sync"
	"testing"
)

// 🔴 An app's 1xx response reaches the browser through ReverseProxy's own
// Got1xxResponse hook, never through ModifyResponse, so the response rules
// must reach it another way (filter1xx). A 103 Early Hints carrying every
// kind of field the rules remove — a Set-Cookie with a Domain, Clear-Site-Data
// (which a browser applies to the whole registrable domain), the gateway's
// own error-code header, HSTS — reaches the browser with its Link only, and
// the final response is the app's 200 under the usual rules, with nothing of
// the 103 left in it. Through the whole gateway, observed by the browser's
// own client trace.
func TestAppInformationalResponseGetsTheResponseRules(t *testing.T) {
	const link = "</app.css>; rel=preload; as=style"
	e := newGW(t, gwOpts{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4376, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Link", link)
		h.Set("Set-Cookie", "hint=1; Domain="+testDomain+"; Path=/")
		h.Set("Clear-Site-Data", `"*"`)
		h.Set(ErrorCodeHeader, codePreviewClosed)
		h.Set("X-Muster-Owner", "2")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		w.WriteHeader(http.StatusEarlyHints)
		// net/http's server sends w.Header() with a 1xx and keeps it: the
		// app clears it for its final response, as a real one would.
		clear(h)
		h.Set("Content-Type", "text/plain")
		h.Set("X-App", "final")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "body")
	}))

	var mu sync.Mutex
	var codes []int
	var hints []textproto.MIMEHeader
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
		mu.Lock()
		defer mu.Unlock()
		codes = append(codes, code)
		cp := textproto.MIMEHeader{}
		for k, vs := range h {
			cp[k] = append([]string(nil), vs...)
		}
		hints = append(hints, cp)
		return nil
	}}
	req, err := http.NewRequest("GET", e.public.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
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

	mu.Lock()
	defer mu.Unlock()
	if len(codes) != 1 || codes[0] != http.StatusEarlyHints {
		t.Fatalf("the browser saw 1xx responses %v; want one 103", codes)
	}
	hint := http.Header(hints[0])
	if got := hint.Values("Link"); len(got) != 1 || got[0] != link {
		t.Errorf("the 103's Link %q; want %q", got, link)
	}
	for _, k := range []string{"Set-Cookie", "Clear-Site-Data", ErrorCodeHeader, "X-Muster-Owner",
		"Strict-Transport-Security"} {
		if v := hint.Values(k); len(v) != 0 {
			t.Errorf("the app's %s %q reached the browser on its 103", k, v)
		}
	}

	if resp.StatusCode != http.StatusOK || string(b) != "body" {
		t.Fatalf("final HTTP %d %q; want the app's 200 %q", resp.StatusCode, b, "body")
	}
	if got := resp.Header.Get("X-App"); got != "final" {
		t.Errorf("final X-App %q; want the app's own header", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("final Referrer-Policy %q; want the gateway's no-referrer", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("final X-Content-Type-Options %q; want nosniff", got)
	}
	for _, k := range []string{"Link", "Set-Cookie", "Clear-Site-Data", ErrorCodeHeader, "X-Muster-Owner",
		"Strict-Transport-Security"} {
		if v := resp.Header.Values(k); len(v) != 0 {
			t.Errorf("the 103's %s %q lingered into the final response", k, v)
		}
	}
}

// 🔴 filter1xx is a new trace per request: WithClientTrace rewrites the trace
// it is given, composing the older hook into it, so with one shared trace
// every request's ReverseProxy hook would chain onto it — and one app's 103
// would be written to every other in-flight request's browser (a shared trace
// was measured giving a request 3–8 Links instead of 1). Concurrent 103s,
// under -race: each browser sees exactly its own, filtered.
func TestInformationalFilterIsPerRequest(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4377, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</a.js>; rel=preload; as=script")
		w.Header().Set(ErrorCodeHeader, codePreviewClosed)
		w.WriteHeader(http.StatusEarlyHints)
		clear(w.Header())
		w.WriteHeader(http.StatusNoContent)
	}))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var mu sync.Mutex
			var leaked, links int
			trace := &httptrace.ClientTrace{Got1xxResponse: func(_ int, h textproto.MIMEHeader) error {
				mu.Lock()
				defer mu.Unlock()
				links += len(h.Values("Link"))
				leaked += len(h.Values(ErrorCodeHeader))
				return nil
			}}
			req, err := http.NewRequest("GET", e.public.URL+"/", nil)
			if err != nil {
				t.Error(err)
				return
			}
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
			req.Host = pHost(id)
			for k, vs := range own(id, cookie) {
				req.Header[k] = vs
			}
			resp, err := noRedirect.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if resp.StatusCode != http.StatusNoContent || links != 1 || leaked != 0 {
				t.Errorf("HTTP %d, %d Link and %d %s on the 103; want 204, 1 and 0",
					resp.StatusCode, links, leaked, ErrorCodeHeader)
			}
		}()
	}
	wg.Wait()
}
