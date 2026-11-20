package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/ui"
)

// readRecordingProv is a provisioner whose two reads are told apart: Instances
// (the whole fleet) is counted, and InstancesOf records the agents it was
// asked about and answers from byName — or fails with fail.
type readRecordingProv struct {
	stubProvisioner
	mu     sync.Mutex
	fleet  int
	asked  [][]string
	byName map[string]provision.Instance
	fail   error
}

func (p *readRecordingProv) Instances(context.Context) ([]provision.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fleet++
	out := make([]provision.Instance, 0, len(p.byName))
	for _, in := range p.byName {
		out = append(out, in)
	}
	return out, p.fail
}

func (p *readRecordingProv) InstancesOf(_ context.Context, ags []agents.Agent) ([]provision.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(ags))
	var out []provision.Instance
	for _, a := range ags {
		names = append(names, a.Name)
		if in, ok := p.byName[a.Name]; ok {
			out = append(out, in)
		}
	}
	p.asked = append(p.asked, names)
	if p.fail != nil {
		return nil, p.fail
	}
	return out, nil
}

// TestTheStatusIconReadsOnlyTheAgentItShows: the agent page's status icon
// (GET /ui/agents/{id}/status, polled every 10s) reads the live instance of
// the ONE agent it draws — one InstancesOf naming that agent per poll, and no
// whole-fleet Instances read — and still draws the live state, not the stored
// one: a stored-stopped agent whose instance runs reads running, a running
// row with no instance reads stopped, a pending instance reads provisioning,
// and a failed read falls back to the stored status.
//
// RED before the fix: the poll read the whole fleet (Provisioner.Instances)
// to look one agent up — on the Kubernetes driver three API requests per
// agent namespace, per poll, per open page.
func TestTheStatusIconReadsOnlyTheAgentItShows(t *testing.T) {
	// Each row's stored status differs from the status its live instance
	// gives, so the icon can only be right if the narrow read drove it.
	rows := map[string]agents.Agent{
		"tok-run":  {ID: 21, Name: "live-otter", Status: agents.StatusStopped, Namespace: "ns-a", KickedOff: true},
		"tok-gone": {ID: 22, Name: "gone-otter", Status: agents.StatusRunning, Namespace: "ns-b", KickedOff: true},
		"tok-pend": {ID: 23, Name: "slow-otter", Status: agents.StatusStopped, Namespace: "ns-c", KickedOff: true},
	}
	prov := &readRecordingProv{byName: map[string]provision.Instance{
		"live-otter":  {Ref: provision.Ref{Name: "live-otter"}, Phase: provision.PhaseRunning, Ready: true},
		"slow-otter":  {Ref: provision.Ref{Name: "slow-otter"}, Phase: provision.PhasePending},
		"other-heron": {Ref: provision.Ref{Name: "other-heron"}, Phase: provision.PhaseRunning, Ready: true},
	}}
	s := New(nil, AuthConfig{HookToken: relayHookToken, ServiceToken: relayServiceToken, UIPassword: relayPassword},
		log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: relayAgents{byToken: rows}, Provisioner: prov})
	h := s.Handler()

	poll := func(id int64) string {
		t.Helper()
		// The path the agent page's icon polls (ui.StatusPollPath).
		path := ui.StatusPollPath(id, false, mirror.WindowWork{})
		if want := "/ui/agents/" + strconv.FormatInt(id, 10) + "/status?"; !strings.HasPrefix(path, want) {
			t.Fatalf("the icon polls %q; want the one agent's %q", path, want)
		}
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Cookie", sessionCookieFor(s))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status poll %d: %d %s", id, w.Code, w.Body)
		}
		return w.Body.String()
	}

	for i, c := range []struct {
		id         int64
		name, want string
	}{
		{21, "live-otter", "running"},
		{22, "gone-otter", "stopped"},
		{23, "slow-otter", "provisioning"},
	} {
		body := poll(c.id)
		if label := `aria-label="Status: ` + c.want + `"`; !strings.Contains(body, label) {
			t.Errorf("agent %d's icon: %s; want %s", c.id, body, label)
		}
		prov.mu.Lock()
		fleet, asked := prov.fleet, append([][]string(nil), prov.asked...)
		prov.mu.Unlock()
		if fleet != 0 {
			t.Fatalf("after %d poll(s) the whole fleet was read %d time(s); want 0 (the icon shows one agent)", i+1, fleet)
		}
		if len(asked) != i+1 || strings.Join(asked[i], ",") != c.name {
			t.Fatalf("after %d poll(s) the narrow reads were %v; want one per poll, the last naming only %q", i+1, asked, c.name)
		}
	}

	// A failed read is "not observed": the stored status, never "stopped".
	prov.mu.Lock()
	prov.fail = errors.New("backend unreachable")
	prov.mu.Unlock()
	if body := poll(22); !strings.Contains(body, `aria-label="Status: running"`) {
		t.Errorf("a failed read drew %s; want the stored status (running)", body)
	}
}
