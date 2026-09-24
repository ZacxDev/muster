package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/router"
)

// ---------------------------------------------------------------------------
// THE BOOT BANNER IS A CLAIM, AND TWO OF ITS LINES WERE MEASURED WRONG.
//
// logBanner's own header promises "Each line below is printed whether the thing
// is on or off", because "a silent healthy state and a silent refusing state
// are otherwise byte-identical in the log". Two findings against that promise:
//
//  1. MUSTER_SECURE_COOKIES had NO line in either direction. It defaults FALSE,
//     so a TLS deployment that forgets it hands out a session cookie with no
//     Secure attribute and the banner says nothing at all.
//  2. The router line BLAMED THE WRONG VARIABLE. router.New returns nil when
//     base, token OR actor is empty, so `port.Configured()` is false for all
//     three — and the fallback branch reported "MUSTER_ROUTER_URL is unset" at
//     an operator who had set the URL and forgotten the token. Measured live,
//     with the URL set.
//
// 🔴 THE SECOND IS WORSE THAN A MISSING LINE, WHICH IS WHY IT GETS THE STRICTER
// ASSERTION. A missing line leaves the operator where they were; a line naming
// the wrong variable sends them to re-check the one that was already right.
// ---------------------------------------------------------------------------

// renderBanner runs logBanner against cfg with NO database pool.
func renderBanner(t *testing.T, cfg config, ext api.Extensions) string {
	t.Helper()
	return renderBannerWithPool(t, cfg, ext, nil)
}

// renderBannerWithPool runs logBanner and returns what it printed.
//
// 🔴 THE POOL IS A SEPARATE PARAMETER BECAUSE THE PERSISTENCE LINE BRANCHES ON
// IT, NOT ON cfg.Database — AND A FIXTURE THAT SET ONLY THE CONFIG LEFT THAT
// AXIS PINNED. Both renders took the "persistence: NONE" arm, so the
// both-directions check was comparing one branch against itself and would have
// passed however that line was written. Found by the differ-check below, which
// is the whole argument for it. A zero-value *pgxpool.Pool is enough: logBanner
// reads `a.pool != nil` and calls no method on it.
func renderBannerWithPool(t *testing.T, cfg config, ext api.Extensions, pool *pgxpool.Pool) string {
	t.Helper()
	var buf bytes.Buffer
	a := &app{cfg: cfg, logger: log.New(&buf, "", 0), pool: pool}
	port := router.NewPort(router.Config{
		BaseURL: cfg.RouterURL, Token: cfg.RouterToken, Actor: cfg.RouterActor,
	})
	a.logBanner(ext, port)
	out := buf.String()
	if strings.TrimSpace(out) == "" {
		t.Fatal("instrument check FAILED: logBanner printed NOTHING, so every assertion " +
			"below would be about an empty string")
	}
	return out
}

// routerLine returns the banner's "permission router:" line.
func routerLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "permission router:") {
			return line
		}
	}
	t.Fatalf("no \"permission router:\" line in the banner at all — logBanner's header "+
		"promises one in every direction.\nbanner:\n%s", out)
	return ""
}

