package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/push"
)

// THE KNOCK NOTIFICATION (plugin_notify.go; docs/design/meetings.md, "Core
// seam: knock notification"), through the real plugin door and a real plugin
// store, with the owner's devices on a recording push service: a browser and
// a paired phone (payload v2). Every name is invented.
//
// 🔴 THE FIXTURE'S TAB LABEL SHARES NO WORD WITH ANY CONSTANT the handler or
// its tests spell (the two bodies, "muster", the appForm default "Team
// calls"), so a mutant that hardcodes a title cannot produce it.
const knockLabel = "Quartz Desk"

// knockPusher records every body a device was sent, by endpoint, as sent.
type knockPusher struct {
	mu  sync.Mutex
	got map[string][]string
}

func (p *knockPusher) Send(_ context.Context, sub push.Subscription, body []byte) push.SendResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.got == nil {
		p.got = map[string][]string{}
	}
	p.got[sub.Endpoint] = append(p.got[sub.Endpoint], string(body))
	return push.SendResult{StatusCode: 201}
}

func (p *knockPusher) to(ep string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got[ep]...)
}

func (p *knockPusher) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, v := range p.got {
		n += len(v)
	}
	return n
}

type knockFixture struct {
	*handoffSeam
	store   *memPush
	pusher  *knockPusher
	logs    *syncBuffer
	tok     string // the owner's app plugin's token
	id      int64  // its plugin id
	msgTok  string // the owner's messages-only plugin's token
	browser string // the owner's browser endpoint
	phone   string // the owner's paired phone's endpoint (payload v2)
}

func newKnockFixture(t *testing.T) *knockFixture {
	t.Helper()
	f := &knockFixture{handoffSeam: newHandoffSeam(t, hoZone), store: &memPush{}, pusher: &knockPusher{}, logs: &syncBuffer{},
		browser: ep("knock-browser"), phone: ep("knock-phone")}
	f.srv.ext.Push = push.NewService(f.store, f.pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	f.srv.logger = log.New(f.logs, "", 0)
	form := appForm("knock-app", hoEntry)
	form.Set("app_label", knockLabel)
	var rec *httptest.ResponseRecorder
	if f.tok, f.id, rec = f.create(f.a, form); f.tok == "" {
		t.Fatalf("minting the app plugin: %d %s", rec.Code, rec.Body.String())
	}
	if f.msgTok, _, rec = f.create(f.a, url.Values{"name": {"knock-messages"}, "label": {"Example inbox"}, "data_scope": {"example"}}); f.msgTok == "" {
		t.Fatalf("minting the messages plugin: %d %s", rec.Code, rec.Body.String())
	}
	claim(t, f.store, f.a, f.browser)
	f.store.mu.Lock()
	f.store.nextID++
	f.store.devs = append(f.store.devs, push.Device{ID: f.store.nextID, OwnerID: f.a, Kind: push.KindAndroid,
		Sub: func() (s push.Subscription) { s.Endpoint, s.Keys.P256dh, s.Keys.Auth = f.phone, "k", "a"; return }()})
	f.store.mu.Unlock()
	return f
}

// knock posts body to POST /plugin/v1/notify with tok and waits for the fan-out.
func (f *knockFixture) knock(tok, body string) *httptest.ResponseRecorder {
	rec := f.do("POST", "/plugin/v1/notify", body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) })
	f.srv.WaitForPushes()
	return rec
}

