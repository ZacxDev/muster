package main

import "testing"

// TestSeedRefusesAnythingButAnE2EDatabase pins the guard that keeps both the
// browser suite AND the manifest screenshots on fixture data: `npm run capture`
// shares global-setup, which runs this seed (and so this check) before it
// starts the server it photographs. The screenshots are served from
// /static/icons/, which is planned to be reachable without auth.
func TestSeedRefusesAnythingButAnE2EDatabase(t *testing.T) {
	for _, c := range []struct {
		dsn string
		ok  bool
	}{
		{"postgres://muster:muster@127.0.0.1:5432/muster_e2e?sslmode=disable", true},
		{"postgres://u:p@db/x_e2e", true},
		{"", false},
		{"postgres://muster:muster@postgres.muster.svc:5432/muster?sslmode=disable", false},
		{"postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable", false},
		{"postgres://u:p@db/muster_e2e_old", false},
		{"postgres://u:p@db/muster?dbname=muster_e2e", false},
		{"postgres://u:p@db/", false},
	} {
		err := checkE2EDatabase(c.dsn)
		if (err == nil) != c.ok {
			t.Errorf("checkE2EDatabase(%q) = %v, want ok=%v", c.dsn, err, c.ok)
		}
	}
}
