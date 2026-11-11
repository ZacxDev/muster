package api

import (
	"context"
	"testing"
)

// TestEnableOfficersBuildsThePoolsReporter: EnableOfficers hands out a pools
// reporter for the declared officers, wired to each officer's HQ with its
// token, and does not start it (cmd/muster-server runs it with the background
// loops): nothing is pushed until it ticks, and a tick pushes the officer's
// one untiered pool.
func TestEnableOfficersBuildsThePoolsReporter(t *testing.T) {
	s, _, _, hq, o, _ := usageServer(t)
	p := s.OfficerPoolsReporter()
	if p == nil {
		t.Fatal("no pools reporter for a declared officer")
	}
	if n := len(hq.PoolsPuts()); n != 0 {
		t.Fatalf("%d pools pushes before the reporter ran", n)
	}
	p.Tick(context.Background())
	puts := hq.PoolsPuts()
	want := `{"pools":[{"pool":"","model":"","max_instances":1,"max_sessions_per_day":null,"sessions_today":0,` +
		`"sessions_day_resets_at":"2000-01-04T00:00:00Z"}],"reported_at":"2000-01-03T12:20:00Z"}`
	if len(puts) != 1 || puts[0].Officer != o.ID || string(puts[0].Raw) != want {
		t.Fatalf("pools PUTs %+v\nwant one: %s", puts, want)
	}
	if New(nil, AuthConfig{}, nil).OfficerPoolsReporter() != nil {
		t.Fatal("a server without officers has a pools reporter")
	}
}
