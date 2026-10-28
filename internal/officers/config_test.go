package officers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseReadsOfficersAndTheirTokens(t *testing.T) {
	raw := `[{"id":"release:example-web","hq_url":"https://hq.example.com/api/v1/","max_instances":2,
	          "idle_scale_to_zero_after":"45m","poll_interval":"30s"}]`
	list, err := officers.Parse(raw, env(map[string]string{"MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB": " tok-a "}))
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if o.ID != "release:example-web" || o.HQURL != "https://hq.example.com/api/v1" || o.MaxInstances != 2 ||
		o.IdleAfter != 45*time.Minute || o.PollInterval != 30*time.Second || o.Owner != 1 || o.Token() != "tok-a" {
		t.Fatalf("parsed %#v token %q", o, o.Token())
	}
	// The token is never printed or marshalled.
	b, _ := json.Marshal(o)
	for _, s := range []string{o.String(), fmt.Sprintf("%v", o), fmt.Sprintf("%+v", o), fmt.Sprintf("%#v", o), string(b)} {
		if strings.Contains(s, "tok-a") {
			t.Fatalf("token printed: %s", s)
		}
	}
}

func TestParseDefaults(t *testing.T) {
	list, err := officers.Parse(`[{"id":"a:b","hq_url":"http://hq.invalid/api/v1"}]`, env(map[string]string{"MUSTER_OFFICER_TOKEN_A_B": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if o.MaxInstances != officers.DefaultMaxInstances || o.IdleAfter != officers.DefaultIdleAfter || o.PollInterval != officers.DefaultPollInterval {
		t.Fatalf("defaults: %#v", o)
	}
}

func TestParseRefusals(t *testing.T) {
	tok := map[string]string{"MUSTER_OFFICER_TOKEN_A_B": "x", "MUSTER_OFFICER_TOKEN_A_B_C": "x"}
	for name, c := range map[string]struct{ raw, want string }{
		"not json":       {`{`, "not a JSON array"},
		"unknown field":  {`[{"id":"a:b","hq_url":"http://h/api/v1","token":"inline"}]`, "unknown field"},
		"bad id":         {`[{"id":"A:b","hq_url":"http://h/api/v1"}]`, "<type>:<area>"},
		"no colon":       {`[{"id":"ab","hq_url":"http://h/api/v1"}]`, "<type>:<area>"},
		"kind":           {`[{"id":"a:b","hq_url":"http://h/api/v1","kind":"gateway"}]`, "claude-code only"},
		"url":            {`[{"id":"a:b","hq_url":"ftp://h"}]`, "not an http(s) URL"},
		"creds in url":   {`[{"id":"a:b","hq_url":"https://u:p@h/api/v1"}]`, "must not carry credentials"},
		"max":            {`[{"id":"a:b","hq_url":"http://h/api/v1","max_instances":9}]`, "max_instances"},
		"poll floor":     {`[{"id":"a:b","hq_url":"http://h/api/v1","poll_interval":"1s"}]`, "floor"},
		"idle":           {`[{"id":"a:b","hq_url":"http://h/api/v1","idle_scale_to_zero_after":"soon"}]`, "positive duration"},
		"no token":       {`[{"id":"x:y","hq_url":"http://h/api/v1"}]`, "MUSTER_OFFICER_TOKEN_X_Y"},
		"duplicate":      {`[{"id":"a:b","hq_url":"http://h/api/v1"},{"id":"a:b","hq_url":"http://h/api/v1"}]`, "declared twice"},
		"slug collision": {`[{"id":"a:b-c","hq_url":"http://h/api/v1"},{"id":"a-b:c","hq_url":"http://h/api/v1"}]`, "same agent slug"},
		"long name":      {`[{"id":"a:` + strings.Repeat("b", 40) + `","hq_url":"http://h/api/v1"}]`, "characters, over"},
	} {
		t.Run(name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range tok {
				m[k] = v
			}
			m["MUSTER_OFFICER_TOKEN_A_"+strings.ToUpper(strings.Repeat("b", 40))] = "x"
			m["MUSTER_OFFICER_TOKEN_A_B_C"] = "x"
			_, err := officers.Parse(c.raw, env(m))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestInstanceNamesAndTheBindingRule(t *testing.T) {
	a := officers.Officer{ID: "release:web", MaxInstances: 2}
	b := officers.Officer{ID: "release:web-1", MaxInstances: 3}
	reg := officers.NewRegistry([]officers.Officer{a, b})
	if got := officers.InstanceName(a.ID, 1); got != "officer-release-web-1" {
		t.Fatalf("InstanceName = %q", got)
	}
	for name, want := range map[string]string{
		"officer-release-web-1":   "release:web#1",
		"officer-release-web-2":   "release:web#2",
		"officer-release-web-3":   "", // past a's max
		"officer-release-web-1-3": "release:web-1#3",
		"officer-release-web-1-4": "", // past b's max
		"officer-release-web-01":  "",
		"officer-release-web-":    "",
		"officer-release-web":     "",
		"release-web-1":           "",
		"chief":                   "",
	} {
		o, n, ok := reg.ForAgentName(name)
		got := ""
		if ok {
			got = fmt.Sprintf("%s#%d", o.ID, n)
		}
		if got != want {
			t.Errorf("ForAgentName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestWebhookVerification(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(946728000, 0) // 2000-01-01
	body := []byte(`{"type":"officer.item_created","data":{"officer_id":"a:b","item_id":7,"kind":"k"}}`)
	h := func(id string, ts int64, sig string) http.Header {
		hh := http.Header{}
		if id != "" {
			hh.Set("webhook-id", id)
		}
		if ts != 0 {
			hh.Set("webhook-timestamp", fmt.Sprint(ts))
		}
		if sig != "" {
			hh.Set("webhook-signature", sig)
		}
		return hh
	}
	good := officers.Sign(key, "msg_1", now.Unix(), body)
	if id, err := officers.Verify(key, h("msg_1", now.Unix(), "v1,bm9wZQ== "+good), body, now); err != nil || id != "msg_1" {
		t.Fatalf("valid (second of two signatures): id %q err %v", id, err)
	}
	cases := map[string]struct {
		h    http.Header
		body []byte
		want error
	}{
		"missing sig":      {h("msg_1", now.Unix(), ""), body, officers.ErrMissingHeaders},
		"missing id":       {h("", now.Unix(), good), body, officers.ErrMissingHeaders},
		"tampered body":    {h("msg_1", now.Unix(), good), append([]byte{' '}, body...), officers.ErrBadSignature},
		"other id":         {h("msg_2", now.Unix(), good), body, officers.ErrBadSignature},
		"wrong key":        {h("msg_1", now.Unix(), officers.Sign([]byte("another-key-another-key"), "msg_1", now.Unix(), body)), body, officers.ErrBadSignature},
		"stale":            {h("msg_1", now.Add(-6*time.Minute).Unix(), officers.Sign(key, "msg_1", now.Add(-6*time.Minute).Unix(), body)), body, officers.ErrBadTimestamp},
		"future":           {h("msg_1", now.Add(6*time.Minute).Unix(), officers.Sign(key, "msg_1", now.Add(6*time.Minute).Unix(), body)), body, officers.ErrBadTimestamp},
		"v0 scheme only":   {h("msg_1", now.Unix(), "v0,"+strings.TrimPrefix(good, "v1,")), body, officers.ErrBadSignature},
		"garbage sig list": {h("msg_1", now.Unix(), "nonsense"), body, officers.ErrBadSignature},
	}
	for name, c := range cases {
		if _, err := officers.Verify(key, c.h, c.body, now); err != c.want {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	d := officers.NewDedup(4)
	if !d.First("msg_1", now) || d.First("msg_1", now) {
		t.Fatal("dedup: first copy must be new, the replay must not")
	}
	for i := 0; i < 10; i++ {
		d.First(fmt.Sprintf("m%d", i), now)
	}
	if _, err := officers.DecodeSecret("whsec_" + "c2hvcnQ="); err == nil {
		t.Fatal("a short secret must be refused")
	}
	if k, err := officers.DecodeSecret("whsec_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="); err != nil || string(k) != string(key) {
		t.Fatalf("DecodeSecret: %q %v", k, err)
	}
}
