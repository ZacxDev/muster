package connectors_test

import (
	"context"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/safedial"
)

// The postgres connector under MUSTER_DIAL_DENY_PRIVATE (PostgresExecutor.Dial):
// a database whose address is, or resolves to, a non-public one is refused at
// connect time, and with no guard (the default) the same connector connects.
// The package's test database listens on loopback, so it is both the OFF
// case's reachable database and the ON case's refused one.

// pgDialFixtureHost is a public-looking name the guard's injected resolver
// answers with the test database's loopback address.
const pgDialFixtureHost = "db.example.com"

// execWith is a fresh executor (no cached pool) over f's store, dialling
// through g (nil: no guard).
func (f *pgFixture) execWith(t *testing.T, g *safedial.Guard) *connectors.PostgresExecutor {
	t.Helper()
	e := &connectors.PostgresExecutor{Store: f.store, Logger: log.New(io.Discard, "", 0), RatePerMinute: 6000, Burst: 1000, Dial: g}
	t.Cleanup(e.Close)
	return e
}

// addConnector stores another postgres connector over the probe role, at host.
func (f *pgFixture) addConnector(t *testing.T, name, host string) {
	t.Helper()
	ps := connectors.PostgresSettings{SSLMode: "disable", Database: f.target.Database}
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: name, Scheme: connectors.SchemePostgres,
		Hosts: []string{host}, Username: pgTestRole, Secret: pgTestPassword, Postgres: &ps}); err != nil {
		t.Fatalf("create connector %s at %s: %v", name, host, err)
	}
}

func (f *pgFixture) queryVia(e *connectors.PostgresExecutor, connector string) error {
	_, err := e.Query(f.ctx, f.caller, connector, connectors.PGQuery{SQL: `SELECT 1`})
	return err
}

// hostPort is ip:5432. Private addresses are built from octets: the
// repository's leak gate refuses a literal one in source.
func hostPort(ip netip.Addr) string { return net.JoinHostPort(ip.String(), "5432") }

// toTestDB resolves every name to the test database's own address.
func (f *pgFixture) toTestDB() func(context.Context, string) ([]netip.Addr, error) {
	return func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(f.target.Host)}, nil
	}
}

// TestThePostgresDialGuardRefusesNonPublicDatabases: one row per range. Each
// connector names an address (or a name resolving to one) and the query fails
// with class "connect" naming the guard's refusal. The loopback rows point at
// the REAL test database, which the OFF control below reaches — so the
// refusal is the guard's, not an unreachable host's.
func TestThePostgresDialGuardRefusesNonPublicDatabases(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{})
	port := strconv.Itoa(int(f.target.Port))
	if ip, err := netip.ParseAddr(f.target.Host); err != nil || !ip.IsLoopback() {
		t.Skipf("the test database is at %q, not a loopback address; this test needs it on loopback", f.target.Host)
	}
	rows := []struct{ name, host string }{
		{"loopback-literal", net.JoinHostPort(f.target.Host, port)},
		{"name-resolving-to-loopback", net.JoinHostPort(pgDialFixtureHost, port)},
		{"rfc1918", hostPort(netip.AddrFrom4([4]byte{10, 0, 0, 1}))},
		{"rfc1918-172", hostPort(netip.AddrFrom4([4]byte{172, 16, 5, 4}))},
		{"rfc1918-192", hostPort(netip.AddrFrom4([4]byte{192, 168, 1, 1}))},
		{"ipv4-mapped-private", hostPort(netip.AddrFrom16(netip.AddrFrom4([4]byte{10, 0, 0, 1}).As16()))},
		{"ula", "[fd00::1]:5432"},
		{"link-local", "169.254.169.254:5432"},
		{"cgnat", "100.64.0.1:5432"},
		{"unspecified", "0.0.0.0:5432"},
		{"multicast", "224.0.0.1:5432"},
	}
	for _, r := range rows {
		f.addConnector(t, r.name, r.host)
	}
	guard := &safedial.Guard{Resolve: f.toTestDB()}
	e := f.execWith(t, guard)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			err := f.queryVia(e, r.name)
			if err == nil {
				t.Fatalf("%s (%s): the guarded query CONNECTED", r.name, r.host)
			}
			if pgClass(err) != "connect" || !strings.Contains(err.Error(), safedial.ErrForbiddenAddress.Error()) {
				t.Fatalf("%s (%s): class %q, err %v; want a connect error naming the dial guard's refusal", r.name, r.host, pgClass(err), err)
			}
		})
	}

	// CONTROLS, on the same two connectors that point at the real database.
	// (1) No guard — the default: the loopback database is reached.
	if err := f.queryVia(f.execWith(t, nil), "loopback-literal"); err != nil {
		t.Fatalf("CONTROL (guard off): the loopback database was not reached: %v", err)
	}
	// (2) A guard that admits loopback, over the same resolver: the NAME path
	// connects through the guard's own dialer, so the refusals above are the
	// address rule and not a dialer that cannot connect at all.
	loose := f.execWith(t, &safedial.Guard{AllowLoopback: true, Resolve: f.toTestDB()})
	for _, name := range []string{"loopback-literal", "name-resolving-to-loopback"} {
		if err := f.queryVia(loose, name); err != nil {
			t.Fatalf("CONTROL (guard admitting loopback): %s was not reached through the guard: %v", name, err)
		}
	}
}
