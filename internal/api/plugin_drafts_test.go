package api

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/plugins"
)

// TestADraftCardShowsExactlyWhatIsApproved: the reply is drawn as the post
// it is (the step-up card's preview: a non-ASCII letter as itself, a newline
// a line break, never spelled out) with its length in characters on
// data-runes (no longer a pill); the outside sender's text is HTML-escaped in
// its own labelled box; only a pending draft has Approve/Reject, and no card
// of any state offers a second approval (there is no re-approval: an
// unconfirmed draft is never offered again).
func TestADraftCardShowsExactlyWhatIsApproved(t *testing.T) {
	at := time.Now().UTC()
	agentID := int64(3)
	c := plugins.DraftCard{
		Draft: plugins.Draft{ID: 9, AgentID: &agentID, AgentName: "chief", Body: "café\nsee you", Status: plugins.DraftPending,
			CreatedAt: at, ExpiresAt: at.Add(plugins.DraftTTL)},
		Message:     plugins.Message{ID: 4, Sender: `Eve <eve@example.invalid>`, Subject: `<b>hi</b>`, Body: `</pre><script>x()</script>`},
		PluginLabel: "Example inbox",
	}
	got := draftCardHTML(c)
	for _, want := range []string{
		`id="draft-9"`, `data-draft-status="pending"`,
		"data-stepup-preview-text><p class=\"whitespace-pre-wrap break-words\">café\nsee you</p></div>", `data-runes="12"`,
		`&lt;/pre&gt;&lt;script&gt;x()&lt;/script&gt;`, `&lt;b&gt;hi&lt;/b&gt;`, `Eve &lt;eve@example.invalid&gt;`,
		`data-draft-untrusted>untrusted</span>`, `data-draft-original`,
		`action="/plugins/drafts/9/approve" data-step-up-form`, `action="/plugins/drafts/9/reject"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the pending card lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"<script>", `\u{`, `caf\u{e9}\nsee`, "12 characters", "data-draft-runes"} {
		if strings.Contains(got, bad) {
			t.Errorf("the pending card carries %q", bad)
		}
	}
	// Approved and not yet claimed: the approval can be withdrawn; once claimed, not.
	c.Draft.Status = plugins.DraftApproved
	if got := draftCardHTML(c); !strings.Contains(got, `action="/plugins/drafts/9/reject"`) || !strings.Contains(got, "data-draft-withdraw") {
		t.Errorf("an approved, unclaimed card has no Withdraw:\n%s", got)
	}
	c.Draft.ClaimedAt = &at
	if got := draftCardHTML(c); strings.Contains(got, "data-draft-withdraw") {
		t.Errorf("a claimed card offers Withdraw:\n%s", got)
	}
	c.Draft.ClaimedAt = nil
	// A pending card whose message has an unconfirmed earlier reply says so.
	c.Draft.Status, c.EarlierUnconfirmed = plugins.DraftPending, true
	if got := draftCardHTML(c); !strings.Contains(got, "data-draft-twice") {
		t.Errorf("the send-twice warning is missing:\n%s", got)
	}
	c.EarlierUnconfirmed = false
	// The approval surface, structurally: the step-up form is the ONLY way an
	// approval is posted, so its count per state is the claim — one on a pending
	// card, none on any other (an unconfirmed card included).
	for _, st := range []plugins.DraftStatus{plugins.DraftPending, plugins.DraftApproved, plugins.DraftSent, plugins.DraftFailed,
		plugins.DraftRejected, plugins.DraftUnconfirmed, plugins.DraftExpired, plugins.DraftSuperseded} {
		c.Draft.Status = st
		got := draftCardHTML(c)
		want := 0
		if st == plugins.DraftPending {
			want = 1
		}
		if n := strings.Count(got, "data-step-up-form"); n != want || strings.Count(got, "/approve\"") != want {
			t.Errorf("%s card: %d approval form(s); want %d:\n%s", st, n, want, got)
		}
	}
}

// TestADraftBodyIsDrawnAsItsPostAndFlagsHiddenCharacters: a draft's text is
// drawn as the post reads — its blank line a paragraph break, its line
// breaks breaks (the Drafts tab drew a post's blank line as a literal
// "\n\n", then every em dash as \u{2014}) — while each hidden character (a
// right-to-left override and its pop) is a visible flag naming it, never
// written into the page and never spelled out, and HTML in the body is text.
// Read back — each flag its code point, each paragraph break a blank line —
// the drawn text is the body's exact characters (a tab is drawn as a space,
// a carriage return as a line break).
func TestADraftBodyIsDrawnAsItsPostAndFlagsHiddenCharacters(t *testing.T) {
	at := time.Now().UTC()
	agentID := int64(3)
	body := "Launch day.\n\nPay \u202egro.live\u202c now\t<script>alert(1)</script>\r\\n done — 🎉"
	c := plugins.DraftCard{
		Draft: plugins.Draft{ID: 12, AgentID: &agentID, AgentName: "chief", Body: body, Status: plugins.DraftPending,
			CreatedAt: at, ExpiresAt: at.Add(plugins.DraftTTL)},
		Message:     plugins.Message{ID: 4, Sender: "eve@example.invalid", Body: "hi"},
		PluginLabel: "Example inbox",
	}
	got := draftCardHTML(c)
	flag := func(cp string) string {
		return `<mark class="mx-px inline-block rounded-sm bg-danger/15 px-1 align-baseline font-mono text-[11px] font-semibold leading-tight text-fg ring-1 ring-inset ring-danger/60" data-stepup-hidden="` +
			cp + `" title="Hidden character ` + cp + `">` + cp + `</mark>`
	}
	want := `data-stepup-preview-text><p class="whitespace-pre-wrap break-words">Launch day.</p><p class="whitespace-pre-wrap break-words">Pay ` + flag("U+202E") + `gro.live` + flag("U+202C") +
		" now &lt;script&gt;alert(1)&lt;/script&gt;\n\\n done — 🎉</p></div>"
	if !strings.Contains(got, want) {
		t.Fatalf("the draft body is not drawn as its post with its hidden characters flagged; want %q in:\n%s", want, got)
	}
	if !strings.Contains(got, `data-stepup-hidden-note>`) {
		t.Errorf("the card does not say the draft holds hidden characters:\n%s", got)
	}
	for _, bad := range []string{"<script>", "\u202e", "\u202c", "\r", `\u{`, `day.\n\nPay`} {
		if strings.Contains(got, bad) {
			t.Errorf("the card carries %q", bad)
		}
	}
	// Read back, it is the body.
	rest := got[strings.Index(got, "data-stepup-preview-text>")+len("data-stepup-preview-text>"):]
	drawn := rest[:strings.Index(rest, "</div>")]
	drawn = regexp.MustCompile(`<mark [^>]*data-stepup-hidden="U\+([0-9A-F]+)"[^>]*>[^<]*</mark>`).ReplaceAllStringFunc(drawn, func(m string) string {
		n, _ := strconv.ParseInt(regexp.MustCompile(`U\+([0-9A-F]+)"`).FindStringSubmatch(m)[1], 16, 32)
		return string(rune(n))
	})
	drawn = strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(drawn, `<p class="whitespace-pre-wrap break-words">`), "</p>"), `</p><p class="whitespace-pre-wrap break-words">`, "\n\n")
	if back, norm := html.UnescapeString(drawn), strings.NewReplacer("\t", " ", "\r", "\n").Replace(body); back != norm {
		t.Fatalf("read back %q, want %q", back, norm)
	}
}