func (f *knockFixture) withdraw(tok, ref string) *httptest.ResponseRecorder {
	rec := f.do("DELETE", "/plugin/v1/notify/"+ref, "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) })
	f.srv.WaitForPushes()
	return rec
}

func knockBody(ref string, count int) string {
	return `{"kind":"knock","ref":"` + ref + `","count":` + strconv.Itoa(count) + `}`
}

func (f *knockFixture) tag(ref string) string {
	return "app-" + strconv.FormatInt(f.id, 10) + "-" + ref
}

// TestAKnockIsValidatedFieldByField: kind exactly "knock", ref 1-64 of
// [A-Za-z0-9_-], count 1-99 (0 and 100 refused, 1 and 99 sent), and no field
// but the three: each refusal is a 400 that sends nothing; each control sends
// exactly one push to each of the owner's two devices.
func TestAKnockIsValidatedFieldByField(t *testing.T) {
	f := newKnockFixture(t)
	for name, body := range map[string]string{
		"no kind":             `{"ref":"r","count":1}`,
		"kind Knock":          `{"kind":"Knock","ref":"r","count":1}`,
		"kind message":        `{"kind":"message","ref":"r","count":1}`,
		"no ref":              `{"kind":"knock","count":1}`,
		"an empty ref":        knockBody("", 1),
		"a 65-character ref":  knockBody(strings.Repeat("a", 65), 1),
		"a ref with a slash":  knockBody("a/b", 1),
		"a ref of ..":         knockBody("..", 1),
		"a ref with a space":  knockBody("a b", 1),
		"a ref with a dot":    knockBody("a.b", 1),
		"count 0":             knockBody("r", 0),
		"count 100":           knockBody("r", 100),
		"count -1":            knockBody("r", -1),
		"count 2.5":           `{"kind":"knock","ref":"r","count":2.5}`,
		"count as a string":   `{"kind":"knock","ref":"r","count":"2"}`,
		"no count":            `{"kind":"knock","ref":"r"}`,
		"a guest's name":      `{"kind":"knock","ref":"r","count":1,"name":"Ada"}`,
		"a title":             `{"kind":"knock","ref":"r","count":1,"title":"Ada is here"}`,
		"a body":              `{"kind":"knock","ref":"r","count":1,"body":"Ada is here"}`,
		"two objects":         knockBody("r", 1) + knockBody("s", 1),
		"not JSON":            `kind=knock`,
		"an array":            `[` + knockBody("r", 1) + `]`,
		"a nested extra data": `{"kind":"knock","ref":"r","count":1,"data":{"url":"/x"}}`,
	} {
		if rec := f.knock(f.tok, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", name, rec.Code, rec.Body.String())
		}
	}
	if n := f.pusher.total(); n != 0 {
		t.Fatalf("the refused knocks sent %d push(es); want none", n)
	}
	for i, body := range []string{knockBody("count-1", 1), knockBody("count-99", 99), knockBody(strings.Repeat("Z", 64), 2), knockBody("Az09_-", 3)} {
		if rec := f.knock(f.tok, body); rec.Code != http.StatusAccepted {
			t.Fatalf("CONTROL %s: %d %s; want 202", body, rec.Code, rec.Body.String())
		}
		if b, p := len(f.pusher.to(f.browser)), len(f.pusher.to(f.phone)); b != i+1 || p != i+1 {
			t.Fatalf("CONTROL %s: browser has %d, phone %d push(es); want %d each", body, b, p, i+1)
		}
	}
}

// TestAKnockNeedsTheAppCapability: a live token without `app` is refused 403
// naming the capability on both routes, and nothing is sent; the owner's app
// token is the control.
func TestAKnockNeedsTheAppCapability(t *testing.T) {
	f := newKnockFixture(t)
	rec := f.knock(f.msgTok, knockBody("r", 1))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `\"app\" capability`) {
		t.Fatalf("a messages-only token on POST /plugin/v1/notify: %d %s; want 403 naming app", rec.Code, rec.Body.String())
	}
	if rec := f.withdraw(f.msgTok, "r"); rec.Code != http.StatusForbidden {
		t.Fatalf("a messages-only token on DELETE /plugin/v1/notify/r: %d; want 403", rec.Code)
	}
	if n := f.pusher.total(); n != 0 {
		t.Fatalf("the refused token sent %d push(es); want none", n)
	}
	if rec := f.knock(f.tok, knockBody("r", 1)); rec.Code != http.StatusAccepted || f.pusher.total() != 2 {
		t.Fatalf("CONTROL: the app token: %d, %d push(es); want 202 and one per device", rec.Code, f.pusher.total())
	}
}

