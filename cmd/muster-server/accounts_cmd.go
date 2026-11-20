package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/loopback"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/users"
)

// runAccountCommand runs a one-shot account command and returns the process
// exit code. They are subcommands of the SERVER binary, not of the `muster`
// CLI, on purpose: they need DATABASE_URL (and, for a disable, the GoTrue
// secret), which live in the server's environment and nowhere else — so an
// operator runs them with `kubectl exec` into the running pod.
//
//	muster-server invite --claim-admin   print a single-use link that binds a
//	                                     new GoTrue login to user 1
//	muster-server user disable <id>      disabled_at + GoTrue ban, and the
//	                                     user's queued messages cancelled
//	muster-server user enable <id>       clear both
func runAccountCommand(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: muster-server invite --claim-admin\n"+
			"       muster-server user disable|enable <user-id>")
		return 2
	}
	cfg, err := loadConfig(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 1
	}
	if cfg.Database == "" {
		fmt.Fprintf(stderr, "%s is not set\n", envDatabase)
		return 1
	}
	switch {
	case len(args) == 2 && args[0] == "invite" && args[1] == "--claim-admin":
		if !cfg.goTrueConfigured() {
			fmt.Fprintf(stderr, "%s and %s are not set: a claim link would lead to a page that cannot "+
				"create the login. Configure GoTrue first\n", envGoTrueURL, envGoTrueSecret)
			return 1
		}
		pool, err := db.Connect(ctx, cfg.Database)
		if err != nil {
			fmt.Fprintf(stderr, "db connect: %v\n", err)
			return 1
		}
		defer pool.Close()
		base, note := claimLinkBase(cfg)
		link, err := api.NewAdminClaimLink(ctx, users.NewPG(pool), base, time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "claim link: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\n", link)
		fmt.Fprintf(stderr, "single-use, valid for %s; any earlier unredeemed claim link is now revoked.\n"+
			"Open it at the public address — the machine-token port refuses it.\n", api.ClaimTTL)
		if note != "" {
			fmt.Fprintln(stderr, note)
		}
		return 0
	case len(args) == 3 && args[0] == "user" && (args[1] == "disable" || args[1] == "enable"):
		id, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil || id <= 0 {
			return usage()
		}
		if !cfg.goTrueConfigured() {
			fmt.Fprintf(stderr, "%s and %s are not set: there is no GoTrue login to ban\n", envGoTrueURL, envGoTrueSecret)
			return 1
		}
		pool, err := db.Connect(ctx, cfg.Database)
		if err != nil {
			fmt.Fprintf(stderr, "db connect: %v\n", err)
			return 1
		}
		defer pool.Close()
		disable := args[1] == "disable"
		// Audited as the CLI (no muster user: the operator at a shell), in the
		// same transaction as the change.
		cli := users.Actor{Via: "cli"}
		// A disable cancels the user's queued messages, as the admin page's
		// does (api.Server.DisableUserAs) — after the row has changed, so a
		// refused disable cancels nothing. A running server's delivery refuses
		// them anyway (the account is re-read at send time); cancelling makes
		// the queue's list say so.
		var cancelled func()
		if disable {
			cancelled = func() {
				// Everything they still have queued: their own, and what they
				// queued into their teams' windows (team sharing 2b.4).
				n, err := sendqueue.NewPG(pool).CancelUser(ctx, id, time.Now(), api.SendQueueCancelDisabled)
				if err != nil {
					fmt.Fprintf(stderr, "user %d: cancel their queued messages: %v (a running server still refuses to send them)\n", id, err)
					return
				}
				fmt.Fprintf(stdout, "user %d: %d queued message(s) cancelled.\n", id, n)
			}
		}
		if err := api.SetUserDisabledAs(ctx, goTrueAuth(cfg, pool), &cli, id, disable, cancelled); err != nil {
			fmt.Fprintf(stderr, "%s user %d: %v\n", args[1], id, err)
			return 1
		}
		// What a RUNNING server does, stated as what is true: this process
		// cannot reach its caches, so the server learns from the database.
		if !disable {
			fmt.Fprintf(stdout, "user %d enabled. A running server accepts them again within %s.\n",
				id, userCacheWindow)
			return 0
		}
		fmt.Fprintf(stdout, "user %d %sd. A running server ends that user's open streams (SSE, "+
			"terminals) and refuses their requests within %s if they have anything open (its disable "+
			"sweep), and refuses their next request within %s otherwise (its user cache). GoTrue already "+
			"refuses the user's refresh tokens.\n", id, args[1], api.DisableSweepInterval, userCacheWindow)
		return 0
	}
	return usage()
}

