package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// officer_ask_person: an officer instance asks a named HQ member a question
// about an item it holds (HQ's ask-images and items/{item}/asks routes).
//
// The fixtures' sizes, logins and ids are literals, not the package's
// constants, so a changed bound is a failing test rather than a moved target.
// ---------------------------------------------------------------------------

// The image fixtures: the smallest byte strings http.DetectContentType names
// as each accepted type, padded so no two are the same length.
var (
	askPNG  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 40)...)
	askJPEG = append([]byte("\xFF\xD8\xFF"), bytes.Repeat([]byte{2}, 61)...)
	askWebP = append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{3}, 83)...)
)

// askPNGOf is a PNG fixture of exactly n bytes.
func askPNGOf(n int) []byte {
	return append(append([]byte(nil), askPNG[:8]...), bytes.Repeat([]byte{4}, n-8)...)
}

// askImageServer serves the image URLs the tests fetch.
func askImageServer(t *testing.T) *httptest.Server {
	t.Helper()
	serve := func(ct string, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write(body)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/shots/header.png", serve("image/png", askPNG))
	mux.Handle("/webp", serve("image/webp", askWebP))
	mux.Handle("/notes.txt", serve("text/plain; charset=utf-8", []byte("not a picture")))
	mux.Handle("/lies.png", serve("image/png", []byte("<html><body>a page, not a picture</body></html>")))
	mux.Handle("/big.png", serve("image/png", askPNGOf(5<<20+1)))
	mux.Handle("/exact.png", serve("image/png", askPNGOf(5<<20)))
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/to-png", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/shots/header.png", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// askRig is an officer rig with officer B's instance 1 holding a claimed
// item, a member "design-lead" in HQ, and image URLs fetched with a plain
// client (the test server is on loopback, which production refuses:
// TestAnAskImageURLMayReachPublicAddressesOnly).
func askRig(t *testing.T) (*officerRig, agents.Agent, string) {
	t.Helper()
	r := newOfficerRig(t)
	r.s.officerRT.askHTTP = &http.Client{}
	inst := r.addInstance(r.b, 1, 901)
	item := r.hq.AddItem(offB, "triage.look", "svc:ask")
	r.hq.AddMember("design-lead")
	if out := r.s.dispatchAgentTool(context.Background(), inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	r.hq.ResetRequests()
	return r, inst, item
}

func askArgs(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAnAskSendsHQTheContractBodyAndEndsTheLease: each image is uploaded as
// its raw bytes with its sniffed Content-Type and a base filename, in order,
// with the officer's own token; then the ask is created with EXACTLY the
// contract's body — instance, to, question, options, the upload ids in order,
// context_url and deadline_seconds. The result is HQ's answer plus the note
// that the lease has ENDED; HQ's item is waiting with no holder. A second ask
// on the waiting item is allowed and sends only what was given.
func TestAnAskSendsHQTheContractBodyAndEndsTheLease(t *testing.T) {
	r, inst, item := askRig(t)
	img := askImageServer(t)
	ctx := context.Background()
	out := r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, askArgs(t, map[string]any{
		"item_id": item, "to": "design-lead", "question": "Which header reads better?",
		"options":     []string{"the left one", "the right one"},
		"images":      []string{img.URL + "/shots/header.png", "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(askJPEG), img.URL + "/webp"},
		"context_url": "https://pr.example/42", "deadline": "4h",
	}))
	reqs := r.hq.Requests()
	if len(reqs) != 4 {
		t.Fatalf("%d HQ requests, want 3 uploads and 1 create: %+v\nresult %s", len(reqs), reqs, out)
	}
	uploads := []struct {
		filename, ct string
		data         []byte
	}{{"header.png", "image/png", askPNG}, {"image-2.jpg", "image/jpeg", askJPEG}, {"webp.webp", "image/webp", askWebP}}
	for i, want := range uploads {
		q := reqs[i]
		if q.Method != http.MethodPost || q.Path != "/api/v1/officers/"+offB+"/ask-images" {
			t.Errorf("upload %d: %s %s, want POST /api/v1/officers/%s/ask-images", i, q.Method, q.Path, offB)
		}
		v, _ := url.ParseQuery(q.Query)
		if v.Get("filename") != want.filename || len(v) != 1 {
			t.Errorf("upload %d: query %q, want exactly filename=%s", i, q.Query, want.filename)
		}
		if q.ContentType != want.ct || !bytes.Equal(q.Raw, want.data) {
			t.Errorf("upload %d: Content-Type %q and %d bytes, want %s and the fixture's %d bytes", i, q.ContentType, len(q.Raw), want.ct, len(want.data))
		}
		if q.Auth != "Bearer "+offBTok {
			t.Errorf("upload %d carried Authorization %q, want officer B's token", i, q.Auth)
		}
	}
	c := reqs[3]
	if c.Method != http.MethodPost || c.Path != "/api/v1/officers/"+offB+"/items/"+item+"/asks" || c.Auth != "Bearer "+offBTok {
		t.Fatalf("create: %s %s (auth %q)", c.Method, c.Path, c.Auth)
	}
	want := map[string]any{
		"instance": inst.Name, "to": "design-lead", "question": "Which header reads better?",
		"options": []any{"the left one", "the right one"}, "images": []any{"att-1", "att-2", "att-3"},
		"context_url": "https://pr.example/42", "deadline_seconds": 14400,
	}
	if got, w := normJSON(t, c.Body), normJSON(t, want); got != w {
		t.Fatalf("create body:\n%s\nwant\n%s", got, w)
	}
	if isToolErr(out) {
		t.Fatalf("ask: %s", out)
	}
	var res struct {
		Ask  map[string]any `json:"ask"`
		Item map[string]any `json:"item"`
		Note string         `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result is not JSON: %s", out)
	}
	if res.Ask["id"] != "ask-1" || res.Item["status"] != "waiting" {
		t.Errorf("result does not carry HQ's ask and item: %s", out)
	}
	for _, s := range []string{"ENDED", "do not heartbeat, complete or release", "re-opens", "escalates", "same item_id"} {
		if !strings.Contains(res.Note, s) {
			t.Errorf("the note does not say %q: %s", s, res.Note)
		}
	}
	if it := r.hq.Item(item); it.Status != "waiting" || it.ClaimedBy != "" {
		t.Fatalf("HQ's item after the ask: %+v, want waiting with no holder", it)
	}

	// A second ask on the now-waiting item: only what was given is sent.
	r.hq.ResetRequests()
	out = r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, `{"item_id":"`+item+`","to":"design-lead","question":"And the footer?"}`)
	if isToolErr(out) {
		t.Fatalf("a second ask on a waiting item: %s", out)
	}
	reqs = r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("second ask: %d HQ requests, want 1 (no images)", len(reqs))
	}
	if got, w := normJSON(t, reqs[0].Body), normJSON(t, map[string]any{"instance": inst.Name, "to": "design-lead", "question": "And the footer?"}); got != w {
		t.Fatalf("second create body:\n%s\nwant\n%s", got, w)
	}
}

// TestAnAskDeadlineIsADurationOrSeconds: deadline is a Go duration or a
// number of seconds (JSON number or numeric string), sent as
// deadline_seconds; outside 5m..72h, or not a duration, is refused before HQ.
func TestAnAskDeadlineIsADurationOrSeconds(t *testing.T) {
	r, inst, item := askRig(t)
	ctx := context.Background()
	for _, tc := range []struct {
		deadline string
		want     int
	}{{`"90m"`, 5400}, {`600`, 600}, {`"601"`, 601}, {`"72h"`, 259200}, {`"5m"`, 300}, {`"1h30m15s"`, 5415}} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, `{"item_id":"`+item+`","to":"design-lead","question":"q","deadline":`+tc.deadline+`}`)
		reqs := r.hq.Requests()
		if isToolErr(out) || len(reqs) != 1 {
			t.Fatalf("deadline %s: %s (%d requests)", tc.deadline, out, len(reqs))
		}
		if got := reqs[0].Body["deadline_seconds"]; got != float64(tc.want) {
			t.Errorf("deadline %s sent deadline_seconds %v, want %d", tc.deadline, got, tc.want)
		}
	}
	for _, bad := range []string{`"4m59s"`, `299`, `"73h"`, `259201`, `"soon"`, `1.5`, `-300`, `true`} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, `{"item_id":"`+item+`","to":"design-lead","question":"q","deadline":`+bad+`}`)
		if !isToolErr(out) || !strings.Contains(out, "deadline must be") {
			t.Errorf("deadline %s: %s, want the deadline refusal", bad, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("deadline %s reached HQ (%d requests)", bad, n)
		}
	}
}

// TestAnAskIsCheckedBeforeHQ: a malformed ask is refused before any request —
// no upload of its images either.
func TestAnAskIsCheckedBeforeHQ(t *testing.T) {
	r, inst, item := askRig(t)
	img := askImageServer(t)
	ctx := context.Background()
	png := img.URL + "/shots/header.png"
	base := func(over map[string]any) string {
		m := map[string]any{"item_id": item, "to": "design-lead", "question": "q", "images": []string{png}}
		for k, v := range over {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return askArgs(t, m)
	}
	for _, tc := range []struct {
		name, args, want string
	}{
		{"no item", base(map[string]any{"item_id": nil}), "item_id is required"},
		{"a path-shaped item", base(map[string]any{"item_id": "../1"}), "item_id is required"},
		{"no recipient", base(map[string]any{"to": nil}), "GitHub login"},
		{"a recipient that is no login", base(map[string]any{"to": "design lead"}), "GitHub login"},
		{"no question", base(map[string]any{"question": "  "}), "question is required"},
		{"a question over 2000 characters", base(map[string]any{"question": strings.Repeat("é", 2001)}), "at most 2000"},
		{"seven options", base(map[string]any{"options": []string{"a", "b", "c", "d", "e", "f", "g"}}), "at most 6 options"},
		{"an option over 40 characters", base(map[string]any{"options": []string{strings.Repeat("o", 41)}}), "1-40 characters"},
		{"an empty option", base(map[string]any{"options": []string{"a", " "}}), "1-40 characters"},
		{"a repeated option", base(map[string]any{"options": []string{"yes", "no", "yes"}}), "unique"},
		{"a context_url that is not http(s)", base(map[string]any{"context_url": "javascript:alert(1)"}), "context_url must be"},
		{"five images", base(map[string]any{"images": []string{png, png, png, png, png}}), "at most 4 images"},
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, tc.args)
		if !isToolErr(out) || !strings.Contains(out, tc.want) {
			t.Errorf("%s: %s, want a refusal saying %q", tc.name, out, tc.want)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s reached HQ (%d requests)", tc.name, n)
		}
	}
	// Positive control: the same base with nothing overridden is an ask.
	r.hq.ResetRequests()
	if out := r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, base(nil)); isToolErr(out) || len(r.hq.Requests()) != 2 {
		t.Fatalf("the valid base ask: %s (%d requests, want upload + create)", out, len(r.hq.Requests()))
	}
}

// TestAskImagesAreFetchedCheckedAndBounded: an images entry is an http(s)
// URL or a base64 data: URI. A workspace path (muster cannot read the pod's
// files) and any other scheme are refused; a URL whose server says it is not
// an image, whose bytes are not PNG/JPEG/WebP, that is over 5 MiB, or that
// redirects off http(s) is refused; so is a data: URI that is not a base64
// image or is over 5 MiB. Nothing reaches HQ for any of them — not even the
// images before the bad one. Exactly 5 MiB, and a redirect to another http
// URL, are accepted.
func TestAskImagesAreFetchedCheckedAndBounded(t *testing.T) {
	r, inst, item := askRig(t)
	img := askImageServer(t)
	ctx := context.Background()
	good := img.URL + "/shots/header.png"
	ask := func(src string) string {
		return r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, askArgs(t, map[string]any{
			"item_id": item, "to": "design-lead", "question": "q", "images": []string{good, src}}))
	}
	for _, tc := range []struct{ name, src, want string }{
		{"a workspace path", "shots/header.png", "cannot read files in your workspace"},
		{"a .. escape", "../../etc/passwd", "cannot read files in your workspace"},
		{"an absolute path", "/data/workspace/header.png", "cannot read files in your workspace"},
		{"a file: URL", "file:///data/workspace/header.png", "cannot read files in your workspace"},
		{"an ftp URL", "ftp://files.example/header.png", "cannot read files in your workspace"},
		{"a URL whose server says text", img.URL + "/notes.txt", "not an image"},
		{"a URL whose bytes are no image", img.URL + "/lies.png", "not a PNG, JPEG or WebP"},
		{"a URL over 5 MiB", img.URL + "/big.png", "over 5 MiB"},
		{"a redirect to a file: URL", img.URL + "/to-file", "redirect"},
		{"a URL that is not found", img.URL + "/missing.png", "answered 404"},
		{"a data: URI over 5 MiB", "data:image/png;base64," + base64.StdEncoding.EncodeToString(askPNGOf(5<<20+1)), "over 5 MiB"},
		{"a data: URI of text", "data:text/plain;base64,aGVsbG8=", "must carry an image"},
		{"a data: URI that is not base64", "data:image/png,rawbytes", "base64"},
		{"a data: URI whose bytes are no image", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("plain text, really")), "not a PNG, JPEG or WebP"},
	} {
		r.hq.ResetRequests()
		out := ask(tc.src)
		if !isToolErr(out) || !strings.Contains(out, tc.want) || !strings.Contains(out, "images[1]") {
			t.Errorf("%s: %s, want a refusal of images[1] saying %q", tc.name, out, tc.want)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s: %d HQ requests, want none (the good image before it is not uploaded either)", tc.name, n)
		}
	}
	// Exactly 5 MiB, and a PNG reached through an http redirect, are uploaded.
	r.hq.ResetRequests()
	if out := ask(img.URL + "/exact.png"); isToolErr(out) {
		t.Fatalf("a 5 MiB image: %s", out)
	}
	if imgs := r.hq.AskImages(); len(imgs) != 2 || len(imgs[1].Data) != 5<<20 {
		t.Fatalf("uploaded %d images, want 2 with the second exactly 5 MiB", len(imgs))
	}
	r.hq.ResetRequests()
	if out := ask(img.URL + "/to-png"); isToolErr(out) {
		t.Fatalf("an image behind an http redirect: %s", out)
	}
	if imgs := r.hq.AskImages(); len(imgs) != 4 || !bytes.Equal(imgs[3].Data, askPNG) || imgs[3].Filename != "header.png" {
		t.Fatalf("the redirected image was not uploaded as header.png with the fixture's bytes (%d images uploaded)", len(imgs))
	}
}

// TestAnAskImageURLMayReachPublicAddressesOnly: with the production client
// (no AskImageHTTP), an image URL on a loopback address is refused at the
// dial, and nothing reaches HQ. Positive control: the same URL with a plain
// client is fetched and uploaded, so the refusal is the policy's, not the URL's.
func TestAnAskImageURLMayReachPublicAddressesOnly(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.b, 1, 902)
	item := r.hq.AddItem(offB, "triage.look", "svc:ask")
	r.hq.AddMember("design-lead")
	ctx := context.Background()
	if out := r.s.dispatchAgentTool(ctx, inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatal(out)
	}
	img := askImageServer(t)
	args := askArgs(t, map[string]any{"item_id": item, "to": "design-lead", "question": "q", "images": []string{img.URL + "/shots/header.png"}})
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, args)
	if !isToolErr(out) || !strings.Contains(out, "non-public address") {
		t.Fatalf("a loopback image URL with the production client: %s, want the non-public refusal", out)
	}
	if n := len(r.hq.Requests()); n != 0 {
		t.Fatalf("a refused image URL reached HQ (%d requests)", n)
	}
	r.s.officerRT.askHTTP = &http.Client{}
	if out := r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, args); isToolErr(out) || len(r.hq.AskImages()) != 1 {
		t.Fatalf("positive control (plain client): %s, %d images uploaded", out, len(r.hq.AskImages()))
	}
}

// askRefusal decodes a refused ask's tool result.
func askRefusal(t *testing.T, out string) (msg string, status float64, code string) {
	t.Helper()
	var r struct {
		Error   string `json:"error"`
		Refused struct {
			Status float64        `json:"status"`
			Code   string         `json:"code"`
			HQ     map[string]any `json:"hq"`
		} `json:"refused"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || !isToolErr(out) {
		t.Fatalf("not a tool error: %s", out)
	}
	return r.Error, r.Refused.Status, r.Refused.Code
}

// TestAnHQWithoutAsksSaysSoAndItsRefusalsCarryTheirCode: an HQ whose router
// has no asks routes (404 "no such route") gives the instance a clear "not
// supported yet — escalate instead" error, whether the upload or the create
// meets it; any other 404 (an unknown item) is HQ's own message, with its
// status. 409, 422 and 429 come back with HQ's status and code, in the error
// text and in refused.
func TestAnHQWithoutAsksSaysSoAndItsRefusalsCarryTheirCode(t *testing.T) {
	r, inst, item := askRig(t)
	img := askImageServer(t)
	ctx := context.Background()
	ask := func(item, to string, images ...string) string {
		m := map[string]any{"item_id": item, "to": to, "question": "q"}
		if len(images) > 0 {
			m["images"] = images
		}
		return r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, askArgs(t, m))
	}

	r.hq.SetNoAsks(true)
	for _, tc := range []struct {
		name   string
		images []string
		reqs   int
	}{{"the upload", []string{img.URL + "/shots/header.png"}, 1}, {"the create", nil, 1}} {
		r.hq.ResetRequests()
		out := ask(item, "design-lead", tc.images...)
		if !isToolErr(out) || !strings.Contains(out, "does not support officer_ask_person yet") ||
			!strings.Contains(out, "officer_complete") || !strings.Contains(out, "escalated") {
			t.Errorf("an HQ without asks, met at %s: %s, want the not-supported error", tc.name, out)
		}
		if n := len(r.hq.Requests()); n != tc.reqs {
			t.Errorf("%s: %d HQ requests, want %d", tc.name, n, tc.reqs)
		}
	}
	r.hq.SetNoAsks(false)

	// Another 404: HQ's own message, not the not-supported one.
	out := ask("999", "design-lead")
	if msg, status, _ := askRefusal(t, out); status != 404 || !strings.Contains(out, "no such item") || strings.Contains(msg, "does not support") {
		t.Errorf("an unknown item: %s, want HQ's 404 with its own message", out)
	}

	// 422 unknown_recipient, from the fake's own rule.
	if msg, status, code := askRefusal(t, ask(item, "nobody-here")); status != 422 || code != "unknown_recipient" || !strings.Contains(msg, "422 unknown_recipient") || !strings.Contains(msg, "no HQ member nobody-here") {
		t.Errorf("an unknown recipient: %q %v %q", msg, status, code)
	}
	// 409 lease_lost: an item this instance never claimed.
	other := r.hq.AddItem(offB, "triage.look", "svc:other")
	if msg, status, code := askRefusal(t, ask(other, "design-lead")); status != 409 || code != "lease_lost" || !strings.Contains(msg, "409 lease_lost") {
		t.Errorf("an unclaimed item: %q %v %q", msg, status, code)
	}
	// Forced: 409 too_many_open, 422 recipient_muted, 429 rate_limited.
	for _, tc := range []struct {
		status int
		code   string
	}{{409, "too_many_open"}, {422, "recipient_muted"}, {429, "rate_limited"}} {
		r.hq.RefuseAsks(tc.status, tc.code, "refused by the fake: "+tc.code)
		msg, status, code := askRefusal(t, ask(item, "design-lead"))
		if int(status) != tc.status || code != tc.code || !strings.Contains(msg, "refused by the fake: "+tc.code) {
			t.Errorf("HQ %d %s: %q %v %q", tc.status, tc.code, msg, status, code)
		}
	}
	r.hq.RefuseAsks(0, "", "")
	if it := r.hq.Item(item); it.Status != "claimed" || it.ClaimedBy != inst.Name {
		t.Fatalf("a refused ask changed HQ's item: %+v", it)
	}
}
