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
		// "muster" itself is the declared repository's name, exempt anywhere
		// (TestOutboundBarePublicRepoName); another deployment name is not.
		{"examplecluster is out: " + liveHTTPS, nameRefusal},
		{liveBare + " runs on examplecluster", nameRefusal},
		{liveBare + "/tree/examplecluster", nameRefusal},
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
// owner, a look-alike host, a userinfo or a path in front — keeps the
// deny-word screen. The deny word here is the OWNER: the declared
// repository's name is exempt anywhere (TestOutboundBarePublicRepoName), so
// only a word that is not can show whether the link prefix was stripped.
func TestOutboundUndeclaredRepoIsNotExempt(t *testing.T) {
	ownerWords := []string{"zacxdev", "muster"}
	if why := OutboundTextRefusal(liveBare, ownerWords, []string{liveRepo}); why != "" {
		t.Fatalf("control: the declared link with the owner denied was refused: %s", why)
	}
	for _, text := range []string{
		"github.com/ZacxDev/muster-private",
		"github.com/ZacxDev/musterx/muster",
		"github.com/ZacxDev-other/muster",
		"evil.github.com/ZacxDev/muster",
		"github.com.evil/ZacxDev/muster",
		"https://user@github.com/ZacxDev/muster",
		"example.org/https://github.com/ZacxDev/muster",
		"xgithub.com/ZacxDev/muster",
		"gitlab.com/ZacxDev/muster",
		"x\u00e9github.com/ZacxDev/muster",
	} {
		if why := OutboundTextRefusal(text, ownerWords, []string{liveRepo}); why != nameRefusal {
			t.Errorf("%q: %q; want %q", text, why, nameRefusal)
		}
	}
	// A suffix that makes it another name entirely is screened as that name.
	if why := OutboundTextRefusal(liveBare+".l"+"an", liveWords, []string{liveRepo}); why == "" {
		t.Error("a repository link ending in an internal suffix was allowed")
	}
}

// A made-up deployment for the bare-name exemption: its public project is
// `example.org/acme/widget`, and "widget" is also one of its deny words.
const widgetRepo = "example.org/acme/widget"

var widgetWords = []string{"Widget", "acme", "examplecluster", "widgetstore"}

// TestOutboundBarePublicRepoName (regression, red at base): an outbound draft
// may name the declared public repository bare, though its name is a deny
// word — case-insensitively, the entry in any accepted spelling.
func TestOutboundBarePublicRepoName(t *testing.T) {
	for _, entry := range []string{widgetRepo, "https://Example.org/acme/Widget/", "example.org/acme/widget.git"} {
		for _, text := range []string{
			"widget has a new logo",
			"WIDGET has a new logo!",
			"The new Widget release.",
		} {
			if why := OutboundTextRefusal(text, widgetWords, []string{entry}); why != "" {
				t.Errorf("entry %q, %q refused: %s", entry, text, why)
			}
		}
	}
	if why := OutboundTextRefusal("widget has a new logo: https://"+widgetRepo, widgetWords, []string{widgetRepo}); why != "" {
		t.Errorf("the bare name beside its link refused: %s", why)
	}
}

// TestOutboundBareRepoNameNeedsTheDeclaration (control): the exemption comes
// from the allowlist entry — none, another repository, the owner alone, a
// host alone or a deeper path (whatever its segments) declare nothing.
func TestOutboundBareRepoNameNeedsTheDeclaration(t *testing.T) {
	for _, allow := range [][]string{
		nil,
		{"example.org/acme/gadget"},
		{"example.org/acme"},
		{"widget.example.org"},
		{"example.org/acme/gadget/widget"},
		{"example.org/acme/widget/wiki"},
		{"example.org/widget"},
	} {
		if why := OutboundTextRefusal("widget has a new logo", widgetWords, allow); why != nameRefusal {
			t.Errorf("allowlist %q: %q; want %q", allow, why, nameRefusal)
		}
	}
}

