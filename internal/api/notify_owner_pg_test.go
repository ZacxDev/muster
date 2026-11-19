package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// notify_owner THROUGH THE REAL DOORS against a real Postgres, for every kind
// of caller: a dispatched agent and the chief on the agent door, a mirrored
// session and an agent the chief spawned on a host on the mirror door. Each
// push is checked on the owner's device as the push service received it, and
// each call's audit_log row as the owner reads it. Every name is invented.

type notifyFixture struct {
	*hostFixture
	store  *memPush
	pusher *payloadPusher
	logs   *syncBuffer
	users  *users.PGStore
	device string
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	nf := &notifyFixture{store: &memPush{}, pusher: &payloadPusher{}, logs: &syncBuffer{}, device: ep("notify-owner")}
	nf.hostFixture = newHostFixture(t, func(ext *Extensions) {
		ext.Push = push.NewService(nf.store, nf.pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
		ext.NotifyLinkHosts = []string{"docs.example.com"}
	})
	nf.users = users.NewPG(nf.pool)
	nf.srv.ext.Users = nf.users
	nf.srv.logger = log.New(nf.logs, "", 0)
	claim(t, nf.store, nf.owner, nf.device)
	return nf
}

// notifyToolText is a tools/call result's text, as the mirror door answered it.
func notifyToolText(t *testing.T, result string) string {
	t.Helper()
	var r struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(result), &r); err != nil || len(r.Content) == 0 {
		t.Fatalf("not a tools/call result: %s", result)
	}
	return r.Content[0].Text
}

func notifyResult(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("not JSON: %s", text)
	}
	return out
}

// agentNotify calls notify_owner as agent a on the agent door.
func (f *notifyFixture) agentNotify(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	a := f.worker
	if name == "chief" {
		a = f.chief
	}
	return notifyResult(t, f.callRaw(a, notifyOwnerTool, args))
}

// mirrorNotify calls notify_owner as session sid on host-bravo, on the mirror door.
func (f *notifyFixture) mirrorNotify(t *testing.T, sid string, args map[string]any) map[string]any {
	t.Helper()
	return notifyResult(t, notifyToolText(t, f.mirrorDoor(t, f.owner, "host-bravo", sid, mcpToolCall(notifyOwnerTool, args))))
}

// lastPush is the newest payload the owner's device received.
func (f *notifyFixture) lastPush(t *testing.T) push.Payload {
	t.Helper()
	got := f.pusher.to(f.device)
	if len(got) == 0 {
		t.Fatal("the owner's device received nothing")
	}
	return got[len(got)-1]
}

