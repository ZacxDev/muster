package api

import (
	"regexp"
	"strings"
	"testing"
)

// The wizard's templates (officer_templates.go, officers O-D): the catalogue,
// each template's starting values pinned as literals, that each is a valid
// create as it stands, and that no template text names a host, an address or
// a mail box (they ship in a public repository).

// TestTheTemplateCatalogue pins the "Start from" choices, in order.
func TestTheTemplateCatalogue(t *testing.T) {
	want := [][2]string{
		{"blank", "Blank officer"},
		{"session-minder", "Session minder"},
		{"marketing", "Marketing (you post by hand)"},
		{"qa", "QA"},
	}
	if len(officerTemplates) != len(want) {
		t.Fatalf("%d templates, want %d", len(officerTemplates), len(want))
	}
	for i, w := range want {
		if got := officerTemplates[i]; got.Key != w[0] || got.Name != w[1] || got.Summary == "" {
			t.Errorf("template %d: %q %q (summary %q); want %q %q", i, got.Key, got.Name, got.Summary, w[0], w[1])
		}
	}
	if got := templateNamed("no-such-template").Key; got != "blank" {
		t.Errorf("an unknown key starts from %q; want blank", got)
	}
}

// TestEachTemplatesStartingValues pins what each template fills in — and, as
// important, what it leaves blank (no repository, no connectors, no host,
// Shadow by default).
func TestEachTemplatesStartingValues(t *testing.T) {
	type want struct {
		name, runtime, wake, preset, cron, caps, policy string
		charterHas                                      []string
	}
	cases := map[string]want{
		"blank": {policy: `{}`},
		"session-minder": {name: "Session minder", runtime: "auto", wake: "manual", caps: "session_minder",
			policy: `{"capabilities":["session_minder"]}`,
			charterHas: []string{"session.stalled", "my chief does not nudge me", "officer_ask_person", "no_action",
				"Never type into a session"}},
		"marketing": {name: "Marketing", runtime: "auto", wake: "schedule", preset: "weekly", cron: "0 9 * * 1", policy: `{}`,
			charterHas: []string{"Your venue is manual: I post what you draft.", "You never post", "officer_ask_person",
				`"Posted", "Change it" and "Skip"`}},
		"qa": {name: "QA", runtime: "auto", wake: "schedule", preset: "weekdays", cron: "0 9 * * 1-5", policy: `{}`,
			charterHas: []string{"You test; you never fix.", "never the whole suite", "complete the item as escalated"}},
	}
	for key, w := range cases {
		tm := templateNamed(key)
		if tm.Key != key {
			t.Fatalf("no template %q", key)
		}
		v := tm.Values
		if v.Name != w.name || v.Runtime != w.runtime || v.Wake != w.wake || v.Preset != w.preset || strings.Join(v.Caps, ",") != w.caps {
			t.Errorf("%s: name %q runtime %q wake %q preset %q caps %v; want %q %q %q %q %q", key, v.Name, v.Runtime, v.Wake, v.Preset, v.Caps,
				w.name, w.runtime, w.wake, w.preset, w.caps)
		}
		if v.Repo != "" || v.Host != "" || v.Connectors != "" || v.ReadRepos != "" || v.ID != "" || len(v.Escalate) != 0 || v.Mode != "" || v.Cron != "" {
			t.Errorf("%s fills in a field it must leave to the person: %+v", key, v)
		}
		if got := wizardCron(v); got != w.cron {
			t.Errorf("%s: schedule %q; want %q", key, got, w.cron)
		}
		for _, s := range w.charterHas {
			if !strings.Contains(v.Charter, s) {
				t.Errorf("%s: its charter does not say %q", key, s)
			}
		}
		if key == "blank" {
			if v.Charter != "" {
				t.Errorf("blank has a charter: %q", v.Charter)
			}
			continue
		}
		// As it stands, the template is a create the core takes: no field the
		// form refuses, Shadow, and the policy above.
		in, errs := declaredInOf(v, false)
		if len(errs) != 0 {
			t.Errorf("%s as it stands is refused: %v", key, errs)
		}
		if in.Mode == nil || *in.Mode != "shadow" || string(in.Policy) != w.policy {
			t.Errorf("%s: mode %v policy %s; want shadow and %s", key, in.Mode, in.Policy, w.policy)
		}
	}
}

// templateLeak is what must never appear in a template's text: an IPv4
// address, a URL, a mail address, a host under a private-looking domain.
var templateLeak = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b|https?://|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}|\.(lan|local|internal|home|corp)\b`)

// TestNoTemplateNamesAHostAnAddressOrAMailBox (public repository).
func TestNoTemplateNamesAHostAnAddressOrAMailBox(t *testing.T) {
	// The guard can see a leak (positive control).
	for _, bad := range []string{"reach 192.0.2.7", "see https://example.org", "mail ops@example.org", "on build.corp"} {
		if !templateLeak.MatchString(bad) {
			t.Fatalf("control: the guard misses %q", bad)
		}
	}
	for _, tm := range officerTemplates {
		for field, text := range map[string]string{"name": tm.Name, "summary": tm.Summary, "charter": tm.Values.Charter} {
			if m := templateLeak.FindString(text); m != "" {
				t.Errorf("template %s's %s names %q", tm.Key, field, m)
			}
		}
	}
}