// TestTheRouterBannerNamesTheVariableThatIsActuallyMissing is the F3 guard.
//
// 🔴 IT ASSERTS THE NEGATIVE TOO, AND THAT IS THE HALF THAT MATTERS. A check
// that the line MENTIONS MUSTER_ROUTER_TOKEN would pass against the old
// two-branch banner the moment anyone added the word anywhere; what was wrong
// was that it ALSO said the URL was unset while the URL was set. So each case
// pins both what must be named and what must not be claimed.
func TestTheRouterBannerNamesTheVariableThatIsActuallyMissing(t *testing.T) {
	const url = "https://router.example.invalid"
	cases := []struct {
		name        string
		cfg         config
		mustContain []string
		mustNotSay  []string
	}{
		{
			name: "all three set: configured",
			cfg:  config{RouterURL: url, RouterToken: "t", RouterActor: "muster"},
			// It is the only branch that may call itself CONFIGURED.
			mustContain: []string{"CONFIGURED", url},
			mustNotSay:  []string{"is unset"},
		},
		{
			// 🔴 THE MEASURED CASE: URL set, token forgotten.
			name:        "url set, token missing",
			cfg:         config{RouterURL: url, RouterActor: "muster"},
			mustContain: []string{envRouterToken, "is unset", url},
			// The old banner's exact wrong sentence. The URL is RIGHT here, and
			// telling the operator it is unset is the whole defect.
			mustNotSay: []string{envRouterURL + " is unset"},
		},
		{
			name:        "url set, actor missing",
			cfg:         config{RouterURL: url, RouterToken: "t"},
			mustContain: []string{envRouterActor, "is unset"},
			mustNotSay:  []string{envRouterURL + " is unset", envRouterToken + " is unset"},
		},
		{
			name:        "url set, both credentials missing",
			cfg:         config{RouterURL: url},
			mustContain: []string{envRouterToken, envRouterActor, "are unset"},
			mustNotSay:  []string{envRouterURL + " is unset"},
		},
		{
			name:        "nothing set, standalone declared",
			cfg:         config{Standalone: true},
			mustContain: []string{envStandalone},
			mustNotSay:  []string{envRouterToken + " is unset"},
		},
		{
			// The genuinely-unset case must keep naming the URL, or fixing the
			// finding above would just move the wrong answer to a different input.
			name:        "nothing set at all",
			cfg:         config{},
			mustContain: []string{envRouterURL, "UNDECLARED"},
			mustNotSay:  []string{envRouterToken + " is unset"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := routerLine(t, renderBanner(t, tc.cfg, api.Extensions{}))
			for _, want := range tc.mustContain {
				if !strings.Contains(line, want) {
					t.Errorf("the router banner line does not contain %q, so an operator "+
						"cannot act on it.\n  line: %s", want, line)
				}
			}
			for _, forbidden := range tc.mustNotSay {
				if strings.Contains(line, forbidden) {
					t.Errorf("the router banner line SAYS %q, which is false for this "+
						"configuration. A line naming the wrong variable sends the reader "+
						"to re-check the one that was already right — worse than silence.\n"+
						"  line: %s", forbidden, line)
				}
			}
		})
	}
}

// bannerLedger is every environment variable logBanner is responsible for
// announcing, in BOTH directions.
//
// 🔴 THIS IS THE GUARD WHOSE ABSENCE LET MUSTER_SECURE_COOKIES BE OMITTED.
// logBanner's header states a RELATIONSHIP — "each line below is printed whether
// the thing is on or off" — and nothing checked it, so a variable could be added
// to config.go with no banner line and every test stayed green. A per-variable
// spot check would have had the same hole for the next variable; this is the
// relationship, asserted.
//
// ⚠ IT IS NOT "EVERY CONSTANT IN config.go". Several are inputs to a posture
// this banner reports through a different name (MUSTER_PUBLIC_URL,
// GITHUB_CLIENT_SECRET, the Faro keys, MUSTER_PORT — which the listen line
// reports as a bound address, and MUSTER_TASK_REAP_AFTER, which the reaper
// announces itself). Listing those would force noise lines; the ledger is the
// set whose STATE an operator cannot otherwise discover from the log.
var bannerLedger = []string{
	envDatabase,
	envUIPassword,
	envSecureCookies,
	envHookToken,
	envServiceToken,
	envRouterURL,
	envGitHubEncKey,
}

// bannerStubGitHubStore stands in for a wired GitHub store. Only its presence
// is read by logBanner (`ext.GitHub != nil`), never any method, so embedding the
// interface is enough and cannot accidentally be CALLED without panicking
// loudly — which is the behaviour wanted if the banner ever starts reading it.
type bannerStubGitHubStore struct{ github.Store }