func (f *notifyFixture) auditRows(t *testing.T) []users.AuditEntry {
	t.Helper()
	rows, err := f.users.OwnAudit(f.asOwner, f.owner, users.NotifyAuditAction, 50)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestNotifyOwnerFromEveryKindOfCaller: each kind of caller is offered the
// tool, its push reaches the owner's device, and the push's body names the
// sender as muster knows it — "agent <name> (dispatched)", "your chief",
// the mirrored session's own name, and "(spawned on a host)" for the chief's
// host dispatch — while the title is the caller's. Each call writes one audit
// row with that sender and kind, which only the owner reads.
func TestNotifyOwnerFromEveryKindOfCaller(t *testing.T) {
	f := newNotifyFixture(t)
	for _, a := range []string{"worker", "chief"} {
		ag := f.worker
		if a == "chief" {
			ag = f.chief
		}
		listed := false
		for _, tl := range f.srv.toolsFor(f.asOwner, ag) {
			listed = listed || (tl.def.Name == notifyOwnerTool && tl.mcp)
		}
		if !listed {
			t.Fatalf("the %s is not offered notify_owner on the agent door", a)
		}
	}
	id, win := f.startRunningHostDispatch(t)
	f.setBravoWindows(
		mirror.Window{Session: "agents", ID: win, Claude: true, ClaudeSession: doneAgentSID, Dispatch: id, Status: mirror.StatusIdle},
		mirror.Window{Session: "main", ID: "@901", Claude: true, ClaudeSession: doneOtherSID, Status: mirror.StatusIdle})

	cases := []struct {
		who      string
		call     func(args map[string]any) map[string]any
		from     string
		notFrom  []string
		kind     string
		wantPath string
	}{
		{"dispatched", func(a map[string]any) map[string]any { return f.agentNotify(t, "worker", a) },
			"From agent " + f.worker.Name + " (dispatched): ", nil, notifyKindDispatched, "/agents/" + f.worker.Name},
		{"chief", func(a map[string]any) map[string]any { return f.agentNotify(t, "chief", a) },
			notifySender{label: firstNonBlank(f.chief.DisplayName, f.chief.Name), kind: notifyKindChief}.fromLine() + ": ", []string{"(dispatched)"}, notifyKindChief, "/agents/" + f.chief.Name},
		{"mirrored", func(a map[string]any) map[string]any { return f.mirrorNotify(t, doneOtherSID, a) },
			"From ", []string{"(spawned", "(dispatched)", "your chief"}, notifyKindMirrored, "/"},
		{"spawned", func(a map[string]any) map[string]any { return f.mirrorNotify(t, doneAgentSID, a) },
			" (spawned on a host): ", []string{"(dispatched)"}, notifyKindSpawned, "/"},
	}
	for i, c := range cases {
		res := c.call(map[string]any{"title": "**Build** " + c.who + " is green", "body": "All `12` checks pass."})
		if res["status"] != "delivered" || res["devices"] != float64(1) {
			t.Fatalf("%s: %v", c.who, res)
		}
		p := f.lastPush(t)
		if p.Title != "Build "+c.who+" is green" {
			t.Errorf("%s: title %q (want the caller's, plained)", c.who, p.Title)
		}
		if !strings.Contains(p.Body, c.from) || !strings.HasPrefix(p.Body, "From ") || !strings.HasSuffix(p.Body, "All 12 checks pass.") {
			t.Errorf("%s: body %q; want the sender %q and the plained body", c.who, p.Body, c.from)
		}
		for _, n := range c.notFrom {
			if strings.Contains(p.Body, n) {
				t.Errorf("%s: body %q names %q", c.who, p.Body, n)
			}
		}
		if p.Data["url"] != c.wantPath || p.Data["open"] != "" {
			t.Errorf("%s: url %q open %q; want %q (the default)", c.who, p.Data["url"], p.Data["open"], c.wantPath)
		}
		if res["from"] != strings.TrimSuffix(strings.SplitN(p.Body, ": All", 2)[0], ":") {
			t.Errorf("%s: the result's from %q does not match the push's %q", c.who, res["from"], p.Body)
		}
		rows := f.auditRows(t)
		if len(rows) != i+1 || rows[0].Detail["kind"] != c.kind || rows[0].Detail["status"] != "delivered" ||
			rows[0].Detail["title"] != p.Title || rows[0].UserID != f.owner {
			t.Fatalf("%s: audit rows %+v", c.who, rows)
		}
	}
	// Four senders, four tags: none replaced another on the device.
	tags := map[string]bool{}
	for _, p := range f.pusher.to(f.device) {
		tags[p.Tag] = true
	}
	if len(tags) != 4 {
		t.Errorf("tags %v; want one per sender", tags)
	}
	// Only the owner reads them: another member's read is empty, even naming
	// the owner.
	other := f.newMember(t)
	if rows, err := f.users.OwnAudit(f.as(other), f.owner, users.NotifyAuditAction, 50); err != nil || len(rows) != 0 {
		t.Fatalf("another member read the owner's notify rows: %v %v", rows, err)
	}
	// And the owner's Account page lists them.
	req := httptest.NewRequest("GET", "/account", nil).WithContext(f.asOwner)
	page := f.srv.accountNotifyLog(req)
	if strings.Count(page, `data-testid="notify-log-row"`) != 4 || !strings.Contains(page, "Build spawned is green") {
		t.Fatalf("the Account list: %s", page)
	}
}

// TestNotifyOwnerLinksAnAllowedHostThroughMuster: an allowlisted https link is
// carried as muster's own /notify/open (and marked external for the service
// worker); the result names the address itself; a refused link sends nothing
// and records nothing.
func TestNotifyOwnerLinksAnAllowedHostThroughMuster(t *testing.T) {
	f := newNotifyFixture(t)
	res := f.agentNotify(t, "worker", map[string]any{"title": "Report ready", "url": "https://docs.example.com/r/9"})
	p := f.lastPush(t)
	if res["status"] != "delivered" || res["url"] != "https://docs.example.com/r/9" ||
		p.Data["url"] != notifyOpenPath+"?to="+url.QueryEscape("https://docs.example.com/r/9") || p.Data["open"] != "external" {
		t.Fatalf("result %v, payload data %v", res, p.Data)
	}
	if v2 := p.V2(); v2.URL != p.Data["url"] {
		t.Fatalf("the native payload's url %q; want the muster path %q", v2.URL, p.Data["url"])
	}
	before := len(f.pusher.to(f.device))
	for _, bad := range []string{"https://evil.example/", "javascript:alert(1)", "//evil.example"} {
		res := f.agentNotify(t, "worker", map[string]any{"title": "x", "url": bad})
		if res["error"] == nil {
			t.Fatalf("%q: %v; want a refusal", bad, res)
		}
	}
	for _, empty := range []string{"", "   ", "<b></b>"} {
		if res := f.agentNotify(t, "worker", map[string]any{"title": empty}); res["error"] == nil {
			t.Fatalf("an empty title %q: %v", empty, res)
		}
	}
	f.srv.WaitForPushes()
	if len(f.pusher.to(f.device)) != before || len(f.auditRows(t)) != 1 {
		t.Fatalf("a refused call sent %d push(es) or wrote a row", len(f.pusher.to(f.device))-before)
	}
}

// TestNotifyOwnerSaysWhyItWasNotDelivered: the result, the audit row and the
// log each name the reason — the owner's toggle off, the sender's cap (its
// own: another sender still goes), the owner's cap, no device.
func TestNotifyOwnerSaysWhyItWasNotDelivered(t *testing.T) {
	f := newNotifyFixture(t)
	now := time.Unix(4_000_000, 0)
	f.srv.now = func() time.Time { return now }
	check := func(label string, res map[string]any, status, reason string) {
		t.Helper()
		if res["status"] != status || res["reason"] != reason {
			t.Fatalf("%s: %v; want %s/%s", label, res, status, reason)
		}
		rows := f.auditRows(t)
		if rows[0].Detail["status"] != status || rows[0].Detail["reason"] != reason {
			t.Fatalf("%s: the audit row %+v", label, rows[0])
		}
		if !strings.Contains(f.logs.String(), "event="+push.EventAgentNotify+" owner="+strconv.FormatInt(f.owner, 10)+": "+pushDropSentence(reason)) {
			t.Fatalf("%s: no drop line; log:\n%s", label, f.logs.String())
		}
	}

	// The toggle off ("An agent sent you a notification").
	if err := f.store.SetMuted(f.asOwner, []string{push.EventAgentNotify}); err != nil {
		t.Fatal(err)
	}
	check("muted", f.agentNotify(t, "worker", map[string]any{"title": "muted"}), "suppressed", pushDropMuted)
	if len(f.pusher.to(f.device)) != 0 {
		t.Fatal("a muted notification reached the device")
	}
	_ = f.store.SetMuted(f.asOwner, nil)

	// The sender's cap: the muted call above spent one of the worker's.
	for i := 1; i < notifySenderBurst; i++ {
		if res := f.agentNotify(t, "worker", map[string]any{"title": "n" + strconv.Itoa(i)}); res["status"] != "delivered" {
			t.Fatalf("send %d: %v", i, res)
		}
	}
	check("sender cap", f.agentNotify(t, "worker", map[string]any{"title": "one too many"}), "suppressed", pushDropSenderCap)
	if res := f.agentNotify(t, "chief", map[string]any{"title": "the chief's own"}); res["status"] != "delivered" {
		t.Fatalf("another sender was capped too: %v", res)
	}
	now = now.Add(notifySenderWindow)
	if res := f.agentNotify(t, "worker", map[string]any{"title": "next window"}); res["status"] != "delivered" {
		t.Fatalf("the sender's window did not roll: %v", res)
	}

	// The owner's cap (pushAllowed), spent by everything else.
	for f.srv.pushLimit.wins[f.owner] == nil || f.srv.pushLimit.wins[f.owner].count < pushOwnerBurst {
		f.srv.pushLimit.allow(f.owner, now)
	}
	check("owner cap", f.agentNotify(t, "chief", map[string]any{"title": "capped"}), "suppressed", pushDropOwnerCap)

	// No device.
	now = now.Add(pushOwnerWindow)
	f.store.mu.Lock()
	f.store.devs = nil
	f.store.mu.Unlock()
	check("no device", f.agentNotify(t, "chief", map[string]any{"title": "nobody"}), "no_device", pushDropNoDevice)
}

// TestNotifyOwnerIsRefusedForATeamAndUnlistedWithoutPush: a team-owned
// caller is refused (a team has no one owner), and a server without push
// lists no notify_owner on either door and answers a call by name with why.
func TestNotifyOwnerIsRefusedForATeamAndUnlistedWithoutPush(t *testing.T) {
	f := newNotifyFixture(t)
	team := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleMember, Via: authz.ViaMirrorHost, Team: true})
	c := mirrorCaller("host-bravo", doneOtherSID)
	if res := notifyResult(t, f.srv.toolNotifyOwner(team, c, `{"title":"x"}`)); !strings.Contains(res["error"].(string), "team") {
		t.Fatalf("a team caller: %v", res)
	}
	f.srv.ext.Push = nil
	for _, d := range f.srv.mirrorTools() {
		if d.Name == notifyOwnerTool {
			t.Fatal("the mirror door lists notify_owner without push")
		}
	}
	for _, tl := range f.srv.agentTools() {
		if tl.def.Name == notifyOwnerTool {
			t.Fatal("the agent door lists notify_owner without push")
		}
	}
	if res := notifyResult(t, f.callRaw(f.worker, notifyOwnerTool, map[string]any{"title": "x"})); !strings.Contains(res["error"].(string), "not configured") {
		t.Fatalf("a call by name without push: %v", res)
	}
}
