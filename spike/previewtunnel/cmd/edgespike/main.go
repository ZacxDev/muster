// edgespike runs the preview-tunnel spike against a REAL edge.
//
//	edgespike gateway  -listen :8080          (secret from $SPIKE_SECRET)
//	edgespike run -base https://host -crit 1|4|5|idlecut [-transport h2connect|yamux] ...
//
// `run` starts the test upstream and the host in this process (so SSE timing
// shares one clock), dials the gateway's tunnel through the edge, and runs the
// chosen criterion against the same base URL. Throwaway spike code.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	pt "github.com/ZacxDev/muster/spike/previewtunnel"
	"github.com/coder/websocket"
)

func secret() string {
	s := strings.TrimSpace(os.Getenv("SPIKE_SECRET"))
	if f := os.Getenv("SPIKE_SECRET_FILE"); s == "" && f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			log.Fatalf("secret file: %v", err)
		}
		s = strings.TrimSpace(string(b))
	}
	if len(s) < 32 {
		log.Fatal("SPIKE_SECRET (or SPIKE_SECRET_FILE) must hold >= 32 chars (128-bit hex)")
	}
	return s
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: edgespike gateway|run [flags]")
	}
	switch os.Args[1] {
	case "gateway":
		fs := flag.NewFlagSet("gateway", flag.ExitOnError)
		listen := fs.String("listen", ":8080", "listen address")
		ping := fs.Duration("ping", 30*time.Second, "WebSocket ping interval")
		_ = fs.Parse(os.Args[2:])
		g := &pt.RemoteGateway{Secret: secret(), Ping: *ping}
		log.Printf("gateway listening on %s", *listen)
		log.Fatal((&http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
	case "idlews":
		// Hop discriminator for criterion 4: one no-ping WebSocket to the gateway's
		// own /__bare_echo (no tunnel involved), optionally pinned to an IP so it
		// can skip Cloudflare and hit the origin directly. Prints when it was cut.
		fs := flag.NewFlagSet("idlews", flag.ExitOnError)
		base := fs.String("base", "", "gateway base URL (https://host)")
		connect := fs.String("connect", "", "dial this ip:port instead (origin-direct; TLS not verified)")
		maxW := fs.Duration("max", 5*time.Minute, "give up after")
		_ = fs.Parse(os.Args[2:])
		if dns := os.Getenv("SPIKE_DNS"); dns != "" {
			net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, dns)
			}}
		}
		tr := &http.Transport{}
		if *connect != "" {
			tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, *connect)
			}
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Traefik's default cert; Cloudflare "full" does the same
		}
		ctx, cancel := context.WithTimeout(context.Background(), *maxW+10*time.Second)
		defer cancel()
		u := "wss" + strings.TrimPrefix(strings.TrimRight(*base, "/"), "https") + "/__bare_echo"
		c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr},
			HTTPHeader: http.Header{pt.SecretHeader: {secret()}, pt.NoPingsHeader: {"1"}}})
		if err != nil {
			log.Fatalf("dial: %v", err)
		}
		if err := c.Write(ctx, websocket.MessageText, []byte("x")); err != nil {
			log.Fatalf("write: %v", err)
		}
		if _, p, err := c.Read(ctx); err != nil || string(p) != "x" {
			log.Fatalf("first echo: %v", err)
		}
		t0 := time.Now()
		done := make(chan error, 1)
		go func() { _, _, err := c.Read(ctx); done <- err }()
		select {
		case err := <-done:
			fmt.Printf("idlews connect=%q cut after %v: %v\n", *connect, time.Since(t0).Round(100*time.Millisecond), err)
		case <-time.After(*maxW):
			fmt.Printf("idlews connect=%q alive after %v (no cut)\n", *connect, *maxW)
		}
	case "run":
		// SPIKE_DNS=1.1.1.1:53 resolves through that server instead of the
		// host's (whose cache may hold an NXDOMAIN from before the record existed).
		if dns := os.Getenv("SPIKE_DNS"); dns != "" {
			net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, dns)
			}}
		}
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		base := fs.String("base", "", "gateway base URL")
		hostBase := fs.String("host-base", "", "host's base URL to the gateway (e.g. http://<mesh-ip>:<port>); default -base")
		crit := fs.String("crit", "", "1 | 4 | 5 | idlecut")
		tr := fs.String("transport", string(pt.H2), "h2connect | yamux")
		n := fs.Int("n", 300, "C1: echoes per side; C5: streams")
		idle := fs.Duration("idle", 10*time.Minute, "C4 idle; idlecut max wait")
		ping := fs.Duration("ping", 30*time.Second, "ping interval")
		noPings := fs.Bool("no-pings", false, "negative control: no pings on tunnel or client legs")
		gz := fs.Bool("gzip", false, "C5: request compression like a browser")
		noFlush := fs.Bool("host-no-flush", false, "C5 negative control: host flushes only the response head")
		_ = fs.Parse(os.Args[2:])
		e, err := pt.NewRemoteEnv(pt.RemoteOpts{Base: *base, HostBase: *hostBase, Secret: secret(), Transport: pt.Transport(*tr),
			Ping: *ping, HostNoPings: *noPings, ClientNoPings: *noPings, Gzip: *gz, HostNoFlush: *noFlush})
		if err != nil {
			log.Fatalf("env: %v", err)
		}
		defer e.Close()
		t0 := time.Now()
		var res fmt.Stringer
		switch *crit {
		case "1":
			res, err = pt.CheckC1(e, *n)
		case "4":
			res, err = pt.CheckC4(e, *idle)
		case "5":
			res, err = pt.CheckC5(e, *n, 2*time.Second)
		case "idlecut":
			res, err = pt.MeasureIdleCut(e, *idle)
		default:
			log.Fatalf("unknown -crit %q", *crit)
		}
		fmt.Printf("transport=%s crit=%s host-base=%q no-pings=%v gzip=%v elapsed=%v\n  %v\n", *tr, *crit, *hostBase, *noPings, *gz, time.Since(t0).Round(time.Millisecond), res)
		if err != nil {
			fmt.Printf("  VERDICT: FAIL: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("  VERDICT: pass")
	default:
		log.Fatal("usage: edgespike gateway|run [flags]")
	}
}
