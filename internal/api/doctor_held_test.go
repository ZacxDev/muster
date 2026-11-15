package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// heldHereMirror is the fake hub whose HeldHere says whether the asking
// machine is a held team host connection (mirror.Service.HeldHere).
type heldHereMirror struct {
	*fakeMirror
	held bool
}

func (m *heldHereMirror) HeldHere(context.Context, string, *http.Request) bool {
	return m.held
}

// TestDoctorTellsAHeldMachineItIsHeld (#685 round 0): a team host's HELD
// second machine presents the live host's own token, so the token match alone
// reported it online. GET /mirror/host must say it is HELD and not online.
// The CONTROL is the live machine (HeldHere false): online, as before.
//
// Decoded as JSON keys, so it compiles at 528dc461, where it is red (online).
func TestDoctorTellsAHeldMachineItIsHeld(t *testing.T) {
	toks := fakeMirrorTokens{mirrorTok('a'): mirror.Token{ID: 1, OwnerID: 1, Host: "ws"}}
	cfg := AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}
	for _, c := range []struct {
		held             bool
		online, wantHeld bool
	}{
		{online: true},
		{held: true, wantHeld: true},
	} {
		fm := &heldHereMirror{fakeMirror: &fakeMirror{online: true, interactive: true, hostTok: map[string]int64{"ws": 1}}, held: c.held}
		ts := doctorServer(t, cfg, Extensions{Mirror: fm, MirrorTokens: toks})
		req, _ := http.NewRequest(http.MethodGet, ts.URL+MirrorHostPath, nil)
		req.Header.Set("Authorization", "Bearer "+mirrorTok('a'))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var rep map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&rep)
		resp.Body.Close()
		online, _ := rep["online"].(bool)
		held, _ := rep["held"].(bool)
		if resp.StatusCode != 200 || online != c.online || held != c.wantHeld {
			t.Errorf("a machine with HeldHere=%v: %d %v; want online=%v held=%v", c.held, resp.StatusCode, rep, c.online, c.wantHeld)
		}
	}
}