// TestAKnocksTextTagAndTapAreMusters: the push is the owner's tab label over
// one of muster's two sentences — for count 1 and for 3 — under the tag
// app-<plugin id>-<ref>, opening /apps/<plugin id>?ref=<ref>. Pinned as WHOLE
// payloads on both devices: payload v2 on the paired phone, the service
// worker's payload on the browser. A guest's name, or any text the plugin
// sends, never reaches either.
func TestAKnocksTextTagAndTapAreMusters(t *testing.T) {
	f := newKnockFixture(t)
	id := strconv.FormatInt(f.id, 10)
	for _, c := range []struct {
		ref   string
		count int
		body  string
	}{
		{"meet-1", 1, "Someone is waiting to join your meeting"},
		{"meet_3", 3, "3 people are waiting to join your meeting"},
	} {
		if rec := f.knock(f.tok, knockBody(c.ref, c.count)); rec.Code != http.StatusAccepted {
			t.Fatalf("knock %s: %d %s; want 202", c.ref, rec.Code, rec.Body.String())
		}
		phone := f.pusher.to(f.phone)
		wantV2 := `{"v":2,"type":"app","id":"app-` + id + `-` + c.ref + `","tag":"app-` + id + `-` + c.ref + `","title":"Quartz Desk","body":"` + c.body +
			`","url":"/apps/` + id + `?ref=` + c.ref + `"}`
		if len(phone) == 0 || phone[len(phone)-1] != wantV2 {
			t.Errorf("the phone's payload v2:\n got %v\nwant %s", phone, wantV2)
		}
		browser := f.pusher.to(f.browser)
		wantWeb := `{"type":"app","id":"app-` + id + `-` + c.ref + `","title":"Quartz Desk","body":"` + c.body + `","tag":"app-` + id + `-` + c.ref +
			`","data":{"url":"/apps/` + id + `?ref=` + c.ref + `"},"icon":"/static/icons/icon-192.png"}`
		if len(browser) == 0 || browser[len(browser)-1] != wantWeb {
			t.Errorf("the browser's payload:\n got %v\nwant %s", browser, wantWeb)
		}
	}
	// A label the title rule would rewrite ("1. " reads as a list item) is
	// still the title, verbatim.
	form := appForm("knock-app-listy", hoEntry)
	form.Set("app_label", "1. Desk - Ops")
	listy, listyID, rec := f.create(f.a, form)
	if listy == "" {
		t.Fatalf("minting the second app plugin: %d", rec.Code)
	}
	if rec := f.knock(listy, knockBody("l", 1)); rec.Code != http.StatusAccepted {
		t.Fatalf("knock from the second plugin: %d", rec.Code)
	}
	phone := f.pusher.to(f.phone)
	lid := strconv.FormatInt(listyID, 10)
	if want := `{"v":2,"type":"app","id":"app-` + lid + `-l","tag":"app-` + lid + `-l","title":"1. Desk - Ops","body":"Someone is waiting to join your meeting","url":"/apps/` + lid + `?ref=l"}`; phone[len(phone)-1] != want {
		t.Errorf("a label the title rule would rewrite:\n got %s\nwant %s", phone[len(phone)-1], want)
	}

	// The plugin's own words are refused, never carried.
	for _, body := range []string{
		`{"kind":"knock","ref":"meet-9","count":1,"name":"Ada Lovelace"}`,
		`{"kind":"knock","ref":"meet-9","count":1,"body":"Ada Lovelace is waiting"}`,
		`{"kind":"knock","ref":"meet-9","count":1,"title":"Ada Lovelace"}`,
	} {
		if rec := f.knock(f.tok, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d; want 400", body, rec.Code)
		}
	}
	for _, ep := range []string{f.phone, f.browser} {
		got := f.pusher.to(ep)
		if len(got) != 3 {
			t.Errorf("%s received %d push(es); want the 3 knocks", ep, len(got))
		}
		for _, b := range got {
			if strings.Contains(b, "Ada") {
				t.Errorf("a push carries the plugin's text: %s", b)
			}
		}
	}
}

