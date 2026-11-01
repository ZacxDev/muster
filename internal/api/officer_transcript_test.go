package api

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE TRANSCRIPT FORWARDER, BOUND TO THE SERVER (officers.go): the seam
// between the officer tools / the wake loop and internal/officers/transcript.
// Each half is tested on its own there; this pins that officer_claim starts
// forwarding, officer_complete stops it, a wake pass resumes a held item, and
// the instance's transcript is read through the agent gateway BY ITS OWN
// AGENT ROW.
// ---------------------------------------------------------------------------

// transcriptGateway is the reprompt chat fake plus the gateway's transcript
// read, serving one line per read for whichever agent it is asked about.
type transcriptGateway struct {
	repromptChat
	tmu   sync.Mutex
	asked []string
	end   int64
	clock func() time.Time
}

func (g *transcriptGateway) Transcript(_ context.Context, a agents.Agent, _ string, after int64, _ int) (cctranscript.Page, error) {
	g.tmu.Lock()
	defer g.tmu.Unlock()
	g.asked = append(g.asked, a.Name)
	g.end += 100
	return cctranscript.Page{Session: "s-1", Next: g.end, Lines: []cctranscript.Line{{End: g.end, At: g.clock().UnixMilli(),
		Events: []cctranscript.Event{{Kind: cctranscript.KindText, Text: "working " + a.Name}}}}}, nil
}

func transcriptServer(t *testing.T, gw Gateway) (*Server, *officerAgents, *hqtest.Server, officers.Officer, error) {
	t.Helper()
	hq := hqtest.New(map[string]string{offATok: offA})
	t.Cleanup(hq.Close)
	store := &officerAgents{}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: gw, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	o := officers.Officer{ID: offA, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1,
		Transcript: &officers.Transcript{Enabled: true}}.WithToken(offATok)
	_, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession, NoTranscriptRun: true})
	if err == nil {
		a := agents.Agent{ID: 901, Name: officers.InstanceName(o.ID, 1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
		store.rows = append(store.rows, a)
		prov.phase[a.Name] = provision.PhaseRunning
	}
	return s, store, hq, o, err
}

func TestTheOfficerToolsAndTheWakeLoopDriveTheTranscriptForwarder(t *testing.T) {
	gw := &transcriptGateway{clock: time.Now}
	s, store, hq, o, err := transcriptServer(t, gw)
	if err != nil {
		t.Fatal(err)
	}
	inst, _ := store.GetByName(context.Background(), officers.InstanceName(o.ID, 1))
	fwd := s.officerRT.fwd
	if fwd == nil {
		t.Fatal("no forwarder for a transcript-enabled officer")
	}
	ctx := context.Background()
	item := hq.AddItem(offA, "release.evaluate", "svc:v1")

	if out := s.dispatchAgentTool(ctx, inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	if got := fwd.Active(); got[inst.Name] != officers.ItemID(item) {
		t.Fatalf("after officer_claim, forwarding %v", got)
	}
	fwd.Tick(ctx)
	if got := hq.Transcript(item); len(got) != 1 || got[0].Text != "working "+inst.Name || got[0].Instance != inst.Name {
		t.Fatalf("HQ stored %+v", got)
	}
	gw.tmu.Lock()
	asked := strings.Join(gw.asked, ",")
	gw.tmu.Unlock()
	if asked != inst.Name {
		t.Fatalf("the gateway was asked for %q, want the instance's own row %q", asked, inst.Name)
	}

	if out := s.dispatchAgentTool(ctx, inst, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"done","summary":"ok"}`); isToolErr(out) {
		t.Fatalf("complete: %s", out)
	}
	if got := fwd.Active(); len(got) != 0 {
		t.Fatalf("after officer_complete, still forwarding %v", got)
	}

	// A claim muster did not see (a restart): the wake pass resumes it.
	next := hq.AddItem(offA, "release.evaluate", "svc:v2")
	hq.SetClaim(next, inst.Name, time.Now().Add(10*time.Minute))
	s.officerRT.rec.Check(ctx, offA, true)
	if got := fwd.Active(); got[inst.Name] != officers.ItemID(next) {
		t.Fatalf("after a wake pass seeing the held item, forwarding %v", got)
	}
	// And a pass that no longer sees it held stops it.
	hq.Complete(next)
	time.Sleep(2 * time.Millisecond) // the pass reads after the resume's start
	s.officerRT.rec.Check(ctx, offA, true)
	if got := fwd.Active(); len(got) != 0 {
		t.Fatalf("after a pass with nothing held, forwarding %v", got)
	}
}

func TestATranscriptOfficerWithoutAGatewayTranscriptIsABootRefusal(t *testing.T) {
	_, _, _, _, err := transcriptServer(t, &repromptChat{})
	if err == nil || !strings.Contains(err.Error(), "transcript") {
		t.Fatalf("err = %v, want a refusal naming transcript forwarding", err)
	}
}
