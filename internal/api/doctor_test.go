package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// doctorServer builds a server for GET /api/doctor with the given auth and
// extensions.
func doctorServer(t *testing.T, cfg AuthConfig, ext Extensions) *httptest.Server {
	t.Helper()
	s := New(nil, cfg, log.New(io.Discard, "", 0))
	if ext.Notes == nil {
		ext.Notes = stubNotesStore{}
	}
	if ext.Agents == nil {
		ext.Agents = mirrorAgents{}
	}
	if ext.SessionLiveness == nil {
		ext.SessionLiveness = stubLiveness{}
	}
	s.UseExtensions(ext)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func getDoctor(t *testing.T, ts *httptest.Server, token string) (int, map[string]any, DoctorReport) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+DoctorPath, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	var rep DoctorReport
	_ = json.Unmarshal(body, &rep)
	return resp.StatusCode, raw, rep
}

// TestDoctorRefusesAnUnarmedServerByTheField: with no hook token the route
// answers 503 carrying HookUnarmedField — the discriminator the CLI reports
// "hook token: FAIL" from. The armed server is the control: the SAME request
// shape answers 200.
func TestDoctorRefusesAnUnarmedServerByTheField(t *testing.T) {
	unarmed := doctorServer(t, AuthConfig{UIPassword: testUIPassword}, Extensions{})
	code, raw, _ := getDoctor(t, unarmed, "anything")
	if code != http.StatusServiceUnavailable || raw[HookUnarmedField] != true {
		t.Fatalf("unarmed: %d %v, want 503 with %s=true", code, raw, HookUnarmedField)
	}

	armed := doctorServer(t, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, Extensions{})
	if code, _, _ := getDoctor(t, armed, testHookToken); code != http.StatusOK {
		t.Fatalf("armed with the right token: %d, want 200", code)
	}
	if code, _, _ := getDoctor(t, armed, "not-the-token"); code != http.StatusUnauthorized {
		t.Fatalf("armed with a wrong token: %d, want 401", code)
	}
	if code, _, _ := getDoctor(t, armed, ""); code != http.StatusUnauthorized {
		t.Fatalf("armed with no token: %d, want 401", code)
	}
}

// TestDoctorReportsWhatTheFeaturesRead: every boolean flips with the
// dependency the feature itself reads, in both directions, so no field is a
// constant that happens to match one fixture.
func TestDoctorReportsWhatTheFeaturesRead(t *testing.T) {
	cfg := AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}

	off := doctorServer(t, cfg, Extensions{})
	code, raw, rep := getDoctor(t, off, testHookToken)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if rep.Push || rep.Dictation || rep.Mirror.Enabled {
		t.Fatalf("an empty server reports %+v, want push/dictation/mirror off", rep)
	}
	if hosts, ok := raw["mirror"].(map[string]any)["hosts"].([]any); !ok || len(hosts) != 0 {
		t.Fatalf("mirror.hosts = %#v, want [] (not null)", raw["mirror"])
	}
	if !rep.HookTokenArmed {
		t.Fatalf("an armed server reports hook_token_armed=false")
	}
	if rep.AuthMode != "password" {
		t.Fatalf("auth_mode = %q", rep.AuthMode)
	}
	// The report carries only what a doctor check reads: no version (that is
	// /health's), no public URL (the client has its own).
	for _, gone := range []string{"version", "public_url", "schema"} {
		if _, ok := raw[gone]; ok {
			t.Errorf("the report still carries %q", gone)
		}
	}

	fm := &fakeMirror{online: true, interactive: true, liveHosts: []string{"never-came", "ws"}}
	on := doctorServer(t, cfg, Extensions{Push: stubPush{}, Transcriber: stubTranscriber{}, Mirror: fm})
	_, _, rep = getDoctor(t, on, testHookToken)
	if !rep.Push || !rep.Dictation || !rep.Mirror.Enabled {
		t.Fatalf("a configured server reports %+v, want push, dictation and mirror on", rep)
	}
	want := []DoctorHost{
		{Host: "never-came", Token: true},
		{Host: "ws", Token: true, Online: true, Interactive: true},
	}
	if !reflect.DeepEqual(rep.Mirror.Hosts, want) {
		t.Fatalf("hosts = %+v\nwant %+v", rep.Mirror.Hosts, want)
	}

	// The same host, disconnected and with its token revoked: still listed (the
	// hub remembers it), offline, no token.
	fm2 := &fakeMirror{online: false}
	gone := doctorServer(t, cfg, Extensions{Mirror: fm2})
	_, _, rep = getDoctor(t, gone, testHookToken)
	if !reflect.DeepEqual(rep.Mirror.Hosts, []DoctorHost{{Host: "ws"}}) {
		t.Fatalf("revoked+offline: hosts %+v", rep.Mirror.Hosts)
	}
}

// TestDoctorNamesAFailedReadWithoutLeakingIt: a token read that
// fails is reported as an error string, never as "current" or "no hosts".
func TestDoctorNamesAFailedReadWithoutLeakingIt(t *testing.T) {
	cfg := AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}
	fm := &fakeMirror{liveErr: errors.New("pq: secret dsn detail")}
	ts := doctorServer(t, cfg, Extensions{Mirror: fm})
	_, raw, rep := getDoctor(t, ts, testHookToken)
	if rep.Mirror.HostsError == "" {
		t.Fatalf("failed reads report %+v, want hosts_error set", rep)
	}
	b, _ := json.Marshal(raw)
	if strings.Contains(string(b), "secret dsn detail") {
		t.Fatalf("the report leaks the underlying error: %s", b)
	}
}