// TestAKnockSendsNothingWhenTheOwnerCannotHearIt: the owner's "An app needs
// you" toggle off answers 204 and sends nothing (logged as a dropped push);
// an owner with no device 204 (logged); a server with no push 503. The
// control: the toggle back on sends.
func TestAKnockSendsNothingWhenTheOwnerCannotHearIt(t *testing.T) {
	f := newKnockFixture(t)
	f.store.mu.Lock()
	f.store.muted = map[int64][]string{f.a: {push.EventApp}}
	f.store.mu.Unlock()
	if rec := f.knock(f.tok, knockBody("off", 1)); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("the toggle off: %d %q; want 204 and no body", rec.Code, rec.Body.String())
	}
	if n := f.pusher.total(); n != 0 {
		t.Fatalf("the toggle off sent %d push(es); want none", n)
	}
	if want := "push: dropped tag=" + f.tag("off") + " event=app owner=" + strconv.FormatInt(f.a, 10) + ": muted"; !strings.Contains(f.logs.String(), want) {
		t.Errorf("the muted knock was not logged as a drop (%q):\n%s", want, f.logs.String())
	}
	// Another event muted does not mute this one: the control.
	f.store.mu.Lock()
	f.store.muted = map[int64][]string{f.a: {push.EventAgentNotify}}
	f.store.mu.Unlock()
	if rec := f.knock(f.tok, knockBody("on", 1)); rec.Code != http.StatusAccepted || f.pusher.total() != 2 {
		t.Fatalf("CONTROL: the toggle on: %d, %d push(es); want 202 and one per device", rec.Code, f.pusher.total())
	}

	// No device: the other owner's app plugin.
	form := appForm("knock-app-b", hoEntry)
	form.Set("app_label", knockLabel)
	tokB, idB, rec := f.create(f.b, form)
	if tokB == "" {
		t.Fatalf("minting b's app plugin: %d", rec.Code)
	}
	if rec := f.knock(tokB, knockBody("lonely", 2)); rec.Code != http.StatusNoContent {
		t.Fatalf("an owner with no device: %d %s; want 204", rec.Code, rec.Body.String())
	}
	if want := "push: dropped tag=app-" + strconv.FormatInt(idB, 10) + "-lonely event=app owner=" + strconv.FormatInt(f.b, 10) + ": no device is subscribed"; !strings.Contains(f.logs.String(), want) {
		t.Errorf("the deviceless knock was not logged as a drop (%q)", want)
	}
	if n := f.pusher.total(); n != 2 {
		t.Fatalf("b's knock sent something: %d push(es) in all; want the control's 2", n)
	}

	f.srv.ext.Push = nil
	if rec := f.knock(f.tok, knockBody("nopush", 1)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("push not configured: %d; want 503", rec.Code)
	}
	if rec := f.withdraw(f.tok, "nopush"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a withdrawal with push not configured: %d; want 503", rec.Code)
	}
}

// TestAKnockIsCapped: one send per (plugin, ref) per 30 s and 30 per plugin
// per hour, on the server's clock. Past either the answer is 429 with
// Retry-After, nothing is sent, and the drop is logged. The bounds OVERSHOOT
// each window: a second knock 29 s after the first is refused and one 31 s
// after is sent; 30 refs spread over 58 minutes are sent, the 31st at 59
// minutes refused (Retry-After 60), and at 60 minutes and 1 s sent.
func TestAKnockIsCapped(t *testing.T) {
	f := newKnockFixture(t)
	t0 := f.now
	logged := func(ref string) bool {
		return strings.Contains(f.logs.String(), "push: dropped tag="+f.tag(ref)+" event=app owner="+strconv.FormatInt(f.a, 10)+": the app's cap")
	}

	// Per ref.
	if rec := f.knock(f.tok, knockBody("same", 1)); rec.Code != http.StatusAccepted {
		t.Fatalf("the first knock: %d", rec.Code)
	}
	f.now = t0.Add(29 * time.Second)
	rec := f.knock(f.tok, knockBody("same", 2))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("a second knock for the ref 29 s later: %d Retry-After %q; want 429 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	if !logged("same") {
		t.Errorf("the capped knock was not logged as a drop:\n%s", f.logs.String())
	}
	if n := len(f.pusher.to(f.phone)); n != 1 {
		t.Fatalf("the capped knock sent: the phone has %d push(es); want 1", n)
	}
	// Another ref is not this ref's cap.
	if rec := f.knock(f.tok, knockBody("other", 1)); rec.Code != http.StatusAccepted {
		t.Fatalf("CONTROL: another ref 29 s later: %d; want 202", rec.Code)
	}
	f.now = t0.Add(31 * time.Second)
	if rec := f.knock(f.tok, knockBody("same", 2)); rec.Code != http.StatusAccepted {
		t.Fatalf("CONTROL: the ref again 31 s later: %d; want 202", rec.Code)
	}

	// Per plugin: 3 sent so far, at t0, t0+29 s and t0+31 s. 27 more, two
	// minutes apart from t0+2m, make 30 within the hour (the last at t0+54m).
	for i := 1; i <= 27; i++ {
		f.now = t0.Add(time.Duration(2*i) * time.Minute)
		if rec := f.knock(f.tok, knockBody("hour-"+strconv.Itoa(i), 1)); rec.Code != http.StatusAccepted {
			t.Fatalf("knock %d of 30 at +%s: %d; want 202", i+3, f.now.Sub(t0), rec.Code)
		}
	}
	f.now = t0.Add(59 * time.Minute)
	rec = f.knock(f.tok, knockBody("hour-31", 1))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("the 31st knock in the hour: %d Retry-After %q; want 429 and 60 (the first send leaves the window at +60m)", rec.Code, rec.Header().Get("Retry-After"))
	}
	if !logged("hour-31") {
		t.Errorf("the 31st knock was not logged as a drop")
	}
	if n := len(f.pusher.to(f.phone)); n != 30 {
		t.Fatalf("the phone has %d push(es); want the 30 sent", n)
	}
	f.now = t0.Add(time.Hour + time.Second)
	if rec := f.knock(f.tok, knockBody("hour-31", 1)); rec.Code != http.StatusAccepted {
		t.Fatalf("CONTROL: once the first send left the hour: %d; want 202", rec.Code)
	}
}

