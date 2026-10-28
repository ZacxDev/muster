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
)

// doctorServer builds a server for GET /api/doctor with the given auth and
// extensions.
func doctorServer(t *testing.T, cfg AuthConfig, ext Extensions, schema SchemaCheck) *httptest.Server {
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
	if schema != nil {
		s.SetSchemaCheck(schema)
	}
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
	unarmed := doctorServer(t, AuthConfig{UIPassword: testUIPassword}, Extensions{}, nil)
	code, raw, _ := getDoctor(t, unarmed, "anything")
	if code != http.StatusServiceUnavailable || raw[HookUnarmedField] != true {
		t.Fatalf("unarmed: %d %v, want 503 with %s=true", code, raw, HookUnarmedField)
	}

	armed := doctorServer(t, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, Extensions{}, nil)
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
	cfg := AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, PublicURL: "https://muster.example.test"}

	off := doctorServer(t, cfg, Extensions{}, nil)
	code, raw, rep := getDoctor(t, off, testHookToken)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if rep.Push || rep.Dictation || rep.Mirror.Enabled || rep.Schema != nil {
		t.Fatalf("an empty server reports %+v, want push/dictation/mirror off and no schema", rep)
	}
	if raw["schema"] != nil {
		t.Fatalf("schema = %v, want JSON null for a server with no database", raw["schema"])
	}
	if hosts, ok := raw["mirror"].(map[string]any)["hosts"].([]any); !ok || len(hosts) != 0 {
		t.Fatalf("mirror.hosts = %#v, want [] (not null)", raw["mirror"])
	}
	if !rep.HookTokenArmed {
		t.Fatalf("an armed server reports hook_token_armed=false")
	}
	if rep.AuthMode != "password" || rep.PublicURL != "https://muster.example.test" || rep.Version != BuildVersion {
		t.Fatalf("identity fields = %q %q %q", rep.AuthMode, rep.PublicURL, rep.Version)
	}

	fm := &fakeMirror{online: true, interactive: true, liveHosts: []string{"never-came", "ws"}}
	on := doctorServer(t, cfg, Extensions{Push: stubPush{}, Transcriber: stubTranscriber{}, Mirror: fm},
		func(context.Context) (int, int, error) { return 41, 42, nil })
	_, _, rep = getDoctor(t, on, testHookToken)
	if !rep.Push || !rep.Dictation || !rep.Mirror.Enabled {
		t.Fatalf("a configured server reports %+v, want push, dictation and mirror on", rep)
	}
	if rep.Schema == nil || rep.Schema.Applied != 41 || rep.Schema.Latest != 42 || rep.Schema.Current {
		t.Fatalf("schema = %+v, want 41 of 42, not current", rep.Schema)
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
	gone := doctorServer(t, cfg, Extensions{Mirror: fm2},
		func(context.Context) (int, int, error) { return 42, 42, nil })
	_, _, rep = getDoctor(t, gone, testHookToken)
	if !reflect.DeepEqual(rep.Mirror.Hosts, []DoctorHost{{Host: "ws"}}) || rep.Schema == nil || !rep.Schema.Current {
		t.Fatalf("revoked+offline: hosts %+v schema %+v", rep.Mirror.Hosts, rep.Schema)
	}
}

// TestDoctorNamesAFailedReadWithoutLeakingIt: a schema or token read that
// fails is reported as an error string, never as "current" or "no hosts".
func TestDoctorNamesAFailedReadWithoutLeakingIt(t *testing.T) {
	cfg := AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}
	fm := &fakeMirror{liveErr: errors.New("pq: secret dsn detail")}
	ts := doctorServer(t, cfg, Extensions{Mirror: fm},
		func(context.Context) (int, int, error) { return 0, 0, errors.New("pq: secret dsn detail") })
	_, raw, rep := getDoctor(t, ts, testHookToken)
	if rep.Schema != nil || rep.SchemaError == "" || rep.Mirror.HostsError == "" {
		t.Fatalf("failed reads report %+v, want schema null with schema_error and hosts_error set", rep)
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