// TestTheBannerAnnouncesEveryLedgeredVariableInBothDirections is the F6 guard.
func TestTheBannerAnnouncesEveryLedgeredVariableInBothDirections(t *testing.T) {
	// The two configurations are deliberately opposites on every ledgered axis,
	// so a name that appears in only ONE direction is caught. A single "fully
	// on" fixture would pass for a variable whose off-branch prints nothing,
	// which is exactly the defect.
	on := config{
		Database:      "postgres://x/y",
		UIPassword:    strings.Repeat("p", 40),
		SecureCookies: true,
		HookToken:     "hook",
		ServiceToken:  strings.Repeat("s", 40),
		RouterURL:     "https://router.example.invalid",
		RouterToken:   "t",
		RouterActor:   "muster",
	}
	off := config{}

	onOut := renderBannerWithPool(t, on, api.Extensions{GitHub: bannerStubGitHubStore{}}, &pgxpool.Pool{})
	offOut := renderBannerWithPool(t, off, api.Extensions{}, nil)

	// 🔴 POSITIVE CONTROL: THE TWO RENDERS MUST DIFFER. If the fixture failed to
	// flip anything — a field renamed, a zero value that is also the "on" value —
	// both strings would be identical and every assertion below would be about
	// one configuration twice.
	if onOut == offOut {
		t.Fatal("positive control FAILED: the ON and OFF banners are byte-identical, so " +
			"the fixture is not exercising both directions and a one-direction variable " +
			"would pass unnoticed")
	}

	for _, name := range bannerLedger {
		t.Run(name, func(t *testing.T) {
			onLine := lineNaming(onOut, name)
			offLine := lineNaming(offOut, name)

			if onLine == "" {
				t.Errorf("%s is not named in the banner when it IS set.\nbanner:\n%s", name, onOut)
			}
			if offLine == "" {
				t.Errorf("%s is not named in the banner when it is NOT set.\n"+
					"    logBanner's header promises every line is printed in both "+
					"directions, because a silent healthy state and a silent refusing "+
					"state are byte-identical in the log. %s defaults to off, so the "+
					"OFF line is the only one an operator will ever see.\nbanner:\n%s",
					name, name, offOut)
			}

			// 🔴 THE TWO LINES MUST ALSO DIFFER, AND THIS HALF WAS ADDED BECAUSE
			// THE MUTATION SWEEP WALKED THE FIRST ONE. A mutant that forced the
			// secure-cookie branch never to be taken (`if a.cfg.SecureCookies &&
			// false`) SURVIVED: the else arm still printed the variable's name, so
			// "named in both directions" held while the banner reported the wrong
			// state in one of them. Naming a variable is not reporting it — the
			// whole point is that an operator can read the STATE off the log, and
			// two identical lines carry no state at all.
			if onLine != "" && offLine != "" && onLine == offLine {
				t.Errorf("%s produces the SAME banner line whether it is set or not:\n"+
					"    %s\n"+
					"    The name is there, so a contains-check passes, but the line "+
					"reports no state: an operator reading the log cannot tell which "+
					"way this deployment is configured, which is the exact "+
					"indistinguishability logBanner's header exists to remove. The "+
					"branch is either dead or it prints the same sentence on both "+
					"arms.", name, onLine)
			}
		})
	}
}

// lineNaming returns the first banner line containing name, or "".
func lineNaming(banner, name string) string {
	for _, line := range strings.Split(banner, "\n") {
		if strings.Contains(line, name) {
			return line
		}
	}
	return ""
}

// TestTheProvisioningSeamLineDoesNotClaimAnUnnamedRefusal pins the corrected
// agent-provisioning line.
//
// 🔴 THE OLD LINE SAID THE ROUTES "REFUSE" AND NINE OF THEM ANSWERED 200. A
// claim in a boot banner is the strongest documentation this service emits —
// it is what an operator reads while debugging — so it is worth pinning to a
// symbol a reader can go and check rather than to an adjective. The line now
// cites api.requireProvisioner and the discriminator field, both of which are
// real and both of which a grep resolves.
func TestTheProvisioningSeamLineDoesNotClaimAnUnnamedRefusal(t *testing.T) {
	out := renderBanner(t, config{}, api.Extensions{})
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "agent provisioning:") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("the banner no longer announces the provisioning seam at all.\n%s", out)
	}
	for _, want := range []string{"requireProvisioner", api.ProvisionerUnwiredField, "503"} {
		if !strings.Contains(line, want) {
			t.Errorf("the agent-provisioning banner line does not name %q.\n"+
				"    This line asserted for the whole of the previous revision that every "+
				"agent-control route \"refuses at request time\", while nine of them "+
				"answered 200 and did nothing. The claim is only checkable if it names "+
				"the mechanism, so do not soften it back to an unqualified \"refuses\".\n"+
				"  line: %s", want, line)
		}
	}
}
