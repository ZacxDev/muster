package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// notifyTestHosts is the operator's allowlist in these tests: invented names.
var notifyTestHosts = []string{"docs.example.com", "status.example.org"}

// TestNotifyLinkValidation is the notify_owner link rule (notifyLinkFor) as a
// table: what a tap may open — a muster path, or https on an allowlisted host
// through /notify/open — and every shape that would leave muster for a host
// the operator did not allow, each refused with a sentence.
func TestNotifyLinkValidation(t *testing.T) {
	open := func(u string) string { return notifyOpenPath + "?to=" + url.QueryEscape(u) }
	for _, c := range []struct {
		raw, path, external string
	}{
		{"", "", ""},
		{"/tasks/42", "/tasks/42", ""},
		{"/pages/7/preview?v=3", "/pages/7/preview?v=3", ""},
		{"/attention?tab=drafts#draft-3", "/attention?tab=drafts#draft-3", ""},
		{"/agents/build%20bot", "/agents/build%20bot", ""},
		{"  /tasks/9  ", "/tasks/9", ""},
		{"https://docs.example.com/guide?x=1#top", open("https://docs.example.com/guide?x=1#top"), "https://docs.example.com/guide?x=1#top"},
		{"https://status.example.org", open("https://status.example.org"), "https://status.example.org"},
		{"HTTPS://Docs.Example.com/a", open("https://Docs.Example.com/a"), "https://Docs.Example.com/a"},
	} {
		path, ext, refused := notifyLinkFor(c.raw, notifyTestHosts)
		if refused != "" || path != c.path || ext != c.external {
			t.Errorf("%q: path %q external %q refused %q; want path %q external %q", c.raw, path, ext, refused, c.path, c.external)
		}
	}
	for _, c := range []struct{ raw, why string }{
		{"https://other.example.net/", "not one this server allows"},
		{"https://sub.docs.example.com/", "not one this server allows"},
		{"https://docs.example.com./", "not one this server allows"},
		{"https://docs.example.com.evil.example/", "not one this server allows"},
		{"https://evil.example/#@docs.example.com", "not one this server allows"},
		{"http://docs.example.com/", "muster path starting with"},
		{"javascript:alert(1)", "muster path starting with"},
		{"JavaScript:alert(1)", "muster path starting with"},
		{"data:text/html,hi", "muster path starting with"},
		{"ftp://docs.example.com/", "muster path starting with"},
		{"https:docs.example.com", "muster path starting with"},
		{"https:/docs.example.com", "muster path starting with"},
		{"//evil.example/x", "another host"},
		{"/\\evil.example", "URL characters"},
		{"/%2F%2Fevil.example", "another host"},
		{"/%2f/evil.example", "another host"},
		{"/%5Cevil.example", "another host"},
		{"/%5cevil.example", "another host"},
		{"/a//b", "another host"},
		{"/../admin", `".." segment`},
		{"/%2e%2e/admin", `".." segment`},
		{"/./x", `".." segment`},
		{"/x/%2E", `".." segment`},
		{"/tasks%00", "control character"},
		{"/tasks%0a", "control character"},
		{"/bad%zz", "malformed"},
		{"/tasks 1", "URL characters"},
		{"/tasks\t1", "URL characters"},
		{"/tâches", "URL characters"},
		{"https://dоcs.example.com/", "URL characters"}, // a Cyrillic о
		{"https://docs.example.com/\\x", "URL characters"},
		{"https://user:pw@docs.example.com/", "user name or password"},
		{"https://docs.example.com@evil.example/", "user name or password"},
		{"https://docs.example.com%2F@evil.example/", "user name or password"},
		{"https://docs.example.com:8443/", "may not name a port"},
		{"https://docs.example.com:443/", "may not name a port"},
		{notifyOpenPath + "?to=https%3A%2F%2Fdocs.example.com", "as itself"},
		{"/" + strings.Repeat("a", notifyURLMax), "longer than"},
	} {
		path, ext, refused := notifyLinkFor(c.raw, notifyTestHosts)
		if refused == "" || path != "" || ext != "" {
			t.Errorf("%q: path %q external %q — want it refused", c.raw, path, ext)
			continue
		}
		if !strings.Contains(refused, c.why) {
			t.Errorf("%q: refused %q; want a reason containing %q", c.raw, refused, c.why)
		}
	}
	// No allowlist (the default): every external address is refused, with
	// its own sentence; a muster path still goes.
	if _, _, refused := notifyLinkFor("https://docs.example.com/", nil); !strings.Contains(refused, "MUSTER_NOTIFY_LINK_HOSTS is empty") {
		t.Errorf("an empty allowlist: refused %q", refused)
	}
	if path, _, refused := notifyLinkFor("/tasks/1", nil); refused != "" || path != "/tasks/1" {
		t.Errorf("an empty allowlist refused a muster path: %q %q", path, refused)
	}
}

