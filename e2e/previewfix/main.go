// Command previewfix is the previews browser spec's fixture
// (e2e/tests/previews.spec.ts): the two processes around the real gateway and
// the real host that the spec cannot be itself.
//
//		previewfix edge -listen 127.0.0.1:<port> -upstream 127.0.0.1:<gateway public port> -log <file>
//		previewfix app  -listen 127.0.0.1:<port>[,127.0.0.1:<port>…] -log <file>
//
//	  - edge: the TLS edge in front of the gateway's public listener, as the
//	    real deployment's edge is (the gateway speaks plain HTTP and leaves TLS
//	    to the edge). A self-signed certificate made at start for the spec's
//	    zone (the browser runs with ignoreHTTPSErrors), HTTP/1.1 only, and a
//	    reverse proxy that changes NOTHING the gateway reads: the Host, every
//	    Sec-Fetch-* header, Origin and the cookies pass as the browser sent
//	    them, and no X-Forwarded-* is added. It writes one JSON line per
//	    request — what the gateway was sent (the Sec-Fetch-* values, Origin,
//	    the NAMES of the cookies, never their values), the status the gateway
//	    answered (101 for an upgrade) and its refusal code, if any — so the spec reads the real
//	    browser's Fetch Metadata as the gateway saw it.
//	  - app: the "dev server" a mirrored window runs: on each loopback address
//	    it serves a page whose script opens a WebSocket echo (as a dev server's
//	    hot-reload socket does) and an EventSource, the echo and the event
//	    stream themselves, /api/ping, /secret (what a sibling preview must not
//	    reach), and /attack?victim=<url> (a page that loads <url> in a hidden
//	    iframe and opens a WebSocket to <url>'s host /ws: a sibling preview
//	    reaching for another). Every request is one
//	    JSON line in the log: which listener, the method and the path, so the
//	    spec sees what reached an app and what never did.
//
// Every name and value here is invented for the suite; nothing is reached but
// loopback.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: previewfix edge|app …")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "edge":
		err = edge(ctx, os.Args[2:])
	case "app":
		err = app(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatalf("previewfix %s: %v", os.Args[1], err)
	}
}

// jsonLog appends one JSON object per line to a file, each write whole.
type jsonLog struct {
	mu sync.Mutex
	f  *os.File
}

func openLog(path string) (*jsonLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &jsonLog{f: f}, nil
}

func (l *jsonLog) write(v any) {
	b, _ := json.Marshal(v)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(append(b, '\n'))
}

// serve runs srv on ln until ctx ends.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		return err
	}
}

// ------------------------------------------------------------------ edge

// edgeRecord is one request as the gateway was sent it.
type edgeRecord struct {
	Host    string   `json:"host"`
	Method  string   `json:"method"`
	Path    string   `json:"path"`
	Site    string   `json:"sec_fetch_site"`
	Mode    string   `json:"sec_fetch_mode"`
	Dest    string   `json:"sec_fetch_dest"`
	User    string   `json:"sec_fetch_user"`
	Origin  string   `json:"origin"`
	Upgrade string   `json:"upgrade"`
	Cookies []string `json:"cookies"`
	Status  int      `json:"status"`
	// Code is the gateway's own refusal code (X-Muster-Preview-Error), or "".
	Code string `json:"code"`
}

// statusWriter records the status the gateway answered and logs the request
// once, when that status is known: at WriteHeader, at the first Write
// (an implicit 200), at a hijack (an upgrade: 101), or when the handler
// returns. A streamed answer (SSE, a WebSocket) is logged when it starts, not
// when it ends.
type statusWriter struct {
	http.ResponseWriter
	rec    edgeRecord
	log    *jsonLog
	logged bool
}

func (w *statusWriter) note(status int) {
	if !w.logged {
		w.logged = true
		w.rec.Status = status
		w.rec.Code = w.Header().Get("X-Muster-Preview-Error")
		w.log.write(w.rec)
	}
}

func (w *statusWriter) WriteHeader(status int) {
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.note(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.note(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.note(http.StatusSwitchingProtocols)
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func cookieNames(r *http.Request) []string {
	out := []string{}
	for _, c := range r.Cookies() {
		out = append(out, c.Name)
	}
	return out
}

func edge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("edge", flag.ContinueOnError)
	listen := fs.String("listen", "", "the TLS listener, host:port")
	upstream := fs.String("upstream", "", "the gateway's public listener, host:port")
	logPath := fs.String("log", "", "the request log (JSON lines)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *listen == "" || *upstream == "" || *logPath == "" {
		return fmt.Errorf("-listen, -upstream and -log are required")
	}
	lg, err := openLog(*logPath)
	if err != nil {
		return err
	}
	cert, err := selfSigned()
	if err != nil {
		return err
	}
	target := &url.URL{Scheme: "http", Host: *upstream}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host // the gateway routes by Host: pass the browser's
		},
		FlushInterval: -1,
		ErrorLog:      log.New(os.Stderr, "previewfix edge: ", 0),
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, log: lg, rec: edgeRecord{
			Host: r.Host, Method: r.Method, Path: r.URL.Path,
			Site: r.Header.Get("Sec-Fetch-Site"), Mode: r.Header.Get("Sec-Fetch-Mode"),
			Dest: r.Header.Get("Sec-Fetch-Dest"), User: r.Header.Get("Sec-Fetch-User"),
			Origin: r.Header.Get("Origin"), Upgrade: r.Header.Get("Upgrade"), Cookies: cookieNames(r),
		}}
		rp.ServeHTTP(sw, r)
		sw.note(http.StatusOK)
	})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	tln := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	return serve(ctx, srv, tln)
}

