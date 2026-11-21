package api

import (
	"html"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestADraftCardShowsExactlyWhatIsApproved: the reply is drawn through
// flows.VisibleEscape line by line (a non-ASCII letter and an invisible-looking
// character are spelled out, a newline is a line break) with its length in
// characters on data-runes (no longer a pill); the outside
// sender's text is HTML-escaped in its own labelled box; only a pending draft
// has Approve/Reject, and no card of any state offers a second approval (there
// is no re-approval: an unconfirmed draft is never offered again).
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
		"data-draft-body>caf\\u{e9}\nsee you</p>", `data-runes="12"`,
		`&lt;/pre&gt;&lt;script&gt;x()&lt;/script&gt;`, `&lt;b&gt;hi&lt;/b&gt;`, `Eve &lt;eve@example.invalid&gt;`,
		`data-draft-untrusted>untrusted</span>`, `data-draft-original`,
		`action="/plugins/drafts/9/approve" data-step-up-form`, `action="/plugins/drafts/9/reject"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the pending card lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"<script>", "café", `caf\u{e9}\nsee`, "12 characters", "data-draft-runes"} {
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

// TestADraftBodyIsDrawnLineByLineAndStillSpellsOutHiddenCharacters: a draft's
// line breaks are drawn as line breaks (the Drafts tab drew a post's blank
// line as a literal "\n\n"), while everything VisibleEscape spells out is
// still spelled out — a right-to-left override, a tab, a carriage return, a
// backslash — and HTML in the body is text. Split on the line breaks and
// unescaped line by line, the drawn text is the body's exact bytes.
// RED before visibleLines: the card drew `one\n\ntwo` on one line.
func TestADraftBodyIsDrawnLineByLineAndStillSpellsOutHiddenCharacters(t *testing.T) {
	at := time.Now().UTC()
	agentID := int64(3)
	body := "Launch day.\n\nPay \u202egro.live\u202c now\t<script>alert(1)</script>\r\\n done"
	c := plugins.DraftCard{
		Draft: plugins.Draft{ID: 12, AgentID: &agentID, AgentName: "chief", Body: body, Status: plugins.DraftPending,
			CreatedAt: at, ExpiresAt: at.Add(plugins.DraftTTL)},
		Message:     plugins.Message{ID: 4, Sender: "eve@example.invalid", Body: "hi"},
		PluginLabel: "Example inbox",
	}
	got := draftCardHTML(c)
	const want = "data-draft-body>Launch day.\n\nPay \\u{202e}gro.live\\u{202c} now\\t&lt;script&gt;alert(1)&lt;/script&gt;\\u{d}\\\\n done</p>"
	if !strings.Contains(got, want) {
		t.Fatalf("the draft body is not drawn line by line with its hidden characters spelled out; want %q in:\n%s", want, got)
	}
	for _, bad := range []string{"<script>", "\u202e", "\u202c", "\r", `day.\n\nPay`} {
		if strings.Contains(got, bad) {
			t.Errorf("the card carries %q", bad)
		}
	}
	// Decoded line by line, it is the body's exact bytes.
	rest := got[strings.Index(got, "data-draft-body>")+len("data-draft-body>"):]
	drawn := html.UnescapeString(rest[:strings.Index(rest, "</p>")])
	lines := strings.Split(drawn, "\n")
	for i, l := range lines {
		u, ok := flows.UnescapeVisible(l)
		if !ok {
			t.Fatalf("line %d %q is not VisibleEscape's", i, l)
		}
		lines[i] = u
	}
	if back := strings.Join(lines, "\n"); back != body {
		t.Fatalf("decoded %q, want %q", back, body)
	}
}
