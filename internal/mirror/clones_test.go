package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// WHICH CLONE (clones.go). The paths are fictional and pairwise distinct; none
// is a substring of another, and none equals a constant the code names.
const (
	cloneLive  = "/srv/fixture-a/ledger-live"
	cloneOld   = "/srv/fixture-b/ledger-standalone"
	cloneThird = "/opt/fixture-c/ledger-mirror"
)

// TestChooseCloneTakesTheOnlyLiveClone is the operator's rule ("Prefer the
// live sessions' clone"): one clone is the hint; among several, the ONLY live
// one is — wherever it sits in the host's order; with none or several live
// there is no hint, and the choices are every clone, live ones first; no
// clones decide nothing.
func TestChooseCloneTakesTheOnlyLiveClone(t *testing.T) {
	c := func(p string, live bool) mirror.AgentClone { return mirror.AgentClone{Path: p, Live: live} }
	paths := func(cl []mirror.AgentClone) string {
		var out []string
		for _, x := range cl {
			out = append(out, x.Path)
		}
		return strings.Join(out, "|")
	}
	for _, tc := range []struct {
		name    string
		in      []mirror.AgentClone
		hint    string
		choices string
	}{
		{"none", nil, "", ""},
		{"one, not live", []mirror.AgentClone{c(cloneOld, false)}, cloneOld, ""},
		{"the live one second", []mirror.AgentClone{c(cloneOld, false), c(cloneLive, true)}, cloneLive, ""},
		{"the live one first", []mirror.AgentClone{c(cloneLive, true), c(cloneOld, false), c(cloneThird, false)}, cloneLive, ""},
		{"none live", []mirror.AgentClone{c(cloneOld, false), c(cloneThird, false)}, "", cloneOld + "|" + cloneThird},
		{"two live", []mirror.AgentClone{c(cloneOld, false), c(cloneThird, true), c(cloneLive, true)}, "", cloneThird + "|" + cloneLive + "|" + cloneOld},
	} {
		hint, choices := mirror.ChooseClone(tc.in)
		if hint != tc.hint || paths(choices) != tc.choices {
			t.Errorf("%s: hint %q choices %q; want %q %q", tc.name, hint, paths(choices), tc.hint, tc.choices)
		}
	}
}

// TestAHostsCloneListIsTakenWholeOrNotAtAll: one path muster cannot carry
// (relative, unclean, past MaxClonePath, a control character) — or more than
// MaxClones — refuses the WHOLE list, so muster never chooses among the part
// of it that fitted; a repeated path is kept once.
func TestAHostsCloneListIsTakenWholeOrNotAtAll(t *testing.T) {
	good := []mirror.AgentClone{{Path: cloneLive, Live: true}, {Path: cloneOld}, {Path: cloneLive}}
	got, err := mirror.NormalizeClones(good)
	if err != nil || len(got) != 2 || got[0] != (mirror.AgentClone{Path: cloneLive, Live: true}) || got[1].Path != cloneOld {
		t.Fatalf("a good list: %+v %v", got, err)
	}
	for _, bad := range []string{"srv/relative", "/srv/a/../b", "/srv/" + strings.Repeat("x", mirror.MaxClonePath), "/srv/a\x1bb", ""} {
		if _, err := mirror.NormalizeClones([]mirror.AgentClone{{Path: cloneLive, Live: true}, {Path: bad}}); !errors.Is(err, mirror.ErrClonesInvalid) {
			t.Errorf("a list with %q: %v; want ErrClonesInvalid", bad, err)
		}
	}
	var many []mirror.AgentClone
	for i := 0; i <= mirror.MaxClones; i++ {
		many = append(many, mirror.AgentClone{Path: "/srv/many/c" + strings.Repeat("y", i+1)})
	}
	if _, err := mirror.NormalizeClones(many); !errors.Is(err, mirror.ErrClonesInvalid) {
		t.Errorf("%d clones: %v; want ErrClonesInvalid", len(many), err)
	}
}

