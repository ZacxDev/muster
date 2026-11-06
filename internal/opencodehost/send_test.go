package opencodehost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

func fakeClient(t *testing.T) (*opencodetest.Server, *Client) {
	t.Helper()
	srv := opencodetest.New(fixturePassword)
	t.Cleanup(srv.Close)
	tok := srv.Issue()
	cl, err := NewClient(srv.URL, func() string { return tok })
	if err != nil {
		t.Fatal(err)
	}
	return srv, cl
}

// TestAPromptIsAdmittedOnceUnderItsID: a prompt posts {id, text, delivery
// "queue"} and is admitted; the SAME id posted again — with other text — is
// answered with the first admission and adds no message, as the real server
// did (testdata/prompt-v2.0.32.json, prompt1-again); the same id on another
// session is ErrPromptConflict, an unknown session ErrSessionNotFound.
func TestAPromptIsAdmittedOnceUnderItsID(t *testing.T) {
	srv, cl := fakeClient(t)
	srv.PutSession("ses_fixtureA", "a", "")
	srv.PutSession("ses_fixtureB", "b", "")
	ctx := context.Background()
	a, err := cl.Prompt(ctx, "ses_fixtureA", "msg_fixture0001", "hello there")
	if err != nil || a.ID != "msg_fixture0001" || a.Payload.Text != "hello there" || a.Delivery != "queue" {
		t.Fatalf("admitted %+v, %v", a, err)
	}
	again, err := cl.Prompt(ctx, "ses_fixtureA", "msg_fixture0001", "something else")
	if err != nil || again.Payload.Text != "hello there" {
		t.Fatalf("the repeat: %+v, %v", again, err)
	}
	if p := srv.Prompts(); len(p) != 1 || p[0].Delivery != "queue" || srv.PromptPosts() != 2 {
		t.Fatalf("admitted %+v of %d posts; want one, delivery queue, of two", p, srv.PromptPosts())
	}
	if _, err := cl.Prompt(ctx, "ses_fixtureB", "msg_fixture0001", "x"); !errors.Is(err, ErrPromptConflict) {
		t.Fatalf("the id on another session: %v", err)
	}
	if _, err := cl.Prompt(ctx, "ses_fixtureNone", "msg_fixture0002", "x"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("an unknown session: %v", err)
	}
	if _, err := cl.Prompt(ctx, "ses_fixtureA", "nope", "x"); !errors.Is(err, ErrPromptRefused) {
		t.Fatalf("a malformed id: %v", err)
	}
	ms, err := cl.Messages(ctx, "ses_fixtureA", 10)
	if err != nil || len(ms) != 1 || ms[0].Type != "user" || ms[0].Text != "hello there" {
		t.Fatalf("the session's messages: %+v, %v", ms, err)
	}
}

// TestThePromptDecodesTheRealServersAnswers: each body a real server answered
// a prompt with (prompt1, prompt1-again — the first text again — the 404 and
// the 409) decodes into the outcome it means.
func TestThePromptDecodesTheRealServersAnswers(t *testing.T) {
	var first struct {
		Data Admitted `json:"data"`
	}
	if err := json.Unmarshal(opencodetest.Captured("prompt1"), &first); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		body   string
		status int
		want   error
	}{
		{"prompt1", 200, nil},
		{"prompt1-again", 200, nil},
		{"prompt-404", 404, ErrSessionNotFound},
		{"prompt-409", 409, ErrPromptConflict},
		// A 200 naming ANOTHER message (prompt2's) admitted nothing of ours.
		{"prompt2", 200, ErrNotAdmitted},
	} {
		body := opencodetest.Captured(c.body)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write(body)
		}))
		cl, _ := NewClient(ts.URL, func() string { return "t" })
		a, err := cl.Prompt(context.Background(), first.Data.SessionID, first.Data.ID, "anything")
		ts.Close()
		if !errors.Is(err, c.want) && (c.want != nil || err != nil) {
			t.Fatalf("%s: %v, want %v", c.body, err, c.want)
		}
		if c.want == nil && (a.ID != first.Data.ID || a.Payload.Text != first.Data.Payload.Text) {
			t.Fatalf("%s decoded %+v; want the first admission %+v", c.body, a, first.Data)
		}
	}
}

// TestTheFakesSendAnswersHaveTheRealShape: the fake's create, prompt, 404 and
// 409 bodies carry exactly the keys the real server's did — so a test the
// fake passes is not passing against a shape opencode never sends.
func TestTheFakesSendAnswersHaveTheRealShape(t *testing.T) {
	srv, _ := fakeClient(t)
	srv.PutSession("ses_fixtureA", "a", "")
	srv.PutSession("ses_fixtureB", "b", "")
	post := func(path, body string) (int, json.RawMessage) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		req.SetBasicAuth(User, fixturePassword)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		return resp.StatusCode, raw
	}
	keys := func(raw json.RawMessage, path ...string) []string {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		for _, p := range path {
			_ = json.Unmarshal(m[p], &m)
		}
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	for _, c := range []struct {
		name, path, body, captured string
		status                     int
		within                     []string
	}{
		{"create", "/api/session", `{"id":"ses_fixtureNew","location":{"directory":"/srv/fixture"}}`, "create", 200, []string{"data"}},
		{"prompt", "/api/session/ses_fixtureA/prompt", `{"id":"msg_fixture1","text":"hi","delivery":"queue"}`, "prompt1", 200, []string{"data"}},
		{"repeat", "/api/session/ses_fixtureA/prompt", `{"id":"msg_fixture1","text":"other","delivery":"queue"}`, "prompt1-again", 200, []string{"data"}},
		{"conflict", "/api/session/ses_fixtureB/prompt", `{"id":"msg_fixture1","text":"hi","delivery":"queue"}`, "prompt-409", 409, nil},
		{"unknown", "/api/session/ses_fixtureNone/prompt", `{"id":"msg_fixture2","text":"hi"}`, "prompt-404", 404, nil},
	} {
		st, raw := post(c.path, c.body)
		if st != c.status {
			t.Fatalf("%s: HTTP %d, want %d", c.name, st, c.status)
		}
		got, want := keys(raw, c.within...), keys(opencodetest.Captured(c.captured), c.within...)
		if !slices.Equal(got, want) {
			t.Fatalf("%s: keys %q; the real server's %q", c.name, got, want)
		}
	}
}

