package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE PLUGINS, RULES, USAGE AND DRAFT SCREENS IN PLAIN WORDS (jargon audit,
// "settings" theme: F060–F070, U-25). Literal expectations, checked on the
// same render as the old wording's absence.

// listedPlugins is a plugin store listing a fixed set, with nothing queued
// or retryable.
type listedPlugins struct {
	plugins.Store
	list []plugins.Plugin
}

func (l listedPlugins) List(context.Context) ([]plugins.Plugin, error) { return l.list, nil }
func (l listedPlugins) QueuedCounts(context.Context) (map[int64]int, error) {
	return map[int64]int{}, nil
}
func (l listedPlugins) Retryable(context.Context, int) ([]plugins.Message, error) { return nil, nil }

// TestThePluginsPageIsPlain (F060, F061, F063, F064, F065): the lead is
// "Plugins bring messages from other apps to your chief." (the token detail
// in its ⓘ); the create form's field is "Notes topic (optional)"; a card says
// "Can post messages and check drafts" (the raw names in its ⓘ), "Delivers
// to Marketing — Acme" with a Send to choice by name, and "Notes topic:
// social · added on Jan 2, 2000".
func TestThePluginsPageIsPlain(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	to := "officer:marketing:acme"
	s.UseExtensions(Extensions{Users: stubUsers{}, Plugins: listedPlugins{list: []plugins.Plugin{{
		ID: 3, Name: "acme-inbox", Label: "Acme inbox", DataScope: "social", CreatedAt: plainDay,
		Capabilities: []plugins.Capability{plugins.CapMessages, plugins.CapDraftChecks}, DeliverTo: &to,
	}}}})
	page := mainOf(t, render(t, asRole(authz.RoleAdmin), func(w http.ResponseWriter, r *http.Request) {
		s.renderPluginsPage(w, r, http.StatusOK, "", "")
	}))
	for _, want := range []string{
		`<p class="text-sm text-fg2">Plugins bring messages from other apps to your chief.</p>`,
		`>Notes topic (optional) <span class="text-xs text-fg2">where your chief keeps notes from these messages (empty: the name)</span></label>`,
		`>Name <span class="text-xs text-fg2">lowercase letters, numbers and dashes</span></label>`,
		`data-plugin-capabilities>Can post messages and check drafts</span>`,
		`data-info-code="">can: messages, draft_checks</code>`,
		`data-plugin-delivers-to>Delivers to <span class="text-fg">Marketing — Acme</span></span>`,
		`<option value="officer:marketing:acme" selected>Marketing — Acme</option>`,
		`Notes topic: social · <time datetime="2000-01-02T03:04:05Z" data-plain-when="946782245000" data-plain-when-prefix="added ">added on Jan 2, 2000</time>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Plugins page has no %s", want)
		}
	}
	top := stripSheets(page)
	for _, gone := range []string{"Data scope", "can: messages", ">officer:marketing:acme<", "scope social", "UTC", "separate process", "plugin token, which"} {
		if strings.Contains(top, gone) {
			t.Errorf("the Plugins page's top layer still says %q", gone)
		}
	}
}

// TestAPluginNameRefusalIsPlain (F066): a bad plugin name is refused with
// "Use up to 40 lowercase letters, numbers and dashes, not starting or ending
// with a dash." — never "1-40 lower-case letters, digits and hyphens".
func TestAPluginNameRefusalIsPlain(t *testing.T) {
	err := plugins.ValidName("")
	if err == nil || !strings.HasSuffix(err.Error(), ": Use up to 40 lowercase letters, numbers and dashes, not starting or ending with a dash.") {
		t.Errorf("an empty plugin name: %v", err)
	}
}

// TestTheRoutingRulesLeadIsPlain (F067).
func TestTheRoutingRulesLeadIsPlain(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	page := render(t, asRole(authz.RoleAdmin), func(w http.ResponseWriter, r *http.Request) { s.renderRulesPage(w, r, http.StatusOK, "") })
	if !strings.Contains(page, `<p class="text-sm text-fg2">Rules handle routine plugin messages without asking your chief. Your chief suggests them; you approve each one.</p>`) {
		t.Errorf("the Routing rules lead is not the plain one:\n%.3000s", mainOf(t, page))
	}
	if strings.Contains(page, "places a plugin message without your chief") {
		t.Error("the old lead is still on the page")
	}
}

// plainUsage is a chiefusage.Store whose report is fixed.
type plainUsage struct {
	chiefusage.Store
	rep chiefusage.Report
}

func (f plainUsage) Report(context.Context, time.Time) (chiefusage.Report, error) { return f.rep, nil }

// TestTheUsageTotalsAndDaysArePlain (F068, F069, F070): the totals say "2
// chief replies" and each day "3 messages", the token and source breakdowns
// in ⓘs; the token footnote is in the totals' ⓘ, not at the page's end.
func TestTheUsageTotalsAndDaysArePlain(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	tok := chiefusage.Tokens{Input: 1000, Output: 200, CacheRead: 3000, CacheCreation: 50}
	s.UseExtensions(Extensions{ChiefUsage: plainUsage{rep: chiefusage.Report{
		Totals: chiefusage.Totals{Turns: 2, Tokens: tok, Messages: 3, ByRule: 1, ByMuster: 1, ByChief: 1},
		Days:   []chiefusage.Day{{Day: plainDay, Turns: 2, Tokens: tok, Messages: 3, ByRule: 1, ByMuster: 1, ByChief: 1}},
	}}})
	page := mainOf(t, render(t, asRole(authz.RoleAdmin), s.handleUsagePage))
	for _, want := range []string{
		`<span class="text-sm text-fg" data-usage-replies>2 chief replies</span>`,
		`<span class="text-xs text-fg2" data-usage-day-messages>3 messages</span>`,
		`data-info-code="">1 rule · 1 muster · 1 chief · 2 replies · 4,250 tokens</code>`,
		`data-info-code="">2 chief replies · 4,250 tokens (in 1,000 · out 200 · cache read 3,000 · cache write 50)</code>`,
		"A token total is a count, not a cost",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Usage page has no %s\n%.5000s", want, page)
		}
	}
	top := stripSheets(page)
	for _, gone := range []string{"chief turns ·", "cache read", "(1 rule ·", "A token total is a count"} {
		if strings.Contains(top, gone) {
			t.Errorf("the Usage page's top layer still says %q", gone)
		}
	}
}

// TestADraftCardNamesItsOfficer (U-25): an officer's outbound draft is headed
// by what the officer is for ("Marketing officer (Acme)"), never its agent's
// slug, and its target reads "New post on X" — not "Outbound · x · New
// post"; its Attention line is "New post on X by the Marketing officer
// (Acme)".
func TestADraftCardNamesItsOfficer(t *testing.T) {
	officer, venue, plugin := "marketing:acme", "x", int64(3)
	c := plugins.DraftCard{Draft: plugins.Draft{ID: 11, AgentName: "marketing-acme-1", Body: "Hello from acme/api.", Status: plugins.DraftPending,
		CreatedAt: plainDay, ExpiresAt: plainDay.Add(72 * time.Hour), Officer: &officer, Venue: &venue, PluginID: &plugin},
		PluginLabel: "Acme social"}
	card := draftCardHTMLAt(c, plainDay.Add(time.Hour))
	for _, want := range []string{
		`data-draft-agent>Marketing officer (Acme)</span>`,
		`data-draft-outbound-what>New post on X</span>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the draft card has no %s\n%s", want, card)
		}
	}
	for _, gone := range []string{"marketing-acme-1", "Outbound · ", "UTC"} {
		if strings.Contains(card, gone) {
			t.Errorf("the draft card still says %q", gone)
		}
	}
	if got, want := outboundAttentionText(c), "New post on X by the Marketing officer (Acme)"; got != want {
		t.Errorf("the Attention line = %q, want %q", got, want)
	}
}