// TestDoctorStatesTheArmingItself: a personal token reaches the handler on an
// unarmed server too (requireArmedHookToken admits it first), so the report
// must say the server is unarmed rather than imply it by answering at all.
func TestDoctorStatesTheArmingItself(t *testing.T) {
	for _, c := range []struct {
		hook string
		want bool
	}{{"", false}, {testHookToken, true}} {
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: c.hook}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}})
		w := httptest.NewRecorder()
		s.handleDoctor(w, httptest.NewRequest(http.MethodGet, DoctorPath, nil))
		var rep DoctorReport
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		if rep.HookTokenArmed != c.want {
			t.Errorf("hook token %q: hook_token_armed = %v, want %v", c.hook, rep.HookTokenArmed, c.want)
		}
	}
}

// TestMirrorHostIsTheTokensOwnHost: GET /mirror/host answers for the host the
// presented mirror token was minted for, from the tunnel hub's view of THAT
// token's owner. A bad token is 401 (the doctor's FAIL); a host the hub holds
// no tunnel from is online=false; another owner's same-named host is not
// visible; with the mirror plugin off the route is 404.
func TestMirrorHostIsTheTokensOwnHost(t *testing.T) {
	toks := fakeMirrorTokens{
		mirrorTok('a'): mirror.Token{ID: 1, OwnerID: 1, Host: "ws"},
		mirrorTok('b'): mirror.Token{ID: 2, OwnerID: 1, Host: "laptop"},
		mirrorTok('c'): mirror.Token{ID: 3, OwnerID: 2, Host: "ws"},
		// A second machine of user 1 minted under the same name "ws".
		mirrorTok('d'): mirror.Token{ID: 4, OwnerID: 1, Host: "ws"},
	}
	cfg := AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}
	fm := &fakeMirror{online: true, interactive: true, hostTok: map[string]int64{"ws": 1}}
	ts := doctorServer(t, cfg, Extensions{Mirror: fm, MirrorTokens: toks})
	get := func(tok string) (int, MirrorHostReport) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+MirrorHostPath, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var rep MirrorHostReport
		_ = json.NewDecoder(resp.Body).Decode(&rep)
		return resp.StatusCode, rep
	}
	for _, c := range []struct {
		tok  string
		code int
		want MirrorHostReport
	}{
		{mirrorTok('a'), 200, MirrorHostReport{Host: "ws", Online: true, Interactive: true}},
		{mirrorTok('b'), 200, MirrorHostReport{Host: "laptop"}},
		{mirrorTok('c'), 200, MirrorHostReport{Host: "ws"}}, // user 2: user 1's "ws" is not theirs
		// Same owner, same name, another token: "ws" is connected, but not by it.
		{mirrorTok('d'), 200, MirrorHostReport{Host: "ws", OtherToken: true}},
		{mirrorTok('z'), 401, MirrorHostReport{}},
		{testHookToken, 401, MirrorHostReport{}}, // the hook token is not a mirror token
	} {
		code, rep := get(c.tok)
		if code != c.code || rep != c.want {
			t.Errorf("token %q: %d %+v, want %d %+v", c.tok[:6], code, rep, c.code, c.want)
		}
	}
	fm.mu.Lock()
	fm.online = false
	fm.mu.Unlock()
	if code, rep := get(mirrorTok('a')); code != 200 || rep.Online || rep.Interactive {
		t.Errorf("after the host drops: %d %+v, want online=false", code, rep)
	}

	off := doctorServer(t, cfg, Extensions{})
	req, _ := http.NewRequest(http.MethodGet, off.URL+MirrorHostPath, nil)
	req.Header.Set("Authorization", "Bearer "+mirrorTok('a'))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("mirror plugin off: %d, want 404", resp.StatusCode)
	}
}

// TestDoctorListsOnlyTheCallersHosts: the report's host list is built under
// the CALLER's principal — a second user (a personal token acts as its owner)
// gets an empty list while user 1 sees "ws". (The store's own row-level
// scoping is TestLiveTokenHostsAreTheCallersOnly, over Postgres.)
func TestDoctorListsOnlyTheCallersHosts(t *testing.T) {
	cfg := AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}
	s := New(nil, cfg, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{},
		Mirror: &fakeMirror{online: true, liveHosts: []string{"ws"}}})
	for _, c := range []struct {
		user  int64
		hosts int
	}{{1, 1}, {2, 0}} {
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: c.user, Role: authz.RoleMember, Via: authz.ViaToken})
		w := httptest.NewRecorder()
		s.handleDoctor(w, httptest.NewRequest(http.MethodGet, DoctorPath, nil).WithContext(ctx))
		var rep DoctorReport
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		if len(rep.Mirror.Hosts) != c.hosts {
			t.Errorf("user %d sees %+v, want %d host(s)", c.user, rep.Mirror.Hosts, c.hosts)
		}
	}
}