// TestOutboundBareRepoNameExemptsOnlyThatWord (security guard): every other
// deny word in the same draft is refused as before — another deployment name,
// the repository's OWNER, a deny word that merely contains the name, and the
// owner even when it is also a declared repository's name.
func TestOutboundBareRepoNameExemptsOnlyThatWord(t *testing.T) {
	allow := []string{widgetRepo}
	for _, text := range []string{
		"widget has a new logo, built on examplecluster",
		"widget has a new logo, from acme",
		"widget has a new logo; widgetstore is down",
	} {
		if why := OutboundTextRefusal(text, widgetWords, allow); why != nameRefusal {
			t.Errorf("%q: %q; want %q", text, why, nameRefusal)
		}
	}
	// A deny word that contains the name is screened whole, as before: it
	// does not match the bare name, and the name does not unlock it.
	if why := OutboundTextRefusal("widgets and widgetry", widgetWords, allow); why != "" {
		t.Errorf("words that only contain the name: %q; want passed, as at base", why)
	}
	// An owner that is also a declared repository's name stays refused.
	if why := OutboundTextRefusal("acme has a new logo", widgetWords, []string{widgetRepo, "example.org/other/acme"}); why != nameRefusal {
		t.Errorf("an owner that is also a repository name: %q; want %q", why, nameRefusal)
	}
	// The rest of the outbound screen is unchanged beside the exempt name.
	if why := OutboundTextRefusal("widget is in the namespace", widgetWords, allow); !strings.Contains(why, "an infrastructure word") {
		t.Errorf("an infrastructure word beside the name: %q", why)
	}
}

// TestOutboundBareRepoNameIsOnlyTheStandaloneName (security guard): the
// exemption is for the project's OWN name standing alone, and "alone" is an
// allowlist: each neighbour is the text's start or end, whitespace, one of
// , ; ! ? ' ’ " “ ” ( ) [ ], or after the name a `.` or `:` followed by
// whitespace or the end. Any other neighbour joins it to another name, which
// is screened as at base.
func TestOutboundBareRepoNameIsOnlyTheStandaloneName(t *testing.T) {
	allow := []string{widgetRepo}
	only := []string{"widget"}
	for _, text := range []string{
		"widget has a new logo.",
		"widget's",
		"Try widget.",
		"(widget)",
		"widget: new logo",
		"Try widget:",
		"(widget), widget; widget! widget? [widget]",
		"widget\u2019s new logo",
		"\"widget\" and \u201cwidget\u201d",
		"widget\tnew\nwidget",
	} {
		if why := OutboundTextRefusal(text, only, allow); why != "" {
			t.Errorf("bare %q refused: %s", text, why)
		}
	}
	for _, text := range []string{
		"widget-prod is up",
		"prod-widget is up",
		"the widget.staging box",
		"see x.widget",
		"see a/widget",
		"see \u00e9widget",
		"widget\u2013prod",
		"widget\u2014prod",
		"widget:5432",
		"widget+prod",
		"widget~prod",
		"ops@widget",
		"widget\\prod",
		"widget._prod",
		"widget\u0301prod",
		"widget.prod",
		"Try widget...",
		"...widget",
		"see:widget",
		"see example.org/acme/widget-private",
		"see github.com/acme/widget-private",
	} {
		if why := OutboundTextRefusal(text, only, allow); why != nameRefusal {
			t.Errorf("compound %q: %q; want %q", text, why, nameRefusal)
		}
	}
	// Built at run time, as above, so the leak gate never reads a private host.
	if why := OutboundTextRefusal("widget."+"internal is up", only, allow); why == "" {
		t.Error("the name before an internal suffix passed")
	}
	// The pattern screens see the text unmasked: a token-shaped run the bare
	// name sits inside (between `+`, which does not continue a name) is still
	// one run.
	token := strings.Repeat("a", 20) + "+widget+" + strings.Repeat("b", 20)
	if why := OutboundTextRefusal("key "+token, only, allow); !strings.Contains(why, "a long token-shaped string") {
		t.Errorf("a token around the bare name: %q; want the token refusal", why)
	}
	// A compound that is itself a deny word: the bare name inside it is not
	// masked, so the compound is still found whole.
	for _, c := range []struct{ text, word string }{
		{"widget_staging is up", "widget_staging"},
		{"widget-prod is up", "widget-prod"},
		{"widget.staging is up", "widget.staging"},
		{"see widgetco", "widgetco"},
		{"widget2 is up", "widget2"},
		{"see prod/widget", "prod/widget"},
		{"widget prod is up", "widget prod"},
		{"the widget, prod", "widget, prod"},
	} {
		if why := OutboundTextRefusal(c.text, []string{"widget", c.word}, allow); why != nameRefusal {
			t.Errorf("%q with %q denied: %q; want %q", c.text, c.word, why, nameRefusal)
		}
	}
}

// TestReplyScreenKeepsThePublicRepoName: AgentTextRefusal (every non-officer
// agent text) has no allowlist and so no exempt name.
func TestReplyScreenKeepsThePublicRepoName(t *testing.T) {
	if why := AgentTextRefusal("widget has a new logo", widgetWords); why != nameRefusal {
		t.Errorf("reply screen: %q; want %q", why, nameRefusal)
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
