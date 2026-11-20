package db_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE PLUGIN CAPABILITY CHECKS AND GO'S LISTS ARE ONE SET. Each capability
// migration REPLACES plugins_capabilities_known (and, from 0166, the pairing
// request's check) whole, so a migration written beside another's — two
// capability PRs in flight — that lists only its own value silently refuses
// every token of the other's. This pins three relationships:
//
//   - the LIVE constraints of a fully migrated database list exactly
//     plugins.KnownCapabilities and plugins.PairableCapabilities;
//   - the newest migration that sets each constraint lists exactly them too
//     (the file a reviewer reads);
//   - each such migration's ROLLBACK block restores exactly the list the
//     previous one set, so a rollback cannot drop a capability it did not add.

var (
	arrayRE       = regexp.MustCompile(`ARRAY\[([^\]]*)\]`)
	quotedRE      = regexp.MustCompile(`'([^']*)'`)
	knownRE       = regexp.MustCompile(`ADD CONSTRAINT plugins_capabilities_known\s+CHECK \(capabilities <@ ARRAY\[[^\]]*\]`)
	pairNamedRE   = regexp.MustCompile(`ADD CONSTRAINT plugin_pair_requests_capabilities_check\s+CHECK \(capabilities <@ ARRAY\[[^\]]*\]`)
	pairInlineRE  = regexp.MustCompile(`capabilities\s+TEXT\[\]\s+NOT NULL CHECK \(capabilities <@ ARRAY\[[^\]]*\]`)
	migrationName = regexp.MustCompile(`^(\d{4})_.*\.sql$`)
)

// quotedValues is the sorted quoted values of the ARRAY[...] in s.
func quotedValues(s string) []string {
	m := arrayRE.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	var out []string
	for _, q := range quotedRE.FindAllStringSubmatch(m[1], -1) {
		out = append(out, q[1])
	}
	sort.Strings(out)
	return out
}

func capNames(cs []plugins.Capability) []string {
	var out []string
	for _, c := range cs {
		out = append(out, string(c))
	}
	sort.Strings(out)
	return out
}

// capabilityMigration is one migration's list for a constraint: in its body
// (what it applies) and in its ROLLBACK block (what it restores), each nil
// when the file does not say.
type capabilityMigration struct {
	name           string
	body, rollback []string
}

// capabilityMigrations is every migration that sets the constraint matched by
// re (or, when inline is set, creates it inline in a CREATE TABLE), oldest first.
func capabilityMigrations(t *testing.T, re, inline *regexp.Regexp) []capabilityMigration {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var out []capabilityMigration
	for _, e := range entries {
		mm := migrationName.FindStringSubmatch(e.Name())
		if mm == nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var body []string
		for _, l := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") {
				body = append(body, l)
			}
		}
		bodyText := strings.Join(body, "\n")
		cm := capabilityMigration{name: e.Name()}
		if m := re.FindString(bodyText); m != "" {
			cm.body = quotedValues(m)
		} else if inline != nil && strings.Contains(bodyText, "CREATE TABLE IF NOT EXISTS public.plugin_pair_requests") {
			if m := inline.FindString(bodyText); m != "" {
				cm.body = quotedValues(m)
			}
		}
		if cm.body == nil {
			continue
		}
		if m := re.FindString(strings.Join(asksRollbackSQL(t, e.Name()), " ")); m != "" {
			cm.rollback = quotedValues(m)
		}
		out = append(out, cm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func TestPluginCapabilityChecksMatchGo(t *testing.T) {
	for _, c := range []struct {
		constraint    string
		re, inline    *regexp.Regexp
		want          []string
		minMigrations int
		goName        string
	}{
		{"plugins_capabilities_known", knownRE, nil, capNames(plugins.KnownCapabilities), 4, "plugins.KnownCapabilities"},
		{"plugin_pair_requests_capabilities_check", pairNamedRE, pairInlineRE, capNames(plugins.PairableCapabilities), 2, "plugins.PairableCapabilities"},
	} {
		ms := capabilityMigrations(t, c.re, c.inline)
		if len(ms) < c.minMigrations {
			t.Fatalf("positive control: %d migrations set %s; want at least %d (the parser reads nothing?)", len(ms), c.constraint, c.minMigrations)
		}
		newest := ms[len(ms)-1]
		if !slices.Equal(newest.body, c.want) {
			t.Errorf("%s, the newest migration setting %s, lists %v; %s is %v — a capability migration must list EVERY capability, not just its own",
				newest.name, c.constraint, newest.body, c.goName, c.want)
		}
		for i := 1; i < len(ms); i++ {
			if ms[i].rollback == nil {
				continue
			}
			if !slices.Equal(ms[i].rollback, ms[i-1].body) {
				t.Errorf("%s's ROLLBACK restores %s as %v; the migration before it (%s) set %v", ms[i].name, c.constraint, ms[i].rollback, ms[i-1].name, ms[i-1].body)
			}
		}
		if newest.rollback == nil && len(ms) > 1 {
			t.Errorf("%s sets %s but its ROLLBACK block does not restore it", newest.name, c.constraint)
		}
	}

	// The LIVE constraints, on a database migrated to the newest file.
	ctx := ctx0006(t)
	latest := 0
	entries, _ := os.ReadDir("migrations")
	for _, e := range entries {
		if mm := migrationName.FindStringSubmatch(e.Name()); mm != nil {
			if v, _ := strconv.Atoi(mm[1]); v > latest {
				latest = v
			}
		}
	}
	pool := freshAt(ctx, t, "ms_plugin_cap_checks", latest)
	checkLive(t, pool, "after the migrations")
	// A live row holding the newest capability: an older body re-applied by
	// RLSOn that re-adds a narrower CHECK fails on it, as well as narrowing.
	newest := string(plugins.KnownCapabilities[len(plugins.KnownCapabilities)-1])
	if _, err := pool.Exec(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities)
		VALUES (1, 'example-newest', 'Example', 'hash-newest', 'none', ARRAY[$1]::text[])`, newest); err != nil {
		t.Fatalf("a plugin with %s: %v", newest, err)
	}
	// db.RLSOff then db.RLSOn (a real rollback mode, and what api tests do to a
	// shared database) re-applies older migration bodies, 0141's among them,
	// which replaces plugins_capabilities_known: the lists must survive it.
	if err := db.RLSOff(ctx, pool); err != nil {
		t.Fatalf("RLSOff: %v", err)
	}
	if err := db.RLSOn(ctx, pool); err != nil {
		t.Fatalf("RLSOn: %v", err)
	}
	checkLive(t, pool, "after RLSOff and RLSOn")
}

// checkLive compares the live capability CHECKs with Go's lists.
func checkLive(t *testing.T, pool *pgxpool.Pool, when string) {
	t.Helper()
	ctx := context.Background()
	for constraint, want := range map[string][]string{
		"plugins_capabilities_known":              capNames(plugins.KnownCapabilities),
		"plugin_pair_requests_capabilities_check": capNames(plugins.PairableCapabilities),
	} {
		var def string
		if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, constraint).Scan(&def); err != nil {
			t.Fatalf("%s: %v", constraint, err)
		}
		if got := quotedValues(def); !slices.Equal(got, want) {
			t.Errorf("%s the live %s lists %v; Go lists %v (%s)", when, constraint, got, want, def)
		}
	}
}
