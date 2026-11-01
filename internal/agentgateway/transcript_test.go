package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// TestTranscriptReadsTheAgentsTranscriptWithItsTurnBearer: the GET carries
// the agent's own turn bearer and the query ccd's route takes, and the page
// decodes; a runtime without the route is ErrTranscriptUnsupported.
func TestTranscriptReadsTheAgentsTranscriptWithItsTurnBearer(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery, gotPath = r.Header.Get("Authorization"), r.URL.RawQuery, r.URL.Path
		_ = json.NewEncoder(w).Encode(cctranscript.Page{Session: "s-9", Next: 512,
			Lines: []cctranscript.Line{{End: 512, At: 1700000000000, Events: []cctranscript.Event{{Kind: cctranscript.KindText, Text: "hi"}}}}})
	}))
	defer srv.Close()
	gw, err := New(Config{Driver: &fixedResolver{ep: endpointOf(t, srv.URL)}, Runtime: HooksSHA256(), Model: testSentinel, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	a := fixtureAgent()
	p, err := gw.Transcript(context.Background(), a, "s-8", 256, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/transcript" || gotQuery != "after=256&backfill=4096&session=s-8" {
		t.Fatalf("GET %s?%s", gotPath, gotQuery)
	}
	if gotAuth != "Bearer "+HooksSHA256().Bearer(a.HooksToken) {
		t.Fatalf("authorization %q is not the agent's turn bearer", gotAuth)
	}
	if p.Session != "s-9" || p.Next != 512 || len(p.Lines) != 1 || p.Lines[0].Events[0].Text != "hi" || p.Lines[0].At != 1700000000000 {
		t.Fatalf("page %+v", p)
	}

	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	gw, _ = New(Config{Driver: &fixedResolver{ep: endpointOf(t, old.URL)}, Runtime: HooksSHA256(), Model: testSentinel, Client: old.Client()})
	if _, err := gw.Transcript(context.Background(), a, "", -1, 0); !errors.Is(err, ErrTranscriptUnsupported) {
		t.Fatalf("an image without the route: %v, want ErrTranscriptUnsupported", err)
	}
}
