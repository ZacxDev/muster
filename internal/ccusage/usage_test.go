package ccusage

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// liveShape is the shape the endpoint answered to one live call (a full-scope
// login), trimmed: most keys null, five_hour null (no session window open),
// seven_day set, extra_usage a money block with a null utilization, and the
// limits array.
const liveShape = `{
 "five_hour": null,
 "seven_day": {"utilization": 34.0, "resets_at": "2026-11-17T08:59:59.745797+00:00", "limit_dollars": null, "locked_reason": null},
 "seven_day_oauth_apps": null, "seven_day_opus": null, "seven_day_sonnet": null, "cinder_cove": null,
 "extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null, "currency": null},
 "limits": [{"kind": "weekly_all", "group": "weekly", "percent": 34, "severity": "normal",
             "resets_at": "2026-11-17T08:59:59.745797+00:00", "scope": null, "is_active": true}],
 "spend": {"used": {"amount_minor": 0, "currency": "USD", "exponent": 2}, "percent": 0},
 "member_dashboard_available": false
}`

func util(t *testing.T, r Reading, name string) float64 {
	t.Helper()
	w, ok := r.Windows[name]
	if !ok || w.Utilization == nil {
		t.Fatalf("window %s: %+v (present %v)", name, w, ok)
	}
	return *w.Utilization
}

func TestParseTheLiveShape(t *testing.T) {
	r, err := Parse([]byte(liveShape))
	if err != nil {
		t.Fatal(err)
	}
	if got := util(t, r, SevenDay); got != 34 {
		t.Fatalf("seven_day = %v, want 34", got)
	}
	want := time.Date(2026, 11, 17, 8, 59, 59, 745797000, time.UTC)
	if rs := r.Windows[SevenDay].ResetsAt; rs == nil || !rs.Equal(want) {
		t.Fatalf("seven_day resets_at = %v, want %v", rs, want)
	}
	if _, ok := r.Windows[FiveHour]; ok {
		t.Fatal("a null five_hour became a window")
	}
	// extra_usage carries a utilization key, null, and no resets_at: nothing
	// in it. spend has no utilization key.
	if _, ok := r.Windows["extra_usage"]; ok {
		t.Fatal("an extra_usage with nothing in it became a window")
	}
	if _, ok := r.Windows["spend"]; ok {
		t.Fatal("spend (no utilization key) became a window")
	}
	if len(r.Windows) != 1 {
		t.Fatalf("windows = %v, want seven_day alone", r.Windows)
	}
}

func TestParseEveryWindowAndPerModelLimits(t *testing.T) {
	body := `{"five_hour": {"utilization": 87.5, "resets_at": "2026-11-10T20:00:00Z"},
	 "seven_day": {"utilization": 12, "resets_at": "2026-11-15T00:00:00Z"},
	 "seven_day_opus": {"utilization": 3, "resets_at": null},
	 "seven_day_sonnet": {"utilization": null, "resets_at": null},
	 "new_window_name": {"utilization": 5, "resets_at": "2026-11-12T00:00:00Z"},
	 "limits": [{"kind": "weekly_scoped", "percent": 61, "resets_at": "2026-11-15T00:00:00Z", "scope": {"model": {"display_name": "Fable"}}},
	            {"kind": "weekly_scoped", "percent": 99, "scope": {"model": {"display_name": "fable"}}},
	            {"kind": "weekly_all", "percent": 12, "scope": null}]}`
	r, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{FiveHour: 87.5, SevenDay: 12, "seven_day_opus": 3, "new_window_name": 5, "seven_day_model:fable": 61} {
		if got := util(t, r, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if _, ok := r.Windows["seven_day_sonnet"]; ok {
		t.Error("a seven_day_sonnet with both values null became a window")
	}
	if r.Windows["seven_day_opus"].ResetsAt != nil {
		t.Error("a null resets_at became a time")
	}
	if len(r.Windows) != 5 {
		t.Errorf("windows = %d, want 5: %v", len(r.Windows), r.Windows)
	}
}

func TestParseAnswerWithNoWindows(t *testing.T) {
	r, err := Parse([]byte(`{"five_hour": null, "seven_day": null}`))
	if err != nil || len(r.Windows) != 0 {
		t.Fatalf("Parse = %+v, %v; want no windows, no error", r, err)
	}
}

func TestParseRefusesErrorBodiesAndNonObjects(t *testing.T) {
	for _, body := range []string{
		`{"type": "error", "error": {"type": "permission_error", "message": "OAuth token does not meet scope requirement user:profile"}}`,
		`[1, 2]`, `null`, `"text"`, `not json`, ``,
	} {
		if _, err := Parse([]byte(body)); !errors.Is(err, ErrNotUsage) {
			t.Errorf("Parse(%q) = %v, want ErrNotUsage", body, err)
		}
	}
	_, err := Parse([]byte(`{"type": "error", "error": {"type": "permission_error", "message": "secret sk-ant-oat01-XYZ"}}`))
	if err == nil || strings.Contains(err.Error(), "sk-ant") || !strings.Contains(err.Error(), "permission_error") {
		t.Fatalf("the error object's error = %v; want its type only, never its message", err)
	}
}

// TestParseSkipsMalformedWindows: a window whose utilization is not a number
// or whose resets_at is not a time is dropped, not read as zero; the others stand.
func TestParseSkipsMalformedWindows(t *testing.T) {
	r, err := Parse([]byte(`{"five_hour": {"utilization": "high", "resets_at": null},
	 "seven_day": {"utilization": 10, "resets_at": "tomorrow"},
	 "seven_day_opus": {"utilization": -1},
	 "Bad Name": {"utilization": 1},
	 "seven_day_sonnet": {"utilization": 9}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Windows) != 1 || util(t, r, "seven_day_sonnet") != 9 {
		t.Fatalf("windows = %v, want only seven_day_sonnet", r.Windows)
	}
}

// TestParseBoundsTheWindowCount keeps five_hour and seven_day whatever sorts
// before them.
func TestParseBoundsTheWindowCount(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"five_hour": {"utilization": 1}, "seven_day": {"utilization": 2}`)
	for i := 0; i < 3*MaxWindows; i++ {
		b.WriteString(`, "a` + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `": {"utilization": 3}`)
	}
	b.WriteString(`}`)
	r, err := Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Windows) != MaxWindows {
		t.Fatalf("windows = %d, want %d", len(r.Windows), MaxWindows)
	}
	if util(t, r, FiveHour) != 1 || util(t, r, SevenDay) != 2 {
		t.Fatal("the cap dropped five_hour or seven_day")
	}
}
