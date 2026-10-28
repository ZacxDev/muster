package officers_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseReadsOfficersAndTheirTokens(t *testing.T) {
	raw := `[{"id":"release:example-web","hq_url":"https://hq.example.com/api/v1/","max_instances":2,
	          "idle_scale_to_zero_after":"45m","poll_interval":"30s"}]`
	list, err := officers.Parse(raw, env(map[string]string{"MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB": " tok-a "}), "muster-agent-")
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
	list, err := officers.Parse(`[{"id":"a:b","hq_url":"http://hq.invalid/api/v1"}]`, env(map[string]string{"MUSTER_OFFICER_TOKEN_A_B": "x"}), "muster-agent-")
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
		"kind is gone":   {`[{"id":"a:b","hq_url":"http://h/api/v1","kind":"claude-code"}]`, "unknown field"},
		"url":            {`[{"id":"a:b","hq_url":"ftp://h"}]`, "not an http(s) URL"},
		"creds in url":   {`[{"id":"a:b","hq_url":"https://u:p@h/api/v1"}]`, "must not carry credentials"},
		"max":            {`[{"id":"a:b","hq_url":"http://h/api/v1","max_instances":9}]`, "max_instances"},
		"poll floor":     {`[{"id":"a:b","hq_url":"http://h/api/v1","poll_interval":"1s"}]`, "floor"},
		"idle":           {`[{"id":"a:b","hq_url":"http://h/api/v1","idle_scale_to_zero_after":"soon"}]`, "positive duration"},
		"no token":       {`[{"id":"x:y","hq_url":"http://h/api/v1"}]`, "MUSTER_OFFICER_TOKEN_X_Y"},
		"duplicate":      {`[{"id":"a:b","hq_url":"http://h/api/v1"},{"id":"a:b","hq_url":"http://h/api/v1"}]`, "declared twice"},
		"slug collision": {`[{"id":"a:b-c","hq_url":"http://h/api/v1"},{"id":"a-b:c","hq_url":"http://h/api/v1"}]`, "same agent slug"},
		"bad model":      {`[{"id":"a:b","hq_url":"http://h/api/v1","model":"opus now"}]`, "is not a model name"},
		"long name":      {`[{"id":"a:` + strings.Repeat("b", 40) + `","hq_url":"http://h/api/v1"}]`, "characters, over 50"},
	} {
		t.Run(name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range tok {
				m[k] = v
			}
			m["MUSTER_OFFICER_TOKEN_A_"+strings.ToUpper(strings.Repeat("b", 40))] = "x"
			m["MUSTER_OFFICER_TOKEN_A_B_C"] = "x"
			_, err := officers.Parse(c.raw, env(m), "muster-agent-")
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

// TestTheNameBoundFollowsTheNamespacePrefix: the longest instance name an
// officer may have is what a namespace (63) leaves after the deployment's
// prefix — the same id is accepted under a short prefix and refused under a
// long one.
func TestTheNameBoundFollowsTheNamespacePrefix(t *testing.T) {
	id := "release:" + strings.Repeat("x", 30) // officer-release-<30x>-1 = 47 chars
	raw := `[{"id":"` + id + `","hq_url":"http://h/api/v1"}]`
	tok := env(map[string]string{officers.TokenEnvPrefix + officers.TokenEnvSuffix(id): "t"})
	if _, err := officers.Parse(raw, tok, "muster-agent-"); err != nil { // 63-13 = 50
		t.Fatalf("47 chars under a 13-char prefix: %v", err)
	}
	_, err := officers.Parse(raw, tok, "a-very-long-namespace-prefix-") // 63-29 = 34
	if err == nil || !strings.Contains(err.Error(), "over 34") {
		t.Fatalf("47 chars under a 29-char prefix: err = %v, want a refusal naming 34", err)
	}
}

// TestParseReadsTheModel: model is optional, kept verbatim when it is a model
// name.
func TestParseReadsTheModel(t *testing.T) {
	for _, m := range []string{"opus", "claude-opus-4-5-20251101", "opus[1m]", ""} {
		raw := `[{"id":"a:b","hq_url":"http://h/api/v1","model":"` + m + `"}]`
		list, err := officers.Parse(raw, env(map[string]string{"MUSTER_OFFICER_TOKEN_A_B": "x"}), "muster-agent-")
		if err != nil || list[0].Model != m {
			t.Fatalf("model %q: %+v %v", m, list, err)
		}
	}
}