// TestCreateGetAndCommands: the launcher's create keeps the id and the
// directory; a read finds it and 404s an unknown one; the commands list
// needs a directory, as measured.
func TestCreateGetAndCommands(t *testing.T) {
	_, cl := fakeClient(t)
	ctx := context.Background()
	s, err := cl.CreateSession(ctx, "ses_fixtureNew", "/srv/fixture/proj")
	if err != nil || s.ID != "ses_fixtureNew" || s.Location.Directory != "/srv/fixture/proj" {
		t.Fatalf("created %+v, %v", s, err)
	}
	if _, err := cl.CreateSession(ctx, "ses_fixtureRel", "relative"); err == nil {
		t.Fatal("a relative directory was sent")
	}
	g, err := cl.GetSession(ctx, "ses_fixtureNew")
	if err != nil || g.ID != "ses_fixtureNew" || g.ParentID != "" {
		t.Fatalf("read %+v, %v", g, err)
	}
	if _, err := cl.GetSession(ctx, "ses_fixtureNone"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("an unknown session: %v", err)
	}
	cmds, err := cl.Commands(ctx, "/srv/fixture/proj")
	if err != nil || len(cmds) != 2 || cmds[0].Name != "init" || cmds[1].Name != "review" {
		t.Fatalf("commands %+v, %v", cmds, err)
	}
	if cmds, err := cl.Commands(ctx, ""); err != nil || len(cmds) != 0 {
		t.Fatalf("commands without a directory %+v, %v", cmds, err)
	}
}

// TestIDFormats: a minted session id is a valid one in upstream's format
// (ses_, 12 hex time-DESCENDING digits, 14 alphabet characters), later ones
// sorting first; message ids are bounded.
func TestIDFormats(t *testing.T) {
	t0 := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	a, b := NewSessionID(t0), NewSessionID(t0.Add(time.Second))
	for _, id := range []string{a, b} {
		if !ValidSessionID(id) || len(id) != 4+12+14 || !strings.HasPrefix(id, "ses_") {
			t.Fatalf("minted %q", id)
		}
		for _, r := range id[4:16] {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("time part of %q", id)
			}
		}
	}
	if !(b[4:16] < a[4:16]) {
		t.Fatalf("a later id %q does not sort before %q (descending)", b, a)
	}
	for id, ok := range map[string]bool{"msg_abc123": true, "msg_": false, "msg_a-b": false, "ses_x": false, "msg_" + strings.Repeat("a", 65): false} {
		if ValidMessageID(id) != ok {
			t.Errorf("ValidMessageID(%q) = %v", id, !ok)
		}
	}
}

// TestTheLaunchRecord: a record round-trips at its pane's path, 0600 in a
// 0700 directory; one for another pane, a malformed one and a non-loopback
// server are refused; RemoveLaunch removes only the launch it names.
func TestTheLaunchRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "launch")
	r := LaunchRecord{V: LaunchVersion, Socket: "/tmp/tmux-1000/default", Pane: "%12", PID: 4242, PIDStart: "777",
		Session: "ses_fixtureA", Server: "http://127.0.0.1:4096", Directory: "/srv/fixture", Created: 946684800}
	if err := WriteLaunch(dir, r); err != nil {
		t.Fatal(err)
	}
	p, _ := LaunchPath(dir, r.Socket, r.Pane)
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record file %v, %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Dir(p)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("record dir mode %v", fi.Mode().Perm())
	}
	got, err := ReadLaunch(dir, r.Socket, r.Pane)
	if err != nil || got != r {
		t.Fatalf("read %+v, %v", got, err)
	}
	if _, err := ReadLaunch(dir, r.Socket, "%13"); !errors.Is(err, ErrNoLaunch) {
		t.Fatalf("another pane: %v", err)
	}
	if _, err := ReadLaunch(dir, "/tmp/tmux-1000/work", r.Pane); !errors.Is(err, ErrNoLaunch) {
		t.Fatalf("another tmux server: %v", err)
	}
	bad := r
	bad.Server = "http://192.0.2.1:4096"
	if err := WriteLaunch(dir, bad); err == nil {
		t.Fatal("a non-loopback server was recorded")
	}
	// A file at the pane's path that names another pane is not its record.
	other := r
	other.Pane = "%99"
	b, _ := json.Marshal(other)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLaunch(dir, r.Socket, r.Pane); err == nil || errors.Is(err, ErrNoLaunch) {
		t.Fatalf("a record naming another pane was read: %v", err)
	}
	if err := WriteLaunch(dir, r); err != nil {
		t.Fatal(err)
	}
	newer := r
	newer.PID = 5151
	if err := RemoveLaunch(dir, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLaunch(dir, r.Socket, r.Pane); err != nil {
		t.Fatalf("removing another launch removed this one: %v", err)
	}
	if err := RemoveLaunch(dir, r); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLaunch(dir, r.Socket, r.Pane); !errors.Is(err, ErrNoLaunch) {
		t.Fatalf("after removal: %v", err)
	}
}