// TestAKnockIsWithdrawn: DELETE /plugin/v1/notify/{ref} sends the existing
// withdrawal under the knock's own tag — a `resolved` the browser closes it
// by, and payload v2's silent resolved on the paired phone — and spends no
// cap; a ref outside the pattern is 400 and sends nothing.
func TestAKnockIsWithdrawn(t *testing.T) {
	f := newKnockFixture(t)
	if rec := f.knock(f.tok, knockBody("meet-7", 2)); rec.Code != http.StatusAccepted {
		t.Fatalf("the knock: %d", rec.Code)
	}
	for _, bad := range []string{strings.Repeat("a", 65), "a.b", "a%2Fb"} {
		if rec := f.withdraw(f.tok, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("DELETE with ref %q: %d; want 400", bad, rec.Code)
		}
	}
	if n := f.pusher.total(); n != 2 {
		t.Fatalf("the refused withdrawals sent: %d push(es); want the knock's 2", n)
	}
	for i := 0; i < 2; i++ { // twice: a withdrawal is never capped
		if rec := f.withdraw(f.tok, "meet-7"); rec.Code != http.StatusAccepted {
			t.Fatalf("DELETE /plugin/v1/notify/meet-7: %d %s; want 202", rec.Code, rec.Body.String())
		}
	}
	tag := f.tag("meet-7")
	phone := f.pusher.to(f.phone)
	if want := `{"v":2,"type":"resolved","id":"` + tag + `","tag":"` + tag + `"}`; len(phone) != 3 || phone[1] != want || phone[2] != want {
		t.Errorf("the phone's withdrawals:\n got %v\nwant two of %s", phone[1:], want)
	}
	browser := f.pusher.to(f.browser)
	var p push.Payload
	if len(browser) != 3 || json.Unmarshal([]byte(browser[1]), &p) != nil || p.Type != push.TypeResolved || p.Tag != tag || p.Title != "" || p.Body != "" {
		t.Errorf("the browser's withdrawal: %v; want a resolved under %s with no text", browser, tag)
	}
}

// TestAnotherPluginCannotWithdrawAKnock: the withdrawal's {ref} names no row —
// its tag is the PRESENTING token's own plugin's — so another owner's app
// plugin DELETEing the same ref sends A's devices nothing, and A's own plugin
// is the control that it would have.
func TestAnotherPluginCannotWithdrawAKnock(t *testing.T) {
	f := newKnockFixture(t)
	if rec := f.knock(f.tok, knockBody("shared-ref", 1)); rec.Code != http.StatusAccepted {
		t.Fatalf("A's knock: %d", rec.Code)
	}
	form := appForm("knock-app-other", hoEntry)
	form.Set("app_label", knockLabel)
	tokB, _, rec := f.create(f.b, form)
	if tokB == "" {
		t.Fatalf("minting b's app plugin: %d", rec.Code)
	}
	if rec := f.withdraw(tokB, "shared-ref"); rec.Code != http.StatusAccepted {
		t.Fatalf("b's withdrawal: %d; want 202 (its own tag)", rec.Code)
	}
	if n := f.pusher.total(); n != 2 {
		t.Fatalf("b's withdrawal reached A's devices: %d push(es); want A's knock's 2", n)
	}
	if rec := f.withdraw(f.tok, "shared-ref"); rec.Code != http.StatusAccepted || f.pusher.total() != 4 {
		t.Fatalf("CONTROL: A's own withdrawal: %d, %d push(es); want 202 and one more per device", rec.Code, f.pusher.total())
	}
}
