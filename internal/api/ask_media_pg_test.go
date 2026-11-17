package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/askattach"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/users"
)

// AN AGENT'S ASK CARRIES FILES AND A WHY (ask_media.go, migration 0143), on the
// isolation world's real Postgres, real upload service and real mux: A's agent
// sends a file to the upload store with its own token, asks with its id, and
// only who may read the ask — its owner, or a member of the team that owns it
// — can fetch the file, through a signed-in session.

// askMediaPNG is a 1×1 PNG's leading bytes: sniffed image/png.
var askMediaPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89-pixels")

// askUpload sends data as tok's agent's ask attachment; the status and the id.
func (w *isoWorld) askUpload(t *testing.T, tok, name string, data []byte) (int, string) {
	t.Helper()
	rec := w.do(w.h, http.MethodPost, askattach.AgentPath+"?filename="+name, string(data), cred{agentToken: tok})
	var r askattach.Record
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r.ID
}

// askTool calls attention_ask over POST /mcp as tok's agent: the tool's text
// and whether it is a tool error.
func (w *isoWorld) askTool(t *testing.T, tok string, args map[string]any) (string, bool) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": attentionAskTool, "arguments": args}})
	rec := w.do(w.h, http.MethodPost, "/mcp", string(raw), cred{agentToken: tok})
	var reply struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Error != nil || len(reply.Result.Content) == 0 {
		t.Fatalf("attention_ask: HTTP %d %s", rec.Code, rec.Body)
	}
	text := reply.Result.Content[0].Text
	if reply.Result.IsError {
		// A tool error's text is {"error": "<sentence>", …}: the sentence.
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(text), &e) == nil && e.Error != "" {
			text = e.Error
		}
	}
	return text, reply.Result.IsError
}

// mediaAskID is the id in attention_ask's answer.
func mediaAskID(t *testing.T, text string) int64 {
	t.Helper()
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID <= 0 {
		t.Fatalf("attention_ask answered %s", text)
	}
	return out.ID
}