// claimLinkBase is the address the admin claim link is written on, and a note
// when it is not MUSTER_PUBLIC_URL as set. In gotrue mode only the browser
// listener serves a claim, so a MUSTER_PUBLIC_URL that is this machine's own
// loopback address on the MAIN port (a hub with no Tailscale, set to the
// install's http://localhost:<port>) would print a link that refuses the
// browser it is opened in (onboarding round 3): the link goes on the same
// host at the browser listener's port instead, and the note says to set
// MUSTER_PUBLIC_URL there. Any other address (https, another host, another
// port) is taken as the person routed it, and kept.
//
// It assumes the Compose stack's mapping, the same port number on the host as
// in the container (deploy/docker-compose.yml, and the override
// docs/guides/gotrue.md gives for the browser port).
func claimLinkBase(c config) (base, note string) {
	if api.AuthMode(c.AuthMode) != api.AuthModeGoTrue || c.BrowserPort == 0 || !loopback.HTTPURL(c.PublicURL) {
		return c.PublicURL, ""
	}
	u, err := url.Parse(strings.TrimSpace(c.PublicURL))
	if err != nil {
		return c.PublicURL, ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	if port != strconv.Itoa(c.Port) {
		return c.PublicURL, ""
	}
	u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(c.BrowserPort))
	base = strings.TrimRight(u.String(), "/")
	return base, fmt.Sprintf("%s=%s is the machine-token port, which refuses a browser in gotrue mode: this link is on "+
		"the browser listener (%s=%d) instead. Set %s=%s so every link muster writes is too",
		envPublicURL, c.PublicURL, envBrowserPort, c.BrowserPort, envPublicURL, base)
}

// userCacheWindow is api's sub→user cache lifetime, as the CLI states it.
const userCacheWindow = 30 * time.Second

// logAuthBanner states the auth mode, the GoTrue wiring and the browser
// listener, each in both directions.
func (a *app) logAuthBanner() {
	l, c := a.logger, a.cfg
	if api.AuthMode(c.AuthMode) == api.AuthModeGoTrue {
		l.Printf("auth mode: GOTRUE — %s=gotrue: browser sign-in is per user through muster's GoTrue "+
			"(cookies muster_at/muster_rt, __Host- prefixed on an https deployment, HttpOnly, SameSite=Lax, ALWAYS Secure), and only on the browser "+
			"listener", envAuthMode)
	} else {
		l.Printf("auth mode: PASSWORD — %s=password (the default): one shared operator password, acting "+
			"as user 1. Set %s=gotrue for per-user sign-in", envAuthMode, envAuthMode)
	}
	if c.goTrueConfigured() {
		l.Printf("gotrue: CONFIGURED — %s=%s, %s set (%d characters, never logged); tokens must be "+
			"HS256 from issuer %q for audience %q. The admin claim (`muster-server invite --claim-admin`) "+
			"is redeemable", envGoTrueURL, c.GoTrueURL, envGoTrueSecret, len(c.GoTrueSecret), gotrueIssuer, gotrueAudience)
	} else {
		l.Printf("gotrue: NOT CONFIGURED — %s and %s are unset, so there is no per-user sign-in and no "+
			"admin claim (password mode only)", envGoTrueURL, envGoTrueSecret)
	}
	if c.BrowserPort != 0 {
		l.Printf("browser listener: %s=%d — the only port gotrue mode serves a browser session, sign-in "+
			"or claim on. The main port serves machine tokens only in gotrue mode. The client address is "+
			"decided the same way on every port (%s)", envBrowserPort, c.BrowserPort, envTrustedProxies)
	} else {
		l.Printf("browser listener: NONE — %s is unset; every listener is the main one (gotrue mode "+
			"refuses to boot this way)", envBrowserPort)
	}
}
