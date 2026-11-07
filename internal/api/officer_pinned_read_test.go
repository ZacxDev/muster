package api

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// Two commits that differ, so a resolver that served the wrong item's pins, or
// a constant, could not produce the literal pins below.
const (
	pinnedShaWidget = "3f9c1e0a7b2d4c6e8f0a1b3c5d7e9f1a2b4c6d8e"
	pinnedShaDocs   = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
)

// newPinnedOfficerRig: officer A (`release:alpha-web`) pins its reads to the
// item it holds; officer B (`triage:beta-api`) reads the same way WITHOUT the
// pin. Both have a repository.
func newPinnedOfficerRig(t *testing.T) *officerRig {
	t.Helper()
	return newOfficerRigWith(t, map[string]string{offATok: offA, offBTok: offB}, func(hqURL string) []officers.Officer {
		raw := `[{"id":"` + offA + `","hq_url":"` + hqURL + `","repo":"example-org/widget","read_repos":["example-org/docs"],"pin_repo_reads_to_item":true},` +
			`{"id":"` + offB + `","hq_url":"` + hqURL + `","repo":"example-org/widget"}]`
		list, err := officers.Parse(raw, func(k string) string {
			return map[string]string{
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offA): offATok,
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offB): offBTok,
			}[k]
		}, "muster-agent-")
		if err != nil {
			t.Fatalf("parse the pinned rig's officers: %v", err)
		}
		return list
	})
}

// hostApp is a GitHubApp that only answers its API host (readPinOf's one
// question); every other method panics (the nil embedded interface).
type hostApp struct {
	GitHubApp
	host string
}

func (h hostApp) APIHost() string { return h.host }

// TestAPinnedOfficersCallerResolvesTheHeldItemsPins is pin_repo_reads_to_item
// from the configuration to the proxy's caller (callerOf → Caller.ReadPin):
//
//   - only the pinned officer's instance carries a ReadPin — not the unpinned
//     officer's, not a plain agent;
//   - it pins the `github` connector at the App's API host (GitHub's when
//     there is no App);
//   - its pins are the payload of the ONE item the instance holds a live claim
//     on, read from HQ with the officer's own token at the moment of the
//     request — and every other state refuses: no item held, an item held by
//     another instance, an item that pins nothing, HQ down;
//   - composed with the proxy's check, a read at the pin passes and one at
//     the OTHER repository's pin does not.
func TestAPinnedOfficersCallerResolvesTheHeldItemsPins(t *testing.T) {
	r := newPinnedOfficerRig(t)
	ctx := context.Background()
	pinned := r.addInstance(r.a, 1, 1801)
	unpinned := r.addInstance(r.b, 1, 1802)
	plain := agents.Agent{ID: officerOrdinaryID, Name: "brave-otter"}

	if p := r.s.callerOf(unpinned).ReadPin; p != nil {
		t.Fatalf("the unpinned officer's instance carries a ReadPin: %+v", p)
	}
	if p := r.s.callerOf(plain).ReadPin; p != nil {
		t.Fatalf("a plain agent carries a ReadPin: %+v", p)
	}
	pin := r.s.callerOf(pinned).ReadPin
	if pin == nil || pin.Connector != "github" || pin.APIHost != "api.github.com" || pin.Pins == nil {
		t.Fatalf("the pinned instance's ReadPin = %+v, want github at api.github.com with a resolver", pin)
	}

	refused := func(name, want string) {
		t.Helper()
		if pins, err := pin.Pins(ctx); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: pins %+v, err %v; want an error containing %q", name, pins, err, want)
		}
	}
	refused("no item held", "you hold no claimed item")

	item := r.hq.AddItem(offA, "anomaly.alert", "node-a")
	r.hq.SetPayload(item, `{"alertname":"x","replay":{"case":4,"repo_refs":[`+
		`{"repo":"Example-Org/Widget","sha":"`+strings.ToUpper(pinnedShaWidget)+`"},{"repo":"example-org/docs","sha":"`+pinnedShaDocs+`"}]}}`)
	r.hq.SetClaim(item, officers.InstanceName(offA, 2), r.now().Add(10*time.Minute))
	refused("the item is held by another instance", "you hold no claimed item")

	r.hq.SetClaim(item, pinned.Name, r.now().Add(10*time.Minute))
	got, err := pin.Pins(ctx)
	want := []connectors.RepoPin{{Repo: "example-org/widget", SHA: pinnedShaWidget}, {Repo: "example-org/docs", SHA: pinnedShaDocs}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("pins = %+v, %v; want %+v", got, err, want)
	}
	if err := connectors.CheckPinnedRead("GET", pin.APIHost, "api.github.com", "repos/example-org/widget/contents/rules.yaml", "ref="+pinnedShaWidget, got); err != nil {
		t.Errorf("a read at the held item's pin is refused: %v", err)
	}
	if err := connectors.CheckPinnedRead("GET", pin.APIHost, "api.github.com", "repos/example-org/widget/contents/rules.yaml", "ref="+pinnedShaDocs, got); !errors.Is(err, connectors.ErrPinnedRead) {
		t.Errorf("a read at the other repository's pin: %v, want refused", err)
	}

	r.hq.SetPayload(item, `{"alertname":"x","replay":{"case":4}}`)
	refused("the item pins nothing", "pins no repository")

	r.hq.SetDown(true)
	if _, err := pin.Pins(ctx); err == nil {
		t.Error("HQ down: pins resolved, want an error")
	}
	r.hq.SetDown(false)

	// With a GitHub App, the pin is at the App's own API host.
	r.s.ext.GitHubApp = hostApp{host: "api.fake.test:4431"}
	if p := r.s.callerOf(pinned).ReadPin; p == nil || p.APIHost != "api.fake.test:4431" {
		t.Fatalf("with an App, ReadPin = %+v, want the App's API host", p)
	}
}