// selfSigned is a fresh certificate for the spec's zone, valid for a day.
func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "previewfix edge"},
		DNSNames:     []string{"*.preview.example.net", "preview.example.net"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// ------------------------------------------------------------------- app

type appRecord struct {
	Port   int    `json:"port"`
	Method string `json:"method"`
	Path   string `json:"path"`
}

// appPage is the app's page: it opens the WebSocket echo and the event
// stream on its own host (as a dev server's client does, from location.host)
// and writes what came back into the page.
const appPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Preview app %[1]d</title></head>
<body><h1 id="app">preview app on %[1]d</h1>
<p id="ws">ws: pending</p><p id="sse">sse: pending</p>
<script>
(function(){
var ws=document.getElementById('ws'),sse=document.getElementById('sse');
var sent='hmr-'+Math.random().toString(36).slice(2);
try{var s=new WebSocket((location.protocol==='https:'?'wss://':'ws://')+location.host+'/ws');
 s.onopen=function(){s.send(sent);};
 s.onmessage=function(e){ws.textContent=e.data==='echo:'+sent?'ws: echo ok':'ws: wrong echo '+e.data;};
 s.onerror=function(){ws.textContent='ws: error';};
 s.onclose=function(e){if(ws.textContent==='ws: pending')ws.textContent='ws: closed '+e.code;};
}catch(e){ws.textContent='ws: threw '+e;}
var es=new EventSource('/sse');
es.onmessage=function(e){if(sse.textContent==='sse: pending')sse.textContent='sse: '+e.data;};
es.onerror=function(){if(sse.textContent==='sse: pending')sse.textContent='sse: error';};
})();
</script></body></html>`

// attackPage loads the victim URL in a hidden iframe and opens a WebSocket to
// the victim's /ws (a hostile page's script reaching for another preview's
// hot-reload socket), writing what came of it into #wsattack.
const attackPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Sibling %[1]d</title></head>
<body><h1 id="attacker">sibling preview on %[1]d</h1>
<p id="wsattack">wsattack: pending</p>
<iframe id="victim" hidden src="%[2]s"></iframe>
<script>
(function(){
var out=document.getElementById('wsattack');
try{var s=new WebSocket('wss://'+new URL(document.getElementById('victim').src).host+'/ws');
 s.onopen=function(){out.textContent='wsattack: open';};
 s.onerror=function(){out.textContent='wsattack: error';};
}catch(e){out.textContent='wsattack: threw '+e;}
})();
</script></body></html>`

func appHandler(port int, lg *jsonLog) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, appPage, port)
	})
	mux.HandleFunc("GET /attack", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query().Get("victim")
		if !strings.HasPrefix(v, "https://") {
			http.Error(w, "victim must be an https URL", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, attackPage, port, html.EscapeString(v))
	})
	mux.HandleFunc("GET /secret", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "the secret of the app on %d\n", port)
	})
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"pong":%d}`+"\n", port)
	})
	// The echo, with the library's default Origin check: the Origin must name
	// the request's Host, as a dev server's hot-reload socket checks it. The
	// gateway rewrites the preview's Origin to http://localhost:<port> for
	// exactly this (internal/previewgw wsrelay.go).
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, append([]byte("echo:"), b...)); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("GET /sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		rc := http.NewResponseController(w)
		fmt.Fprintf(w, "data: hello from %d\n\n", port)
		if rc.Flush() != nil {
			return
		}
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for n := 1; ; n++ {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
				fmt.Fprintf(w, "data: tick %d\n\n", n)
				if rc.Flush() != nil {
					return
				}
			}
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lg.write(appRecord{Port: port, Method: r.Method, Path: r.URL.Path})
		mux.ServeHTTP(w, r)
	})
}

func app(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	listen := fs.String("listen", "", "comma-separated loopback host:port listeners")
	logPath := fs.String("log", "", "the request log (JSON lines)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *listen == "" || *logPath == "" {
		return fmt.Errorf("-listen and -log are required")
	}
	lg, err := openLog(*logPath)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errc := make(chan error, 8)
	for _, addr := range strings.Split(*listen, ",") {
		_, ps, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		port, err := strconv.Atoi(ps)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: appHandler(port, lg), ReadHeaderTimeout: 10 * time.Second}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serve(ctx, srv, ln); err != nil {
				errc <- err
			}
		}()
	}
	// Ready: every listener is bound.
	fmt.Println("listening")
	wg.Wait()
	close(errc)
	return <-errc
}
