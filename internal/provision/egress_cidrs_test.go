package provision

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestValidateRefusesEgressCIDRsThatConfineNothingOrReachInside: from a valid
// isolated spec by ONE change each, every refusal by its own words.
func TestValidateRefusesEgressCIDRsThatConfineNothingOrReachInside(t *testing.T) {
	ok := fingerprintFixture()
	ok.Network = Network{Isolate: true, EgressCIDRs: []CIDRPort{{"160.79.104.0/23", 443}, {"160.79.104.0/23", 8443}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("control: %v", err)
	}
	for name, c := range map[string]struct {
		cidrs   []CIDRPort
		isolate bool
		want    string
	}{
		"without isolate": {[]CIDRPort{{"160.79.104.0/23", 443}}, false, "egress CIDRs [160.79.104.0/23:443] without Isolate"},
		"twice":           {[]CIDRPort{{"160.79.104.0/23", 443}, {"160.79.104.0/23", 443}}, true, "egress CIDR 160.79.104.0/23:443 appears twice"},
		"port zero":       {[]CIDRPort{{"160.79.104.0/23", 0}}, true, "port 0 out of range"},
		"port too big":    {[]CIDRPort{{"160.79.104.0/23", 65536}}, true, "port 65536 out of range"},
		"not a cidr":      {[]CIDRPort{{"160.79.104.0", 443}}, true, `"160.79.104.0" is not a CIDR block`},
		"ipv6":            {[]CIDRPort{{"2607:6bc0::/48", 443}}, true, `"2607:6bc0::/48" is not an IPv4 block`},
		"host bits":       {[]CIDRPort{{"160.79.104.10/23", 443}}, true, "has host bits set; write the block itself, 160.79.104.0/23"},
		"too wide":        {[]CIDRPort{{"160.0.0.0/15", 443}}, true, `"160.0.0.0/15" is wider than a /16`},
		"everything":      {[]CIDRPort{{"0.0.0.0/0", 443}}, true, `"0.0.0.0/0" is wider than a /16`},
		"rfc1918 inside":  {[]CIDRPort{{"10.43.0.0/16", 443}}, true, "overlaps the non-public range 10.0.0.0/8"},
		"172.16/12":       {[]CIDRPort{{"172.20.0.0/16", 443}}, true, "overlaps the non-public range 172.16.0.0/12"},
		"192.168/16":      {[]CIDRPort{{"192.168.50.0/24", 443}}, true, "overlaps the non-public range 192.168.0.0/16"},
		"link-local":      {[]CIDRPort{{"169.254.169.254/32", 80}}, true, "overlaps the non-public range 169.254.0.0/16"},
		"cgnat":           {[]CIDRPort{{"100.100.0.0/16", 443}}, true, "overlaps the non-public range 100.64.0.0/10"},
		"loopback":        {[]CIDRPort{{"127.0.0.0/16", 443}}, true, "overlaps the non-public range 127.0.0.0/8"},
		"this network":    {[]CIDRPort{{"0.0.0.0/16", 443}}, true, "overlaps the non-public range 0.0.0.0/8"},
		"multicast":       {[]CIDRPort{{"239.1.0.0/16", 443}}, true, "overlaps the non-public range 224.0.0.0/4"},
		"reserved":        {[]CIDRPort{{"255.255.255.255/32", 443}}, true, "overlaps the non-public range 240.0.0.0/4"},
	} {
		s := fingerprintFixture()
		s.Network = Network{Isolate: c.isolate, EgressCIDRs: c.cidrs}
		err := s.Validate()
		if !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want ErrInvalidSpec containing %q", name, err, c.want)
		}
	}
}

// TestTheWidestAcceptedBlockIsASlash16: the bound's boundary, both sides of it,
// on a public address no other check refuses.
func TestTheWidestAcceptedBlockIsASlash16(t *testing.T) {
	if err := CheckEgressCIDR(CIDRPort{"160.79.0.0/16", 443}); err != nil {
		t.Errorf("a public /16 is refused: %v", err)
	}
	if err := CheckEgressCIDR(CIDRPort{"160.78.0.0/15", 443}); err == nil || !strings.Contains(err.Error(), "wider than a /16") {
		t.Errorf("a public /15 is not refused as too wide: %v", err)
	}
	if err := CheckEgressCIDR(CIDRPort{"160.79.104.10/32", 65535}); err != nil {
		t.Errorf("a single public address on the top port is refused: %v", err)
	}
}

// TestEgressCIDRsMoveTheFingerprintOnlyWhenDeclared: an isolated spec without
// blocks keeps its pinned fingerprint; declaring one moves it, different
// blocks (or ports) differ, and order does not matter.
func TestEgressCIDRsMoveTheFingerprintOnlyWhenDeclared(t *testing.T) {
	with := func(c ...CIDRPort) string {
		s := fingerprintFixture()
		s.Network = Network{Isolate: true, PublicEgressTCPPorts: []int{443}, EgressCIDRs: c}
		return Fingerprint(s)
	}
	base := with()
	if base != pinnedIsolatedFingerprint {
		t.Fatalf("an isolated spec with no egress CIDRs changed fingerprint: %s", base)
	}
	a := with(CIDRPort{"160.79.104.0/23", 443})
	b := with(CIDRPort{"153.61.224.0/22", 443})
	p := with(CIDRPort{"160.79.104.0/23", 8443})
	if a == base || b == base || p == base || a == b || a == p {
		t.Fatalf("egress CIDRs do not move the fingerprint distinctly: base=%s a=%s b=%s p=%s", base, a, b, p)
	}
	if with(CIDRPort{"160.79.104.0/23", 443}, CIDRPort{"153.61.224.0/22", 443}) !=
		with(CIDRPort{"153.61.224.0/22", 443}, CIDRPort{"160.79.104.0/23", 443}) {
		t.Fatal("the order of the blocks moves the fingerprint")
	}
}

// TestANetworkWithoutEgressCIDRsMarshalsAsBefore: the field is omitted when
// empty, so every spec built before it existed marshals byte-identically, and
// a Network holding only blocks is not zero.
func TestANetworkWithoutEgressCIDRsMarshalsAsBefore(t *testing.T) {
	b, err := json.Marshal(Network{Isolate: true, PublicEgressTCPPorts: []int{443}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"Isolate":true,"PublicEgressTCPPorts":[443],"ControllerEgressTCPPorts":null}`; got != want {
		t.Errorf("marshal = %s, want %s", got, want)
	}
	if (Network{EgressCIDRs: []CIDRPort{{"160.79.104.0/23", 443}}}).IsZero() {
		t.Error("a Network declaring egress CIDRs reports IsZero")
	}
}
