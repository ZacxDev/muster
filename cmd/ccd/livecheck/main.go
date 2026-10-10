// Command livecheck sends ONE turn to a running ccd through muster's own chat
// client — agentgateway.Gateway, the code path muster's chat box and kickoff use
// — and prints the reply. It is the operator's live check for the
// claude-code-agent image; it is not part of any automated suite, because a real
// answer needs a real subscription token.
//
//	CCD_LIVECHECK_HOOKS_TOKEN=<the pod's HOOKS_TOKEN> \
//	  go run ./cmd/ccd/livecheck -url http://127.0.0.1:18789 "reply with exactly PONG"
//
// Exit 0 with the reply on stdout, or exit 1 with muster's error on stderr — which
// for a failed turn carries ccd's typed JSON body (auth_failed, rate_limited, …).
// The token is read from the environment, never from argv.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

type fixedEndpoint provision.Endpoint

func (f fixedEndpoint) Endpoint(context.Context, provision.Ref) (provision.Endpoint, error) {
	return provision.Endpoint(f), nil
}

func main() {
	rawURL := flag.String("url", "http://127.0.0.1:18789", "ccd's base URL (e.g. a kubectl port-forward)")
	timeout := flag.Duration("timeout", 10*time.Minute, "whole-turn deadline")
	session := flag.String("session", "livecheck", "the X-Openclaw-Session-Key muster would send")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: livecheck [-url U] \"<prompt>\"   (token in CCD_LIVECHECK_HOOKS_TOKEN)")
		os.Exit(2)
	}
	tok := os.Getenv("CCD_LIVECHECK_HOOKS_TOKEN")
	if tok == "" {
		fmt.Fprintln(os.Stderr, "livecheck: CCD_LIVECHECK_HOOKS_TOKEN is not set")
		os.Exit(2)
	}
	u, err := url.Parse(*rawURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "livecheck: -url:", err)
		os.Exit(2)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "livecheck: -url needs host:port:", err)
		os.Exit(2)
	}
	port, _ := strconv.Atoi(portStr)

	gw, err := agentgateway.New(agentgateway.Config{
		Driver:  fixedEndpoint{Scheme: u.Scheme, Host: host, Port: port, Path: u.Path},
		Runtime: agentgateway.HooksSHA256(),
		// ccd ignores `model`; muster requires the field to be non-empty.
		Model: "ccd",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "livecheck:", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	start := time.Now()
	reply, err := gw.Chat(ctx, agents.Agent{ID: 1, Name: "livecheck", HooksToken: tok}, *session, flag.Arg(0), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "livecheck: FAILED after %s: %v\n", time.Since(start).Round(time.Millisecond), err)
		os.Exit(1)
	}
	if reply == "" {
		fmt.Fprintln(os.Stderr, "livecheck: FAILED: muster's client returned an EMPTY reply with no error")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "livecheck: ok after %s, %d bytes\n", time.Since(start).Round(time.Millisecond), len(reply))
	fmt.Println(reply)
}
