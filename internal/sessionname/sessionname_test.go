package sessionname

import (
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestResolveIsThePrecedence: each level wins over every level below it, and
// a level that plains to nothing falls through. Every fixture value is
// distinct, so a resolver that returned the wrong level cannot pass by
// coincidence.
func TestResolveIsThePrecedence(t *testing.T) {
	all := Parts{Manual: "Manual one", System: "System one", Auto: "Auto one", Dispatch: "Dispatch one",
		Fallback: "slug-one", Technical: "tech-one"}
	for _, c := range []struct {
		name string
		drop func(*Parts)
		want Name
	}{
		{"manual wins", func(*Parts) {}, Name{"Manual one", SourceManual, "tech-one"}},
		{"system next", func(p *Parts) { p.Manual = "" }, Name{"System one", SourceSystem, "tech-one"}},
		// A task's name is the stable name: above the auto name (operator
		// decision on #504, Q1).
		{"dispatch next", func(p *Parts) { p.Manual, p.System = "", "" }, Name{"Dispatch one", SourceDispatch, "tech-one"}},
		{"auto next", func(p *Parts) { p.Manual, p.System, p.Dispatch = "", "", "" }, Name{"Auto one", SourceAuto, "tech-one"}},
		{"fallback last", func(p *Parts) { p.Manual, p.System, p.Auto, p.Dispatch = "", "", "", "" }, Name{"slug-one", SourceFallback, "tech-one"}},
		// A manual name that plains to "" (only white space) falls through.
		{"empty after plaining falls through", func(p *Parts) { p.Manual = " \t " }, Name{"System one", SourceSystem, "tech-one"}},
		// Markdown in a name is plained; the fallback never is.
		{"plained", func(p *Parts) { p.Manual = "Fix the **login** page" }, Name{"Fix the login page", SourceManual, "tech-one"}},
		{"fallback not plained", func(p *Parts) { p.Manual, p.System, p.Auto, p.Dispatch, p.Fallback = "", "", "", "", "my__proj" },
			Name{"my__proj", SourceFallback, "tech-one"}},
	} {
		p := all
		c.drop(&p)
		if got := Resolve(p); got != c.want {
			t.Errorf("%s: Resolve = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestAnAgentsDisplayNameRanksByItsSource: the same display name is manual,
// system, dispatch or the slug's copy according to who set it — and a manual,
// system or dispatch (task) one outranks the auto name: a task's name never
// changes on its own.
func TestAnAgentsDisplayNameRanksByItsSource(t *testing.T) {
	stored := focus.Name{Auto: "Billing migration"}
	for _, c := range []struct {
		src, display string
		want         Name
	}{
		{agents.NameManual, "My billing work", Name{"My billing work", SourceManual, "brave-otter"}},
		{agents.NameSystem, "chief", Name{"chief", SourceSystem, "brave-otter"}},
		{agents.NameDispatch, "#42 · Fix the login bug", Name{"#42 · Fix the login bug", SourceDispatch, "brave-otter"}},
		{agents.NameDefault, "brave-otter", Name{"Billing migration", SourceAuto, "brave-otter"}},
		// A row from before the column: "" reads as default.
		{"", "brave-otter", Name{"Billing migration", SourceAuto, "brave-otter"}},
	} {
		a := agents.Agent{Name: "brave-otter", DisplayName: c.display, DisplayNameSource: c.src}
		if got := Agent(a, stored); got != c.want {
			t.Errorf("source %q: %+v, want %+v", c.src, got, c.want)
		}
	}
	// With no auto name at all: a dispatch name heads the agent, a slug copy
	// is the fallback.
	if got := Agent(agents.Agent{Name: "brave-otter", DisplayName: "#42 · Fix it", DisplayNameSource: agents.NameDispatch}, focus.Name{}); got.Source != SourceDispatch || got.Text != "#42 · Fix it" {
		t.Errorf("dispatch before freeze: %+v", got)
	}
	if got := Agent(agents.Agent{Name: "brave-otter", DisplayName: "brave-otter"}, focus.Name{}); got != (Name{"brave-otter", SourceFallback, "brave-otter"}) {
		t.Errorf("slug before freeze: %+v", got)
	}
	// A default-sourced display name that is not the slug (a writer that set
	// no source) heads the agent PLAINED, as a task's name does — and, like
	// one, the auto name never replaces it.
	odd := agents.Agent{Name: "brave-otter", DisplayName: "#41 · **Fix** the login"}
	if got := Agent(odd, focus.Name{}); got != (Name{"#41 · Fix the login", SourceDispatch, "brave-otter"}) {
		t.Errorf("default-sourced custom name: %+v", got)
	}
	if got := Agent(odd, stored); got.Source != SourceDispatch {
		t.Errorf("default-sourced custom name after the freeze: %+v, want it kept", got)
	}

}

// TestAMirroredSessionsLabelIsAFallback: a window's host label ("mango:1")
// names it only until its conversation has an auto or manual name; the
// technical id is the window's tmux address on its host either way.
func TestAMirroredSessionsLabelIsAFallback(t *testing.T) {
	e := mirror.SessionView{Host: "hosta", Session: "work", Window: "@3", Index: 2, Label: "mango:2", ClaudeSession: "sid-1"}
	tech := e.Where()
	if got := Mirror(e, focus.Name{}); got != (Name{"mango:2", SourceFallback, tech}) {
		t.Errorf("no stored name: %+v", got)
	}
	if got := Mirror(e, focus.Name{Auto: "Stripe webhooks"}); got != (Name{"Stripe webhooks", SourceAuto, tech}) {
		t.Errorf("auto: %+v", got)
	}
	if got := Mirror(e, focus.Name{Auto: "Stripe webhooks", Manual: "Payments"}); got != (Name{"Payments", SourceManual, tech}) {
		t.Errorf("manual over auto: %+v", got)
	}
	// No label: the tmux address, qualified when the list needed it.
	e.Label, e.Qualifier = "", "hosta"
	if got := Mirror(e, focus.Name{}); got.Text != "work:2 · hosta" || got.Source != SourceFallback {
		t.Errorf("no label: %+v", got)
	}
	if !Mirror(e, focus.Name{Auto: "x y"}).Human() || Mirror(e, focus.Name{}).Human() {
		t.Error("Human: true for a stored name, false for the fallback")
	}
}
