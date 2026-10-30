package provision_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

func hostsSpec(hosts ...string) provision.Spec {
	s := provisiontest.MinimalSpec("named")
	s.Network = provision.Network{Isolate: true, PublicEgressTCPPorts: []int{443}, PublicEgressHosts: hosts}
	return s
}

// TestEveryEgressHostsRefusalIsReachedOnItsOwn: each Validate refusal of
// PublicEgressHosts from the valid spec by one mutation, by its own words.
func TestEveryEgressHostsRefusalIsReachedOnItsOwn(t *testing.T) {
	if err := hostsSpec("api.example.test", "git.example.test").Validate(); err != nil {
		t.Fatalf("control: a valid spec is refused: %v", err)
	}
	for _, c := range []struct {
		name string
		mut  func(*provision.Spec)
		want string
	}{
		{"no isolate", func(s *provision.Spec) { s.Network.Isolate = false; s.Network.PublicEgressTCPPorts = nil }, "without Isolate"},
		{"no port", func(s *provision.Spec) { s.Network.PublicEgressTCPPorts = nil }, "no public egress port"},
		{"no dns", func(s *provision.Spec) { s.Network.NoClusterDNS = true }, "could not be resolved"},
		{"wildcard", func(s *provision.Spec) { s.Network.PublicEgressHosts = []string{"*.example.test"} }, "not an exact"},
		{"upper case", func(s *provision.Spec) { s.Network.PublicEgressHosts = []string{"API.example.test"} }, "not an exact"},
		{"port", func(s *provision.Spec) { s.Network.PublicEgressHosts = []string{"api.example.test:443"} }, "not an exact"},
		{"single label", func(s *provision.Spec) { s.Network.PublicEgressHosts = []string{"localhost"} }, "not an exact"},
		{"twice", func(s *provision.Spec) { s.Network.PublicEgressHosts = []string{"a.example.test", "a.example.test"} }, "appears twice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := hostsSpec("api.example.test")
			c.mut(&s)
			err := s.Validate()
			if !errors.Is(err, provision.ErrInvalidSpec) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want ErrInvalidSpec containing %q", err, c.want)
			}
		})
	}
}

// TestADriverThatCannotFilterByNameRefusesHosts: CheckSpec refuses a spec naming
// hosts without NetworkEgressByName, and accepts it with; a spec naming none
// passes either way.
func TestADriverThatCannotFilterByNameRefusesHosts(t *testing.T) {
	caps := provision.Capabilities{NetworkIsolation: true}
	err := provision.CheckSpec(caps, hostsSpec("api.example.test"))
	if !errors.Is(err, provision.ErrUnsupported) || !strings.Contains(err.Error(), "api.example.test") {
		t.Fatalf("err = %v, want ErrUnsupported naming the host", err)
	}
	caps.NetworkEgressByName = true
	if err := provision.CheckSpec(caps, hostsSpec("api.example.test")); err != nil {
		t.Fatalf("with the capability: %v", err)
	}
	plain := hostsSpec()
	if err := provision.CheckSpec(provision.Capabilities{NetworkIsolation: true}, plain); err != nil {
		t.Fatalf("a spec naming no host was refused: %v", err)
	}
}

// TestEgressHostsMoveTheFingerprintAndNothingElseDoes: a spec naming no hosts
// keeps the fingerprint it had before the field (pinned literal), and marshals
// without the field; naming hosts moves it; their order does not.
func TestEgressHostsMoveTheFingerprintAndNothingElseDoes(t *testing.T) {
	base := hostsSpec()
	// The fingerprint of this spec before PublicEgressHosts existed, measured
	// with the code at 0f9d3822 (the base this field was added on).
	const before = "f46cf8a3a6127ae91e9142c965b94b28ae2a028f75f8ce68caea1f9d340fd623"
	fpBase := provision.Fingerprint(base)
	if fpBase != before {
		t.Errorf("a spec naming no hosts moved its fingerprint: %s, was %s (every running agent would roll)", fpBase, before)
	}
	b, _ := json.Marshal(base.Network)
	if strings.Contains(string(b), "PublicEgressHosts") {
		t.Errorf("a network naming no hosts marshals the field: %s", b)
	}
	ab := provision.Fingerprint(hostsSpec("a.example.test", "b.example.test"))
	ba := provision.Fingerprint(hostsSpec("b.example.test", "a.example.test"))
	if ab == fpBase {
		t.Error("naming hosts did not move the fingerprint")
	}
	if ab != ba {
		t.Error("the hosts' order moved the fingerprint")
	}
	if provision.Fingerprint(hostsSpec("a.example.test")) == ab {
		t.Error("a different host set has the same fingerprint")
	}
}
