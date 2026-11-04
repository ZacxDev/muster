package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/podagent"
)

// ---------------------------------------------------------------------------
// A workspace file as an ask image: the instance's pod (`ccd ask-image`,
// podagent.UploadAskImage) streams it to POST /agent/officer/ask-images with
// the agent's own token; muster checks the bytes and forwards them to HQ with
// the officer credential of THAT agent's officer; officer_ask_person cites the
// id in image_ids. And officer_items' status filter takes `waiting`.
// ---------------------------------------------------------------------------

// GetByHooksToken resolves an agent by its own token (the agent routes' door).
func (s *officerAgents) GetByHooksToken(_ context.Context, tok string) (agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.rows {
		if tok != "" && a.HooksToken == tok {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("no such agent")
}

// setHooksToken gives agent id its own token.
func (r *officerRig) setHooksToken(id int64, tok string) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	for i := range r.store.rows {
		if r.store.rows[i].ID == id {
			r.store.rows[i].HooksToken = tok
		}
	}
}

// The agents' own tokens: pairwise distinct, and distinct from the rig's
// shared hook token.
const (
	askTokA   = "agent-own-token-alpha-1-4be2"
	askTokB   = "agent-own-token-beta-1-91c7"
	askTokOrd = "agent-own-token-ordinary-30d8"
)

// uploadRig is an officer rig whose server listens (the pod reaches it over
// HTTP), with officer A's and B's instance 1 and the ordinary agent each
// holding its own token.
func uploadRig(t *testing.T) (*officerRig, *httptest.Server, agents.Agent, agents.Agent) {
	t.Helper()
	r := newOfficerRig(t)
	instA := r.addInstance(r.a, 1, 921)
	instB := r.addInstance(r.b, 1, 922)
	r.setHooksToken(instA.ID, askTokA)
	r.setHooksToken(instB.ID, askTokB)
	r.setHooksToken(officerOrdinaryID, askTokOrd)
	r.hq.AddMember("design-lead")
	srv := httptest.NewServer(r.h)
	t.Cleanup(srv.Close)
	return r, srv, instA, instB
}

// postAskImage POSTs body to the upload route as the agent holding tok.
func postAskImage(t *testing.T, base, query, tok string, body []byte) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/agent/officer/ask-images"+query, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Muster-Token", tok)
	req.Header.Set("Content-Type", "image/png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestAWorkspaceImageIsUploadedFromThePodAndCitedByTheAsk: end to end. A real
// PNG in a temp workspace goes through podagent.UploadAskImage, muster's route
// and HQ's ask-images; HQ receives its exact bytes, under officer B's path and
// token, typed image/png and named by its base name. The id comes back, and an
// ask citing it in image_ids sends HQ exactly that id.
func TestAWorkspaceImageIsUploadedFromThePodAndCitedByTheAsk(t *testing.T) {
	r, srv, _, instB := uploadRig(t)
	ctx := context.Background()
	root := t.TempDir()
	png := askPNGOf(70_001)
	if err := os.MkdirAll(filepath.Join(root, "shots"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "shots", "header-v2.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	item := r.hq.AddItem(offB, "triage.look", "svc:review")
	if out := r.s.dispatchAgentTool(ctx, instB, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatal(out)
	}
	r.hq.ResetRequests()

	up, err := podagent.UploadAskImage(ctx, nil, srv.URL, askTokB, root, "shots/header-v2.png")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d HQ requests, want the one upload", len(reqs))
	}
	q := reqs[0]
	if q.Path != "/api/v1/officers/"+offB+"/ask-images" || q.Query != "filename=header-v2.png" || q.ContentType != "image/png" || q.Auth != "Bearer "+offBTok {
		t.Errorf("HQ upload: path %s query %q type %q auth %q", q.Path, q.Query, q.ContentType, q.Auth)
	}
	imgs := r.hq.AskImages()
	if len(imgs) != 1 || !bytes.Equal(imgs[0].Data, png) || imgs[0].Officer != offB || up.ID != imgs[0].ID || up.Size != int64(len(png)) {
		t.Fatalf("HQ stored %d images; the upload answered %+v", len(imgs), up)
	}

	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, instB, officers.ToolAskPerson, askArgs(t, map[string]any{
		"item_id": item, "to": "design-lead", "question": "Is the new header right?", "image_ids": []string{up.ID}}))
	if isToolErr(out) {
		t.Fatalf("ask citing the upload: %s", out)
	}
	reqs = r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("ask: %d HQ requests, want only the create (the image is already in HQ)", len(reqs))
	}
	if got, want := normJSON(t, reqs[0].Body), normJSON(t, map[string]any{"instance": instB.Name, "to": "design-lead", "question": "Is the new header right?", "images": []any{up.ID}}); got != want {
		t.Fatalf("create body:\n%s\nwant\n%s", got, want)
	}
}