// TestNotifyOpenRedirectsOnlyToAnAllowedHost: GET /notify/open re-checks its
// address against the CURRENT allowlist — an allowed one is a 303 with no
// referrer; a host the operator took out, or one never allowed, is a 400 that
// goes nowhere.
func TestNotifyOpenRedirectsOnlyToAnAllowedHost(t *testing.T) {
	s, h := pushServer(t, &memPush{}, &recPusher{}, 7)
	s.ext.NotifyLinkHosts = []string{"docs.example.com"}
	get := func(to string) *http.Response {
		rec := pushDo(t, h, "GET", notifyOpenPath+"?to="+url.QueryEscape(to), "")
		return rec.Result()
	}
	r := get("https://docs.example.com/guide#top")
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "https://docs.example.com/guide#top" ||
		r.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("an allowed address: %d Location %q Referrer-Policy %q", r.StatusCode, r.Header.Get("Location"), r.Header.Get("Referrer-Policy"))
	}
	for _, bad := range []string{"https://evil.example/", "http://docs.example.com/", "javascript:alert(1)", "/tasks/1", "//evil.example", ""} {
		if r := get(bad); r.StatusCode != http.StatusBadRequest || r.Header.Get("Location") != "" {
			t.Errorf("%q: %d Location %q; want 400 and no redirect", bad, r.StatusCode, r.Header.Get("Location"))
		}
	}
	// Taken out of the allowlist: the same link stops at once.
	s.ext.NotifyLinkHosts = nil
	if r := get("https://docs.example.com/guide"); r.StatusCode != http.StatusBadRequest || r.Header.Get("Location") != "" {
		t.Fatalf("a host no longer allowed: %d Location %q", r.StatusCode, r.Header.Get("Location"))
	}
}

// TestTheSenderCapIsPerSenderAndRolls: notifySenderBurst per window per key,
// each key its own, and a rolled window starts over.
func TestTheSenderCapIsPerSenderAndRolls(t *testing.T) {
	var l senderRateLimit
	now := time.Unix(3_000_000, 0)
	for i := 0; i < notifySenderBurst; i++ {
		if !l.allow("notify:agent:1", now) {
			t.Fatalf("send %d refused inside the cap", i+1)
		}
	}
	if l.allow("notify:agent:1", now) {
		t.Fatal("the send past the cap was allowed")
	}
	if !l.allow("notify:agent:2", now) || !l.allow("page_update:agent:1", now) {
		t.Fatal("another sender, or the same sender's other bucket, was refused")
	}
	if l.allow("notify:agent:1", now.Add(notifySenderWindow-time.Second)) {
		t.Fatal("allowed again before the window rolled")
	}
	if !l.allow("notify:agent:1", now.Add(notifySenderWindow)) {
		t.Fatal("still refused once the window rolled")
	}
}

// TestNotifyOutcomeStatus: the result's status from what goNotify decided.
func TestNotifyOutcomeStatus(t *testing.T) {
	for _, c := range []struct {
		delivered      int
		drops          []string
		status, reason string
	}{
		{2, nil, "delivered", ""},
		{1, []string{pushDropNoDevice}, "delivered", ""},
		{0, []string{pushDropMuted}, "suppressed", pushDropMuted},
		{0, []string{pushDropOwnerCap}, "suppressed", pushDropOwnerCap},
		{0, []string{pushDropNoRecipient}, "suppressed", pushDropNoRecipient},
		{0, []string{pushDropNoDevice, pushDropMuted}, "suppressed", pushDropMuted},
		{0, []string{pushDropNoDevice}, "no_device", pushDropNoDevice},
		{0, []string{pushDropFailed}, "failed", pushDropFailed},
		{0, []string{pushDropOff}, "not_configured", pushDropOff},
		{0, []string{pushDropRouterOnly}, "not_configured", pushDropRouterOnly},
		{0, nil, "unknown", ""},
	} {
		if s, r := notifyOutcomeStatus(c.delivered, c.drops); s != c.status || r != c.reason {
			t.Errorf("%d %v: %q %q; want %q %q", c.delivered, c.drops, s, r, c.status, c.reason)
		}
	}
}
