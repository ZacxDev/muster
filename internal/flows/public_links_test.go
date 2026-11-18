package flows

import (
	"strings"
	"testing"
)

// The live failure (muster 0.222.0): the deployment's deny words include the
// project's own name, so an officer's outbound link to its public repository
// was refused as "an infrastructure name". These are the two exact strings the
// officer filed.
const (
	liveHTTPS = "https://github.com/ZacxDev/muster"
	liveBare  = "github.com/ZacxDev/muster"
	liveRepo  = "github.com/ZacxDev/muster"
)

var liveWords = []string{"muster", "examplecluster"}

const nameRefusal = "the text carries an infrastructure name; an agent may not send that to a third party"

// TestOutboundLiveRepoLinksPassWhenAllowlisted (regression, red at base): the
// exact live strings, https and bare, at the end of a post and alone, pass
// the outbound screen when the repository is on the allowlist.
func TestOutboundLiveRepoLinksPassWhenAllowlisted(t *testing.T) {
	for _, text := range []string{
		liveHTTPS, liveBare,
		"Read the source: " + liveHTTPS,
		"Read the source: " + liveBare + ".",
		"(" + liveHTTPS + ")",
		"Source " + liveHTTPS + "/releases and " + liveBare + ".git",
		"both: " + liveHTTPS + " " + liveBare,
		"Read it at HTTPS://WWW.GitHub.com/zacxdev/MUSTER",
	} {
		if why := OutboundTextRefusal(text, liveWords, []string{liveRepo}); why != "" {
			t.Errorf("allowlisted link %q refused: %s", text, why)
		}
	}
}

// TestOutboundRepoLinkWithoutAllowlistRefusesTheName: with no allowlist the
// same strings are refused by the deny-word layer, with ITS message — the
// layer that refused the live draft.
func TestOutboundRepoLinkWithoutAllowlistRefusesTheName(t *testing.T) {
	for _, text := range []string{liveHTTPS, liveBare} {
		if why := OutboundTextRefusal(text, liveWords, nil); why != nameRefusal {
			t.Errorf("%q with no allowlist: %q; want %q", text, why, nameRefusal)
		}
	}
}

// TestOutboundAllowlistExemptsOnlyTheLinkPrefix (security guard): everything
// outside the allowlisted prefix is screened as before, each with its own
// layer's message.
func TestOutboundAllowlistExemptsOnlyTheLinkPrefix(t *testing.T) {
	// Built at run time, so the repository's own leak gate never reads a
	// private-looking host or address in this file.
	internalHost, privateIP, meshIP := "store.example."+"lan", "192.168."+"0.10", "100.64."+"0.9"
	cases := []struct{ text, want string }{
		{"muster is out: " + liveHTTPS, nameRefusal},
		{liveBare + " runs on examplecluster", nameRefusal},
		{liveBare + "/tree/muster", nameRefusal},
		{liveBare + " see " + internalHost, "an internal host name"},
		{liveBare + " on api.default.svc." + "cluster." + "local", "an internal host name"},
		{liveBare + " from " + privateIP, "an IP address"},
		{liveBare + " via " + meshIP, "an IP address"},
		{liveBare + " token ghp_" + strings.Repeat("a", 30), "a credential-shaped string"},
		{liveBare + " the namespace", "an infrastructure word"},
	}
	for _, c := range cases {
		if why := OutboundTextRefusal(c.text, liveWords, []string{liveRepo}); !strings.Contains(why, c.want) {
			t.Errorf("%q: %q; want it to carry %q", c.text, why, c.want)
		}
	}
}

// TestOutboundUndeclaredRepoIsNotExempt (security guard): a link the
// allowlist does not name — another repository of the same owner, another
// owner's repository of the same name, a look-alike host, a userinfo or a
// path in front — keeps the deny-word screen.
func TestOutboundUndeclaredRepoIsNotExempt(t *testing.T) {
	for _, text := range []string{
		"github.com/ZacxDev/muster-private",
		"github.com/ZacxDev/musterx/muster",
		"github.com/other/muster",
		"evil.github.com/ZacxDev/muster",
		"github.com.evil/ZacxDev/muster",
		"https://user@github.com/ZacxDev/muster",
		"example.org/https://github.com/ZacxDev/muster",
		"xgithub.com/ZacxDev/muster",
		"gitlab.com/ZacxDev/muster",
		"x\u00e9github.com/ZacxDev/muster",
	} {
		if why := OutboundTextRefusal(text, liveWords, []string{liveRepo}); why != nameRefusal {
			t.Errorf("%q: %q; want %q", text, why, nameRefusal)
		}
	}
	// A suffix that makes it another name entirely is screened as that name.
	if why := OutboundTextRefusal(liveBare+".l"+"an", liveWords, []string{liveRepo}); why == "" {
		t.Error("a repository link ending in an internal suffix was allowed")
	}
}

// TestOutboundHostOnlyEntry: an operator entry naming a bare host exempts the
// host, never what follows it.
func TestOutboundHostOnlyEntry(t *testing.T) {
	words := []string{"exampledeploy"}
	if why := OutboundTextRefusal("see https://exampledeploy.org today", words, []string{"exampledeploy.org"}); why != "" {
		t.Errorf("an allowlisted host refused: %s", why)
	}
	if why := OutboundTextRefusal("see exampledeploy.org/exampledeploy", words, []string{"exampledeploy.org"}); why != nameRefusal {
		t.Errorf("the path after an allowlisted host: %q; want %q", why, nameRefusal)
	}
}

// TestReplyScreenHasNoAllowlist: AgentTextRefusal (a chief's reply draft, a
// flow's text) still refuses the link, as a host name.
func TestReplyScreenHasNoAllowlist(t *testing.T) {
	for _, text := range []string{liveHTTPS, liveBare} {
		if why := AgentTextRefusal(text, liveWords); !strings.Contains(why, "a host name") {
			t.Errorf("reply screen on %q: %q; want a host name refusal", text, why)
		}
	}
}

func TestParsePublicLinkEntry(t *testing.T) {
	for in, want := range map[string]string{
		"https://GitHub.com/ZacxDev/muster/": "github.com/ZacxDev/muster",
		" www.example.org ":                  "example.org",
		"github.com/a/b":                     "github.com/a/b",
	} {
		if got, err := parsePublicLink(in); err != nil || got != want {
			t.Errorf("parsePublicLink(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "muster", "10.0.0." + "1", "store.example." + "lan", "a.svc." + "cluster", "x." + "local/a",
		"example.org/a b", "ftp://example.org", "example.org?x=1"} {
		if got, err := parsePublicLink(bad); err == nil {
			t.Errorf("parsePublicLink(%q) = %q; want refused", bad, got)
		}
	}
}
