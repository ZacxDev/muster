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
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	pt "github.com/ZacxDev/muster/spike/previewtunnel"
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
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		base := fs.String("base", "", "gateway base URL")
		crit := fs.String("crit", "", "1 | 4 | 5 | idlecut")
		tr := fs.String("transport", string(pt.H2), "h2connect | yamux")
		n := fs.Int("n", 300, "C1: echoes per side; C5: streams")
		idle := fs.Duration("idle", 10*time.Minute, "C4 idle; idlecut max wait")
		ping := fs.Duration("ping", 30*time.Second, "ping interval")
		noPings := fs.Bool("no-pings", false, "negative control: no pings on tunnel or client legs")
		gz := fs.Bool("gzip", false, "C5: request compression like a browser")
		_ = fs.Parse(os.Args[2:])
		e, err := pt.NewRemoteEnv(pt.RemoteOpts{Base: *base, Secret: secret(), Transport: pt.Transport(*tr),
			Ping: *ping, HostNoPings: *noPings, ClientNoPings: *noPings, Gzip: *gz})
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
		fmt.Printf("transport=%s crit=%s no-pings=%v gzip=%v elapsed=%v\n  %v\n", *tr, *crit, *noPings, *gz, time.Since(t0).Round(time.Millisecond), res)
		if err != nil {
			fmt.Printf("  VERDICT: FAIL: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("  VERDICT: pass")
	default:
		log.Fatal("usage: edgespike gateway|run [flags]")
	}
}