// TestAnUploadActsForTheCallingAgentsOwnOfficerOnly: the officer is decided by
// the token. Officer A's instance lands under A's path with A's token, never
// B's; an ordinary agent is refused 403; naming another officer in the query
// is refused 403; and none of the refusals reaches HQ.
func TestAnUploadActsForTheCallingAgentsOwnOfficerOnly(t *testing.T) {
	r, srv, _, _ := uploadRig(t)
	png := askPNGOf(1_234)

	r.hq.ResetRequests()
	if code, out := postAskImage(t, srv.URL, "?filename=a.png", askTokA, png); code != http.StatusCreated {
		t.Fatalf("officer A's instance: %d %v", code, out)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/api/v1/officers/"+offA+"/ask-images" || reqs[0].Auth != "Bearer "+offATok {
		t.Fatalf("A's upload reached HQ as %+v, want A's path with A's token", reqs)
	}

	for _, tc := range []struct {
		name, query, tok string
	}{
		{"an ordinary agent", "?filename=a.png", askTokOrd},
		{"B's instance naming officer A", "?filename=a.png&officer=" + offA, askTokB},
		{"B's instance naming officer A by officer_id", "?filename=a.png&officer_id=" + offA, askTokB},
	} {
		r.hq.ResetRequests()
		if code, out := postAskImage(t, srv.URL, tc.query, tc.tok, png); code != http.StatusForbidden {
			t.Errorf("%s: %d %v, want 403", tc.name, code, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s reached HQ (%d requests)", tc.name, n)
		}
	}
	// A wrong token, and none, are refused before any officer is looked at.
	for _, tok := range []string{"", "agent-own-token-nobody-0000"} {
		if code, _ := postAskImage(t, srv.URL, "?filename=a.png", tok, png); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d, want 401", tok, code)
		}
	}
	// Positive control for the query check: naming its OWN officer is fine.
	r.hq.ResetRequests()
	if code, out := postAskImage(t, srv.URL, "?filename=b.png&officer="+offB, askTokB, png); code != http.StatusCreated || len(r.hq.Requests()) != 1 {
		t.Fatalf("B naming itself: %d %v", code, out)
	}
}

// TestTheUploadRouteChecksTheBytesWhateverSentThem: posted straight to the
// route (anything in the pod holds the token), exactly 5 MiB of PNG is
// accepted and 5 MiB + 1 refused 413; text, even sent as image/png, is
// refused 415; neither refusal reaches HQ.
func TestTheUploadRouteChecksTheBytesWhateverSentThem(t *testing.T) {
	r, srv, _, _ := uploadRig(t)
	for _, tc := range []struct {
		name string
		body []byte
		code int
		hq   int
	}{
		{"exactly 5 MiB", askPNGOf(5 << 20), http.StatusCreated, 1},
		{"5 MiB + 1 byte", askPNGOf(5<<20 + 1), http.StatusRequestEntityTooLarge, 0},
		{"text sent as image/png", []byte("a page of notes, not a picture"), http.StatusUnsupportedMediaType, 0},
	} {
		r.hq.ResetRequests()
		code, out := postAskImage(t, srv.URL, "?filename=x.png", askTokB, tc.body)
		if code != tc.code {
			t.Errorf("%s: %d %v, want %d", tc.name, code, out, tc.code)
		}
		if n := len(r.hq.Requests()); n != tc.hq {
			t.Errorf("%s: %d HQ requests, want %d", tc.name, n, tc.hq)
		}
	}
	if imgs := r.hq.AskImages(); len(imgs) != 1 || len(imgs[0].Data) != 5<<20 {
		t.Fatalf("HQ stored %d images, want the one 5 MiB image", len(imgs))
	}
}

// TestOfficerItemsFiltersOnWaiting: `waiting` is one of officer_items'
// statuses in its schema, and is passed to HQ as ?status=waiting, which lists
// the item an ask left waiting. (Which statuses HQ accepts is HQ's to decide;
// muster passes the filter through.)
func TestOfficerItemsFiltersOnWaiting(t *testing.T) {
	r, inst, item := askRig(t)
	ctx := context.Background()
	var enum []string
	for _, d := range queueToolDefs() {
		if d.Name == officers.ToolItems {
			props := d.Parameters["properties"].(map[string]any)
			enum, _ = props["status"].(map[string]any)["enum"].([]string)
		}
	}
	if strings.Join(enum, ",") != "open,claimed,waiting,done,escalated" {
		t.Errorf("officer_items' status enum = %v, want open,claimed,waiting,done,escalated", enum)
	}
	if out := r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, `{"item_id":"`+item+`","to":"design-lead","question":"q"}`); isToolErr(out) {
		t.Fatal(out)
	}
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, inst, officers.ToolItems, `{"status":"waiting"}`)
	reqs := r.hq.Requests()
	if len(reqs) != 1 || reqs[0].Query != "status=waiting" {
		t.Fatalf("officer_items{waiting} sent %+v, want one request with status=waiting", reqs)
	}
	if isToolErr(out) || !strings.Contains(out, `"id":"`+item+`"`) || !strings.Contains(out, `"status":"waiting"`) {
		t.Fatalf("officer_items{waiting}: %s, want the waiting item", out)
	}
}