// TestTheHubAsksForClonesOnlyFromAHostThatListsThem: a host that advertised
// AgentSpawnClones beside a runtime is sent ONE agent-clones naming the
// repository, and its answer comes back as the clones; a refusal comes back
// as the host's code and sentence. A host that did not advertise it — an
// older muster-mirror — is sent nothing (ErrClonesUnsupported), and neither is
// one advertising it with no runtime it spawns.
func TestTheHubAsksForClonesOnlyFromAHostThatListsThem(t *testing.T) {
	r := newRig(t)
	connect := func(host string, runtimes []string, clones bool) *rawHost {
		t.Helper()
		tok, _ := r.store.mint(t, 1, host)
		h := hello(host, true, "shell")
		h.AgentSpawn, h.AgentSpawnClones = runtimes, clones
		raw, err := r.rawConnect(t, tok, h)
		if err != nil {
			t.Fatal(err)
		}
		r.online(t, 1, host, "shell")
		return raw
	}
	lists := connect("clone-box-1", []string{mirror.SpawnRuntimeClaude}, true)
	old := connect("older-box-2", []string{mirror.SpawnRuntimeClaude}, false)
	none := connect("idle-box-3", nil, true)
	if !hostOf(t, r, "clone-box-1").AgentSpawnClones || hostOf(t, r, "older-box-2").AgentSpawnClones || hostOf(t, r, "idle-box-3").AgentSpawnClones {
		t.Fatal("the hub's AgentSpawnClones is not the hello's beside a runtime")
	}
	answer := func(raw *rawHost, reply func(req mirror.Msg) mirror.Msg) {
		go func() {
			var req mirror.Msg
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				raw.mu.Lock()
				for _, m := range raw.text {
					if m.Type == mirror.TypeAgentClones {
						req = m
					}
				}
				raw.mu.Unlock()
				if req.Type != "" {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			b, _ := json.Marshal(reply(req))
			_ = raw.ws.Write(context.Background(), websocket.MessageText, b)
		}()
	}
	answer(lists, func(req mirror.Msg) mirror.Msg {
		if req.CloneRepo != "fixture-org/ledger" {
			return mirror.Msg{Type: mirror.TypeAgentClonesList, ID: req.ID, Refusal: mirror.SpawnFailed, Error: "wrong repo " + req.CloneRepo}
		}
		return mirror.Msg{Type: mirror.TypeAgentClonesList, ID: req.ID, Clones: []mirror.AgentClone{{Path: cloneOld}, {Path: cloneLive, Live: true}}}
	})
	got, err := r.svc.AgentClones(asUser(1), "clone-box-1", "fixture-org/ledger")
	if err != nil || len(got) != 2 || got[0] != (mirror.AgentClone{Path: cloneOld}) || got[1] != (mirror.AgentClone{Path: cloneLive, Live: true}) {
		t.Fatalf("the clones: %+v %v", got, err)
	}
	lists.mu.Lock()
	lists.text = nil // the next answer is to the next request only
	lists.mu.Unlock()
	answer(lists, func(req mirror.Msg) mirror.Msg {
		return mirror.Msg{Type: mirror.TypeAgentClonesList, ID: req.ID, Refusal: mirror.SpawnNoRepo, Error: "repo fixture-org/other is not on this host's allow-list"}
	})
	_, err = r.svc.AgentClones(asUser(1), "clone-box-1", "fixture-org/other")
	var ref *mirror.ClonesRefusedError
	if !errors.As(err, &ref) || ref.Code != mirror.SpawnNoRepo || ref.Detail != "repo fixture-org/other is not on this host's allow-list" {
		t.Fatalf("a refusal: %v", err)
	}
	for _, h := range []struct {
		name string
		raw  *rawHost
	}{{"older-box-2", old}, {"idle-box-3", none}} {
		if _, err := r.svc.AgentClones(asUser(1), h.name, "fixture-org/ledger"); !errors.Is(err, mirror.ErrClonesUnsupported) {
			t.Errorf("%s: %v; want ErrClonesUnsupported", h.name, err)
		}
		h.raw.mu.Lock()
		sent := slices.ContainsFunc(h.raw.text, func(m mirror.Msg) bool { return m.Type == mirror.TypeAgentClones })
		h.raw.mu.Unlock()
		if sent {
			t.Errorf("%s was sent an agent-clones", h.name)
		}
	}
}
