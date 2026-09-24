package main

import (
	"bytes"
	"log"
	"os"
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
//
// 🔴 AND THE SUBSTRING VERSION OF THAT WAS SATISFIED BY THE BRANCH IT EXCLUDED.
// The configured case asserted `mustContain: {"CONFIGURED", url}` and
// `mustNotSay: {"is unset"}`. But "PARTIALLY CONFIGURED, THEREFORE NOT
// CONFIGURED" CONTAINS "CONFIGURED", and with all three variables set
// missingRouterCredential returns its case-0 sentence, which contains no "is
// unset" — so the partial branch satisfied every assertion of the configured
// case. Measured with an ordinary edit, not a sabotage: move the
// `case a.cfg.RouterURL != "":` arm above `case port.Configured():`, the sort of
// "group the router-URL cases together" tidy-up anybody might make, and a
// FULLY CONFIGURED router boots announcing PARTIALLY CONFIGURED, THEREFORE NOT
// CONFIGURED while the entire suite stays green. Same class as the defect this
// test was written for — the banner reporting the wrong router state — on the
// opposite axis.
//
// 🔴 SO THE WHOLE NORMALISED LINE IS PINNED. When the artifact under test IS
// PROSE, a guard on words is walkable by rewording, and a guard on a word that
// is a substring of its own negation is walkable by doing nothing at all. The
// cost is real and is accepted deliberately: a cosmetic reword of any line below
// fails this test and the wantLine has to be updated with it. That is the price
// of a machine-readable claim about a sentence.
//
// ⚠ mustContain/mustNotSay ARE KEPT ALONGSIDE IT, AND NOT AS BELT-AND-BRACES.
// A pinned line is walkable one way: regenerate the golden. Updating a wantLine
// is a reflex, and the reflex is exactly what a wrong line arrives as — so the
// negatives stay, because "the CONFIGURED line must not say PARTIALLY" is an
// INTENT that survives a reword and a careless re-pin alike.
func TestTheRouterBannerNamesTheVariableThatIsActuallyMissing(t *testing.T) {
	const url = "https://router.example.invalid"
	const partialTail = " A router client needs all three; router.New builds nothing without " +
		"them, so this server behaves exactly as if no router were configured and will NOT " +
		"report ready while a notes store is wired"
	cases := []struct {
		name        string
		cfg         config
		wantLine    string
		mustContain []string
		mustNotSay  []string
	}{
		{
			name: "all three set: configured",
			cfg:  config{RouterURL: url, RouterToken: "t", RouterActor: "muster"},
			wantLine: `permission router: CONFIGURED — ` + envRouterURL + `=` + url +
				`, actor "muster" — session liveness, event publish, notifications, the ` +
				`directory picker and the approval gate are live`,
			// It is the only branch that may call itself CONFIGURED.
			mustContain: []string{"CONFIGURED", url},
			// 🔴 "NOT CONFIGURED" AND "PARTIALLY" ARE THE MEASURED HOLE. Without
			// them this case is satisfied by the partial branch, whose sentence
			// contains "CONFIGURED" as a substring and, with every variable set,
			// says nothing "is unset" either.
			mustNotSay: []string{"is unset", "NOT CONFIGURED", "PARTIALLY"},
		},
		{
			// 🔴 THE MEASURED CASE: URL set, token forgotten.
			name: "url set, token missing",
			cfg:  config{RouterURL: url, RouterActor: "muster"},
			wantLine: `permission router: PARTIALLY CONFIGURED, THEREFORE NOT CONFIGURED — ` +
				envRouterURL + ` is set (` + url + `) but ` + envRouterToken + ` is unset.` +
				partialTail,
			mustContain: []string{envRouterToken, "is unset", url},
			// The old banner's exact wrong sentence. The URL is RIGHT here, and
			// telling the operator it is unset is the whole defect.
			mustNotSay: []string{envRouterURL + " is unset"},
		},
		{
			name: "url set, actor missing",
			cfg:  config{RouterURL: url, RouterToken: "t"},
			wantLine: `permission router: PARTIALLY CONFIGURED, THEREFORE NOT CONFIGURED — ` +
				envRouterURL + ` is set (` + url + `) but ` + envRouterActor + ` is unset.` +
				partialTail,
			mustContain: []string{envRouterActor, "is unset"},
			mustNotSay:  []string{envRouterURL + " is unset", envRouterToken + " is unset"},
		},
		{
			name: "url set, both credentials missing",
			cfg:  config{RouterURL: url},
			wantLine: `permission router: PARTIALLY CONFIGURED, THEREFORE NOT CONFIGURED — ` +
				envRouterURL + ` is set (` + url + `) but ` + envRouterToken + ` and ` +
				envRouterActor + ` are unset.` + partialTail,
			mustContain: []string{envRouterToken, envRouterActor, "are unset"},
			mustNotSay:  []string{envRouterURL + " is unset"},
		},
		{
			name: "nothing set, standalone declared",
			cfg:  config{Standalone: true},
			wantLine: `permission router: NONE, DECLARED (` + envStandalone + `=1) — session ` +
				`transcript links will all read "no transcript recorded", which is TRUE here ` +
				`because nothing records transcripts. Notifications, the directory picker ` +
				`and agent checkpoints degrade and say so where a human can see it`,
			mustContain: []string{envStandalone},
			mustNotSay:  []string{envRouterToken + " is unset", "CONFIGURED"},
		},
		{
			// The genuinely-unset case must keep naming the URL, or fixing the
			// finding above would just move the wrong answer to a different input.
			name: "nothing set at all",
			cfg:  config{},
			wantLine: `permission router: NONE, UNDECLARED — ` + envRouterURL + ` is unset ` +
				`and ` + envStandalone + ` is not 1. This server will NOT report ready while ` +
				`a notes store is wired, on purpose: see /readyz for the reason`,
			mustContain: []string{envRouterURL, "UNDECLARED"},
			mustNotSay:  []string{envRouterToken + " is unset"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := routerLine(t, renderBanner(t, tc.cfg, api.Extensions{}))

			// 🔴 THE WHOLE LINE, NORMALISED ON WHITESPACE ONLY. Folding runs of
			// spaces is what lets the Printf keep its source wrapping; nothing
			// else is folded, so a changed word is a changed line.
			if got, want := normaliseBannerLine(line), normaliseBannerLine(tc.wantLine); got != want {
				t.Errorf("the router banner line is not the sentence this case pins.\n"+
					"  got:  %s\n  want: %s\n"+
					"    This line is READ BY AN OPERATOR MID-INCIDENT and its previous two "+
					"revisions each stated something false — first blaming the wrong "+
					"variable, then calling a fully configured router NOT CONFIGURED. If "+
					"you changed the wording on purpose, update wantLine; if you did not, "+
					"a branch is being selected that this configuration should not reach.",
					got, want)
			}

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

// normaliseBannerLine folds runs of whitespace so a pinned line can be written
// across several source lines. It folds NOTHING else — not case, not
// punctuation — because every one of those carries meaning in this banner.
func normaliseBannerLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// bannerLedger is every environment variable logBanner is responsible for
// announcing, in BOTH directions.
//
// 🔴 THIS IS THE GUARD WHOSE ABSENCE LET MUSTER_SECURE_COOKIES BE OMITTED.
// logBanner's header states a RELATIONSHIP — "each line below is printed whether
// the thing is on or off" — and nothing checked it, so a variable could be added
// to config.go with no banner line and every test stayed green. A per-variable
// spot check would have had the same hole for the next variable.
//
// 🔴 AND FOR A WHOLE REVISION THAT LAST SENTENCE READ "this is the relationship,
// asserted", WHICH WAS FALSE. This list is hand-written, so what was asserted
// was "every LEDGERED variable is announced" — not "every variable logBanner is
// responsible for". The omission it exists to prevent therefore still fitted
// through it exactly as MUSTER_SECURE_COOKIES had: measured by adding an
// envNewThing constant to config.go, wiring it once so the spelled-once-AND-READ
// gate was satisfied, and giving it no banner line and no entry here — the full
// suite stayed green.
//
// 🔴 THE CANDIDATE SET IS DERIVED NOW. TestEveryDeclaredEnvVariableIsLedgeredOrExempt
// reads config.go's const block — the same extraction
// TestEveryServerEnvNameIsSpelledOnceAndRead uses — and requires every constant
// in it to be on THIS list or on bannerExempt with a stated reason. A new
// variable is on neither, so it fails until somebody decides which it is.
//
// ⚠ IT IS STILL NOT "EVERY CONSTANT IN config.go", and that honesty is kept
// rather than papered over: see bannerExempt, which now has to NAME each
// exclusion instead of gesturing at a parenthesised handful in prose.
var bannerLedger = []string{
	envDatabase,
	envUIPassword,
	envSecureCookies,
	envHookToken,
	envServiceToken,
	envRouterURL,
	envGitHubEncKey,
}

// bannerExempt is every environment variable config.go declares that logBanner
// is deliberately NOT responsible for announcing in both directions, and why.
//
// 🔴 A REASON IS REQUIRED, AND THE SPLIT IS MACHINE-CHECKED RATHER THAN TRUSTED.
// An exception list is only better than no guard if it cannot be used as a
// silencer, so TestEveryDeclaredEnvVariableIsLedgeredOrExempt refuses an entry
// here whose variable IS named in the banner in both directions: that is the
// definition of a ledgered variable, and the entry would be hiding a line the
// ledger's both-directions and lines-must-differ checks would otherwise hold to
// account. Adding a banner line for one of these is therefore not a silent
// change — it fails here until the variable is moved onto bannerLedger.
//
// ⚠ EACH REASON BELOW WAS READ OFF THE CODE, NOT INFERRED FROM THE GROUPING.
// The prose this replaced claimed MUSTER_PUBLIC_URL and GITHUB_CLIENT_SECRET
// were "reported through a different name"; githubAuthPosture names
// GITHUB_CLIENT_ID and MUSTER_GITHUB_TOKEN and neither of the other two, so the
// entries say what is actually true of them.
var bannerExempt = map[string]string{
	envPort: "the listen line reports the BOUND ADDRESS (main.go: \"muster %s listening on %s\" " +
		"with ln.Addr()), which is the answer this variable is asked for and is strictly " +
		"better than echoing the request — it is printed after the banner, by serve, not by " +
		"logBanner",
	envGitHubToken: "named by githubAuthPosture on the github-connection line whenever a " +
		"credential exists; there is no off-line for it because the no-credential branch " +
		"names it too, but only when MUSTER_GITHUB_ENCRYPTION_KEY is set at all — the " +
		"encryption key is the ledgered variable that gates the whole surface",
	envGitHubClientID: "named by githubAuthPosture as \"the OAuth App\" on the " +
		"github-connection line; same one-directional shape as MUSTER_GITHUB_TOKEN and for " +
		"the same reason",
	envGitHubClientSec: "the OAuth App is reported by its CLIENT ID, which is the half that " +
		"is safe to print and the half an operator checks. A secret's NAME in a log tells " +
		"the reader nothing its sibling has not already told them",
	envPublicURL: "the OAuth callback base URL — an input to the same GitHubOAuthConfig as " +
		"the client id and secret (main.go), and reported through the same posture line",
	envFaroURL: "browser RUM: handed to ui.SetFaroConfig for the page agent, not a property " +
		"of this server's auth or dependency posture. logBanner has no line for it on purpose",
	envFaroKey: "browser RUM: the collect-endpoint key, handed to ui.SetFaroConfig with the " +
		"URL and reaching the page rather than this server. Same reason as MUSTER_FARO_URL, " +
		"and it is client-public by design so its absence breaks no server posture",
	envFaroTracing: "browser RUM: a tracing toggle on the page agent, handed to " +
		"ui.SetFaroConfig with the URL and the key. It changes what the BROWSER emits, and " +
		"nothing about what this process will or will not do at boot",
	envTagAutoDispatch: "a feature flag on the task surface (api.Extensions.TagAutoDispatch), " +
		"not a refusal or a dependency. Nothing about the boot posture changes with it, so " +
		"a banner line would be noise",
	envTaskReapAfter: "the reaper announces its own state with the VALUE, at the point it " +
		"starts (internal/api/server.go: \"task reaper: sweeping every %s for tasks idle for " +
		">%s\"), which is where a reader looking for it will be",
	envRouterToken: "reported by the permission-router line, which names whichever credential " +
		"is MISSING (missingRouterCredential). A fully configured deployment deliberately " +
		"says nothing about it, so it cannot satisfy the both-directions rule — " +
		"TestTheRouterBannerNamesTheVariableThatIsActuallyMissing is what holds it",
	envRouterActor: "reported by the permission-router line; see MUSTER_ROUTER_TOKEN",
	envStandalone: "reported by the permission-router line's two NONE branches (DECLARED and " +
		"UNDECLARED). When a router IS configured the line has nothing to say about it, " +
		"which is the one-directional shape the ledger cannot express",
}

// bannerStubGitHubStore stands in for a wired GitHub store. Only its presence
// is read by logBanner (`ext.GitHub != nil`), never any method, so embedding the
// interface is enough and cannot accidentally be CALLED without panicking
// loudly — which is the behaviour wanted if the banner ever starts reading it.
type bannerStubGitHubStore struct{ github.Store }

// TestTheBannerAnnouncesEveryLedgeredVariableInBothDirections is the F6 guard.
// bannerBothDirections renders the banner twice from configurations that are
// deliberately opposites on every ledgered axis.
//
// 🔴 ONE FIXTURE, TWO READERS. The ledger test below and the exemption test
// after it both need "is this variable named when it is on / when it is off",
// and two copies of the fixture is how the two tests come to disagree about
// what ON means — at which point a variable can be ledgered against one
// definition and exempted against another, and the split they jointly enforce
// stops meaning anything. A single "fully on" fixture would also pass for a
// variable whose off-branch prints nothing, which is exactly the defect.
func bannerBothDirections(t *testing.T) (onOut, offOut string) {
	t.Helper()
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
	onOut = renderBannerWithPool(t, on, api.Extensions{GitHub: bannerStubGitHubStore{}}, &pgxpool.Pool{})
	offOut = renderBannerWithPool(t, config{}, api.Extensions{}, nil)
	return onOut, offOut
}

func TestTheBannerAnnouncesEveryLedgeredVariableInBothDirections(t *testing.T) {
	onOut, offOut := bannerBothDirections(t)

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

// TestEveryDeclaredEnvVariableIsLedgeredOrExempt is what makes bannerLedger's
// header sentence true.
//
// 🔴 THE LEDGER IS A CLAIM ABOUT A SET IT DOES NOT DEFINE. Its own comment said
// "this is the relationship, asserted", and what was asserted was the
// relationship restricted to the list itself — every ledgered variable is
// announced, which says nothing at all about a variable nobody ledgered. That is
// the identical shape as MUSTER_SECURE_COOKIES' omission, one level up: the
// thing that would have caught it was never the spot check, it was somebody
// noticing. Measured: an envNewThing constant added to config.go and referenced
// once — enough for TestEveryServerEnvNameIsSpelledOnceAndRead's "AND READ"
// direction — with no banner line and no ledger entry left the full suite green.
//
// 🔴 SO THE CANDIDATES COME FROM THE CONST BLOCK, VIA THE SAME EXTRACTION THE
// SPELLED-ONCE GATE USES. envConstDecl is shared deliberately: two regexes for
// one declaration shape is two things to keep in step, and a variable invisible
// to one of them would be invisible to whichever gate owned it.
func TestEveryDeclaredEnvVariableIsLedgeredOrExempt(t *testing.T) {
	declared := declaredEnvConstants(t)

	// 🔴 POSITIVE CONTROL, AS A NUMBER. An empty extraction satisfies "every
	// declared variable is ledgered or exempt" perfectly, and reads identically
	// to a clean tree. Same threshold and same MUSTER_PORT probe as the
	// spelled-once gate, for the same reason.
	if len(declared) < 15 {
		t.Fatalf("positive control FAILED: only %d env constant(s) were extracted from "+
			"config.go's const block, and this binary declares ~20. The declaration "+
			"pattern has stopped matching, so the verdict below is over a truncated set.",
			len(declared))
	}
	if _, ok := declared[envPort]; !ok {
		t.Fatalf("positive control FAILED: %s is declared in config.go and the extraction "+
			"did not find it (%d found). The instrument is not measuring.",
			envPort, len(declared))
	}

	ledgered := map[string]bool{}
	for _, name := range bannerLedger {
		ledgered[name] = true
	}

	// --- every declared variable is one or the other --------------------------
	for name, ident := range declared {
		_, exempt := bannerExempt[name]
		switch {
		case ledgered[name] && exempt:
			t.Errorf("%s (%s) is on BOTH bannerLedger and bannerExempt. They are the two "+
				"answers to one question — does logBanner announce this in both "+
				"directions — and a variable on both makes the exemption unreadable: a "+
				"later reader cannot tell which one is the live decision.", name, ident)
		case !ledgered[name] && !exempt:
			t.Errorf("%s (%s) is declared in config.go and is on NEITHER bannerLedger nor "+
				"bannerExempt.\n"+
				"    logBanner's header promises every thing it is responsible for gets a "+
				"line whether it is on or off, because a silent healthy state and a silent "+
				"refusing state are byte-identical in the log. MUSTER_SECURE_COOKIES was "+
				"omitted exactly this way and shipped a session cookie with no Secure "+
				"attribute, unannounced.\n"+
				"    Decide, do not delete this test: give it a banner line in BOTH "+
				"directions and add it to bannerLedger, or add it to bannerExempt with the "+
				"reason it is not this banner's business.", name, ident)
		}
	}

	// --- neither list may name a variable that is not declared ---------------
	for _, name := range bannerLedger {
		if _, ok := declared[name]; !ok {
			t.Errorf("bannerLedger names %q, which config.go's const block does not declare. "+
				"A ledger entry for a variable that no longer exists is a line of coverage "+
				"that can never fail.", name)
		}
	}
	for name, reason := range bannerExempt {
		if _, ok := declared[name]; !ok {
			t.Errorf("bannerExempt names %q, which config.go's const block does not declare. "+
				"A stale exemption silences nothing and misleads the next reader about what "+
				"the banner covers.", name)
		}
		// 🔴 THE REASON IS THE ENTIRE VALUE OF AN EXCEPTION LIST. An empty or
		// token one turns it into an allowlist anybody can append to, which is
		// the mechanism this test exists to remove rather than relocate.
		if len(strings.TrimSpace(reason)) < 40 {
			t.Errorf("bannerExempt[%q] has no real reason (%q). Say what reports this "+
				"variable's state instead, or that nothing does and why that is correct.",
				name, reason)
		}
	}

	// --- an exemption may not hide a variable the banner DOES report ---------
	//
	// 🔴 THIS IS WHAT STOPS bannerExempt BEING A SILENCER. Named in both
	// directions is the definition of a ledgered variable; an entry here for one
	// of those would move it out from under the both-directions and
	// lines-must-differ checks while looking like documentation.
	onOut, offOut := bannerBothDirections(t)
	if onOut == offOut {
		t.Fatal("positive control FAILED: the ON and OFF banners are byte-identical, so " +
			"the both-directions reads below are one configuration twice")
	}
	for name := range bannerExempt {
		if lineNaming(onOut, name) != "" && lineNaming(offOut, name) != "" {
			t.Errorf("%s is on bannerExempt, but the banner names it in BOTH directions:\n"+
				"    on:  %s\n    off: %s\n"+
				"    That is precisely what a LEDGERED variable is. Move it to "+
				"bannerLedger, where the two lines are also required to DIFFER — an "+
				"exemption would leave it announced but unchecked, which reads as coverage "+
				"and provides none.", name, lineNaming(onOut, name), lineNaming(offOut, name))
		}
	}

	t.Logf("%d declared env constant(s): %d ledgered, %d exempt",
		len(declared), len(bannerLedger), len(bannerExempt))
}

// declaredEnvConstants reads config.go's const block and returns env name ->
// identifier, using the SAME declaration pattern as the spelled-once gate.
func declaredEnvConstants(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if m := envConstDecl.FindStringSubmatch(line); m != nil {
			out[m[2]] = m[1]
		}
	}
	return out
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