func (w *isoWorld) askRow(t *testing.T, owner, id int64) attention.Ask {
	t.Helper()
	a, err := attention.NewPG(w.pool).Get(asUser(owner), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAnAgentsFileReachesItsAskAndOnlyWhoMayReadTheAskFetchesIt(t *testing.T) {
	w := newIsoWorld(t)
	code, id := w.askUpload(t, w.a.token, "shot.png", askMediaPNG)
	if code != http.StatusCreated || id == "" {
		t.Fatalf("A's agent's upload: %d %q", code, id)
	}
	text, isErr := w.askTool(t, w.a.token, map[string]any{"question": "Does this look right?", "attachment_ids": []string{id}, "context": "Checking the new header before I ship it"})
	if isErr {
		t.Fatalf("the ask was refused: %s", text)
	}
	ask := w.askRow(t, w.a.uid, mediaAskID(t, text))
	if len(ask.Attachments) != 1 || ask.Attachments[0].ID != id || ask.Attachments[0].Type != "image/png" || ask.Attachments[0].Name != "shot.png" ||
		ask.Attachments[0].Size != int64(len(askMediaPNG)) {
		t.Fatalf("the ask carries %+v, want the uploaded PNG", ask.Attachments)
	}
	path := askAttachmentPath(ask.ID, id)

	// The owner's session fetches it, inline, as the inert headers say.
	rec := w.do(w.h, http.MethodGet, path, "", cred{sessionUser: w.a.uid})
	if rec.Code != http.StatusOK || rec.Body.String() != string(askMediaPNG) || rec.Header().Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline") || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("the owner's fetch: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	// The ask page and the Asks list draw it as a thumbnail on that route.
	thumb := `<img src="` + path + `"`
	for _, page := range []string{askPagePath(ask.ID), "/attention"} {
		if body := w.do(w.h, http.MethodGet, page, "", cred{sessionUser: w.a.uid}).Body.String(); !strings.Contains(body, thumb) ||
			!strings.Contains(body, `data-ask-why="agent"`) || !strings.Contains(body, "Checking the new header before I ship it") {
			t.Errorf("%s does not draw the thumbnail and the why:\n%.2000s", page, body)
		}
	}

	// Another owner, the admin (not its owner), an agent token and no
	// credential at all: no 2xx, and never the bytes.
	for who, c := range map[string]cred{
		"another owner's session": {sessionUser: w.b.uid},
		"the admin's session":     {sessionUser: w.op.uid},
		"A's own agent's token":   {agentToken: w.a.token},
		"no session":              {},
	} {
		rec := w.do(w.h, http.MethodGet, path, "", c)
		if rec.Code < 300 || strings.Contains(rec.Body.String(), "PNG") {
			t.Errorf("%s fetched A's ask's attachment: %d", who, rec.Code)
		}
	}
	// The ask's id with an upload it does NOT name (A's own other upload): 404.
	if _, other := w.askUpload(t, w.a.token, "other.png", askMediaPNG); w.do(w.h, http.MethodGet, askAttachmentPath(ask.ID, other), "", cred{sessionUser: w.a.uid}).Code != http.StatusNotFound {
		t.Error("an upload the ask does not name was served through it")
	}
}

// TestAnAttachmentOfAPersonalAskIsNotATeammatesAndATeamAsksIsEveryMembers:
// B is a member of a team A manages. A's PERSONAL ask's file is not B's to
// fetch (a team member without rights to that ask); the TEAM's ask's file is
// B's (at viewer and above) and not the admin's, who is no member.
func TestAnAttachmentOfAPersonalAskIsNotATeammatesAndATeamAsksIsEveryMembers(t *testing.T) {
	w := newIsoWorld(t)
	sys := authz.System(context.Background())
	store := users.NewPG(w.pool)
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	team, err := store.CreateTeam(sys, admin, "media-team-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM attention_asks WHERE owner_id = $1`, `DELETE FROM uploads WHERE owner_id = $1`,
			`DELETE FROM team_members WHERE team_id = $1`, `DELETE FROM audit_log WHERE team_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = w.pool.Exec(sys, q, team.ID)
		}
	})
	if err := store.SetTeamMember(sys, admin, team.ID, w.b.uid, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	// A's personal ask with A's file.
	_, aID := w.askUpload(t, w.a.token, "mine.png", askMediaPNG)
	text, isErr := w.askTool(t, w.a.token, map[string]any{"question": "Mine?", "attachment_ids": []string{aID}})
	if isErr {
		t.Fatal(text)
	}
	personal := askAttachmentPath(mediaAskID(t, text), aID)
	// The team's ask with the team's file, as a team host's session files it.
	tctx := authz.WithPrincipal(context.Background(), teamActingPrincipal(team.ID, authz.ViaMirrorHost))
	up, err := w.uploadSvc.PutChecked(tctx, strings.NewReader(string(askMediaPNG)), "team.png", askattach.MaxBytes, attention.MaxTimeout, askattach.Allowed)
	if err != nil {
		t.Fatal(err)
	}
	files, refused := w.srv.askAttachmentsFor(tctx, []string{up.PublicID})
	if refused != "" {
		t.Fatal(refused)
	}
	tAsk, err := attention.NewPG(w.pool).Create(tctx, attention.New{Asker: attention.Asker{MirrorHost: "team-host", MirrorSession: "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b", Label: "team"},
		Question: "Team's?", Attachments: files})
	if err != nil {
		t.Fatal(err)
	}
	teamPath := askAttachmentPath(tAsk.ID, up.PublicID)

	for _, c := range []struct {
		who  string
		path string
		cred cred
		ok   bool
	}{
		{"A, the personal ask's owner (CONTROL)", personal, cred{sessionUser: w.a.uid}, true},
		{"B, A's teammate, on A's personal ask", personal, cred{sessionUser: w.b.uid}, false},
		{"B, a viewer of the team, on the team's ask", teamPath, cred{sessionUser: w.b.uid}, true},
		{"A, the team's manager, on the team's ask", teamPath, cred{sessionUser: w.a.uid}, true},
		{"the admin, no member, on the team's ask", teamPath, cred{sessionUser: w.op.uid}, false},
		{"no session, on the team's ask", teamPath, cred{}, false},
	} {
		rec := w.do(w.h, http.MethodGet, c.path, "", c.cred)
		if got := rec.Code == http.StatusOK && rec.Body.String() == string(askMediaPNG); got != c.ok {
			t.Errorf("%s: %d (served %v, want %v)", c.who, rec.Code, got, c.ok)
		}
	}
}

func TestAnAttachmentIDTheAskerCannotReadIsRefusedInOneSentence(t *testing.T) {
	w := newIsoWorld(t)
	_, bID := w.askUpload(t, w.b.token, "bs.png", askMediaPNG)
	_, aID := w.askUpload(t, w.a.token, "as.png", askMediaPNG)
	revoked := aID
	if _, err := w.uploadSvc.Revoke(asUser(w.a.uid), revoked); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"B's upload": bID, "a missing upload": "GhostGhostGhostGhost00", "A's revoked upload": revoked} {
		text, isErr := w.askTool(t, w.a.token, map[string]any{"question": "q?", "attachment_ids": []string{id}})
		if !isErr || !strings.Contains(text, askAttachmentsRefused(id)) {
			t.Errorf("%s: %s", name, text)
		}
	}
	// Five ids: the count cap, before any is read.
	five := []string{bID, bID, bID, bID, bID}
	if text, isErr := w.askTool(t, w.a.token, map[string]any{"question": "q?", "attachment_ids": five}); !isErr || !strings.Contains(text, "at most 4 attachments") {
		t.Errorf("five ids: %s", text)
	}
	// Paths reach a dispatched agent's muster: a kind with no ccd (the world's
	// agents are not claude-code) is told attachments are not available to it
	// (a claude-code agent's `ccd ask-attach` answer is pinned in
	// TestOnlyAClaudeCodeAgentIsToldToRunCcdAskAttach).
	if w.a.agent.Kind == agents.KindClaudeCode {
		t.Fatal("fixture: A's agent is claude-code")
	}
	if text, isErr := w.askTool(t, w.a.token, map[string]any{"question": "q?", "attachments": []string{"shot.png"}}); !isErr ||
		text != askAttachUnavailable+"; ask without attachments" {
		t.Errorf("paths from a dispatched agent: %s", text)
	}
	// None of them filed an ask.
	var n int
	if err := w.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM attention_asks WHERE agent_id = $1 AND question = 'q?'`, w.a.agent.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d asks filed by refused calls (%v)", n, err)
	}
}

func TestTheUploadRouteHoldsTheAsksSizeAndTypeRules(t *testing.T) {
	w := newIsoWorld(t)
	// Over the cap (the smaller of askattach.MaxBytes and the store's).
	big := make([]byte, min(askattach.MaxBytes, w.uploadSvc.MaxBytes)+1)
	copy(big, askMediaPNG)
	if code, _ := w.askUpload(t, w.a.token, "big.png", big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a file one byte over the cap: %d, want 413", code)
	}
	if code, _ := w.askUpload(t, w.a.token, "max.png", big[:len(big)-1]); code != http.StatusCreated {
		t.Errorf("a file exactly at the cap: %d, want 201", code)
	}
	if code, _ := w.askUpload(t, w.a.token, "page.html", []byte("<!DOCTYPE html><script>alert(1)</script>")); code != http.StatusUnsupportedMediaType {
		t.Errorf("an HTML file: %d, want 415", code)
	}
	// An SVG is accepted — and stored as a type that is never inline.
	code, svgID := w.askUpload(t, w.a.token, "x.svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`))
	if code != http.StatusCreated {
		t.Fatalf("an SVG: %d", code)
	}
	text, isErr := w.askTool(t, w.a.token, map[string]any{"question": "svg?", "attachment_ids": []string{svgID}})
	if isErr {
		t.Fatal(text)
	}
	ask := w.askRow(t, w.a.uid, mediaAskID(t, text))
	rec := w.do(w.h, http.MethodGet, askAttachmentPath(ask.ID, svgID), "", cred{sessionUser: w.a.uid})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("an SVG attachment is served %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Content-Disposition"))
	}
	if page := w.do(w.h, http.MethodGet, askPagePath(ask.ID), "", cred{sessionUser: w.a.uid}).Body.String(); strings.Contains(page, `<img src="`+askAttachmentPath(ask.ID, svgID)) ||
		!strings.Contains(page, `download="x.svg"`) {
		t.Fatalf("the ask page draws the SVG as an image, or not as a chip")
	}
}

// TestTheWhyIsTheAgentsContextWhenGivenAndTheSessionsFocusAndLastWordsWhenNot:
// a context is stored as given (one line, refused past its bound) and the
// fallback is NOT read; without one, the agent's focus title and last
// assistant message are stored, secrets redacted.
func TestTheWhyIsTheAgentsContextWhenGivenAndTheSessionsFocusAndLastWordsWhenNot(t *testing.T) {
	w := newIsoWorld(t)
	if _, err := focus.NewPG(w.pool).Upsert(asUser(w.a.uid), focus.AgentSubject(w.a.agent.ID), focus.Write{Title: "Ship the v2 header", Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	w.srv.nudgeReadHook = func(context.Context, chiefnudge.Session) (nudgeRead, error) {
		reads++
		return nudgeRead{Msgs: []chiefnudge.Msg{{Role: "user", Text: "go"}, {Role: "assistant", Text: "Pushed with token ghp_" + strings.Repeat("a", 36) + "\nnow waiting."}, {Role: "user", Text: "?"}}}, nil
	}
	// Given: stored as one sanitised line; the fallback is not read.
	text, isErr := w.askTool(t, w.a.token, map[string]any{"question": "Ship?", "context": "  CI is green\nbut the canary <b>is</b> red  "})
	if isErr {
		t.Fatal(text)
	}
	if a := w.askRow(t, w.a.uid, mediaAskID(t, text)); a.Why == nil || a.Why.Text != "CI is green but the canary <b>is</b> red" || a.Why.Focus != "" || a.Why.Last != "" {
		t.Fatalf("with a context the why is %+v", a.Why)
	} else if reads != 0 {
		t.Fatalf("the transcript was read %d time(s) for an ask that gave its context", reads)
	}
	if page := w.do(w.h, http.MethodGet, askPagePath(mediaAskID(t, text)), "", cred{sessionUser: w.a.uid}).Body.String(); !strings.Contains(page, "CI is green but the canary &lt;b&gt;is&lt;/b&gt; red") {
		t.Fatalf("the ask page does not show the context escaped")
	}
	// Past its bound: refused, nothing filed.
	if text, isErr := w.askTool(t, w.a.token, map[string]any{"question": "Long?", "context": strings.Repeat("x", attention.MaxWhyRunes+1)}); !isErr || !strings.Contains(text, "the context is longer than 500 characters") {
		t.Fatalf("an over-long context: %s", text)
	}
	// Absent: the focus title and the last assistant message, redacted.
	text, isErr = w.askTool(t, w.a.token, map[string]any{"question": "Ship now?"})
	if isErr {
		t.Fatal(text)
	}
	a := w.askRow(t, w.a.uid, mediaAskID(t, text))
	if a.Why == nil || a.Why.Text != "" || a.Why.Focus != "Ship the v2 header" || !strings.HasPrefix(a.Why.Last, "Pushed with token ") ||
		strings.Contains(a.Why.Last, "ghp_aaaa") || !strings.HasSuffix(a.Why.Last, " now waiting.") || reads != 1 {
		t.Fatalf("without a context the why is %+v (%d reads)", a.Why, reads)
	}
	if page := w.do(w.h, http.MethodGet, askPagePath(a.ID), "", cred{sessionUser: w.a.uid}).Body.String(); !strings.Contains(page, `data-ask-why="session"`) ||
		!strings.Contains(page, "Ship the v2 header") {
		t.Fatalf("the ask page does not show the fallback")
	}
}

// TestAnAsksAttachmentRouteServesHostileFilesNonExecutable is
// attachments_sandbox_test.go's walk over THIS route: every hostile fixture an
// ask can carry is served with a type that names no markup, nosniff, the
// sandbox CSP and an attachment disposition (bar the four raster images); one
// that sniffs as HTML is refused at the upload, before it is stored. A PNG
// stays inline (the control: a route that made everything a download, or
// refused every file, would pass the rest).
func TestAnAsksAttachmentRouteServesHostileFilesNonExecutable(t *testing.T) {
	w := newIsoWorld(t)
	served, refusedHTML := 0, 0
	type fixture = struct {
		name, filename, claimed string
		data                    []byte
	}
	for _, fx := range append(append([]fixture{}, hostileFixtures...), fixture{"png (control)", "shot.png", "image/png", askMediaPNG}) {
		code, id := w.askUpload(t, w.a.token, fx.filename, fx.data)
		if strings.HasPrefix(askattach.Sniff(fx.data), "text/html") {
			if code != http.StatusUnsupportedMediaType {
				t.Errorf("%s: an HTML file's upload answered %d, want 415", fx.name, code)
			}
			refusedHTML++
			continue
		}
		if code != http.StatusCreated {
			t.Fatalf("%s: upload %d", fx.name, code)
		}
		text, isErr := w.askTool(t, w.a.token, map[string]any{"question": fx.name + "?", "attachment_ids": []string{id}})
		if isErr {
			t.Fatalf("%s: %s", fx.name, text)
		}
		rec := w.do(w.h, http.MethodGet, askAttachmentPath(mediaAskID(t, text), id), "", cred{sessionUser: w.a.uid})
		if rec.Code != http.StatusOK || rec.Body.String() != string(fx.data) {
			t.Fatalf("%s: %d", fx.name, rec.Code)
		}
		assertNotExecutable(t, fx.name+" via an ask", rec.Header())
		if fx.name == "png (control)" && (rec.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline")) {
			t.Fatalf("the PNG control is not inline: %v", rec.Header())
		}
		served++
	}
	// Every fixture is accounted for: the HTML one refused, the rest and the
	// control served (a count that cannot move would prove nothing).
	if refusedHTML != 1 || served != len(hostileFixtures) {
		t.Fatalf("%d refused as HTML, %d served; want 1 and %d", refusedHTML, served, len(hostileFixtures))
	}
}

// TestARevokedOrExpiredUploadIsNotServedThroughItsAsk (round 1 G1): the route
// reads the upload's row on every GET and answers 404 for one revoked or past
// its expiry, even while its object is still in the store.
func TestARevokedOrExpiredUploadIsNotServedThroughItsAsk(t *testing.T) {
	w := newIsoWorld(t)
	path := func(name string) (string, string) {
		_, id := w.askUpload(t, w.a.token, name, askMediaPNG)
		text, isErr := w.askTool(t, w.a.token, map[string]any{"question": name + "?", "attachment_ids": []string{id}})
		if isErr {
			t.Fatal(text)
		}
		return askAttachmentPath(mediaAskID(t, text), id), id
	}
	revoked, rid := path("revoked.png")
	// An ask's upload's row lives as long as an ask can stay open — not the
	// store's link TTL (an hour here) — or it would stop being served while
	// its ask still waits.
	if u, err := w.uploadSvc.Store.Get(asUser(w.a.uid), rid); err != nil || u.ExpiresAt.Before(time.Now().Add(attention.MaxTimeout-time.Hour)) {
		t.Fatalf("an ask's upload expires at %v (%v), want about %v from now", u.ExpiresAt, err, attention.MaxTimeout)
	}
	expired, eid := path("expired.png")
	live, _ := path("live.png")
	for _, p := range []string{revoked, expired, live} { // CONTROL: each is served first
		if rec := w.do(w.h, http.MethodGet, p, "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusOK {
			t.Fatalf("%s before: %d", p, rec.Code)
		}
	}
	// Revoke the ROW only (the store's Revoke, not the service's, which would
	// also delete the object), and expire the other row.
	if _, err := w.uploadSvc.Store.Revoke(asUser(w.a.uid), rid); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(authz.System(context.Background()),
		`UPDATE uploads SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE public_id = $1`, eid); err != nil {
		t.Fatal(err)
	}
	if !w.uploads.Has(uploads.ObjectKeyFor(rid)) || !w.uploads.Has(uploads.ObjectKeyFor(eid)) {
		t.Fatal("fixture: the objects are gone")
	}
	for name, p := range map[string]string{"revoked": revoked, "expired": expired} {
		if rec := w.do(w.h, http.MethodGet, p, "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "PNG") {
			t.Errorf("a %s upload through its ask: %d", name, rec.Code)
		}
	}
	if rec := w.do(w.h, http.MethodGet, live, "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusOK {
		t.Errorf("the live upload stopped being served: %d", rec.Code)
	}
}
