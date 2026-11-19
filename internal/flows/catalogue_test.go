package flows

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/plainscan"
)

// TestTheCatalogueIsExactlyMuse pins the closed set. Adding a flow is a
// decision that also needs the runner's local copy to list it, so it
// must change this line, not slip in.
func TestTheCatalogueIsExactlyMuse(t *testing.T) {
	if got := strings.Join(Names(), ","); got != "muse.poll,muse.send" {
		t.Fatalf("catalogue = %s; want muse.poll,muse.send", got)
	}
	want := map[string]Class{"muse.poll": ClassRead, "muse.send": ClassWrite}
	for name, class := range want {
		f, ok := Lookup(name)
		if !ok || f.Class != class {
			t.Errorf("%s: class %q ok=%v; want %q", name, f.Class, ok, class)
		}
	}
}

func TestValidateRefusesEverythingOutsideTheSchema(t *testing.T) {
	ok := func() Request {
		return Request{Flow: "muse.send", Host: "laptop", Instance: "personal",
			Params: map[string]string{"text": "hello\nthere", "thread": "Side chat", "task": "42"}}
	}
	cases := map[string]func(r *Request){
		"unknown flow":            func(r *Request) { r.Flow = "browser.nav" },
		"unknown param":           func(r *Request) { r.Params["url"] = "https://x.test" },
		"missing required text":   func(r *Request) { delete(r.Params, "text") },
		"empty required text":     func(r *Request) { r.Params["text"] = "" },
		"text too long":           func(r *Request) { r.Params["text"] = strings.Repeat("é", 4001) },
		"control char in text":    func(r *Request) { r.Params["text"] = "a\x1bb" },
		"text that reads as flag": func(r *Request) { r.Params["text"] = "--force" },
		"text leading dash":       func(r *Request) { r.Params["text"] = "-x" },
		"thread leading dash":     func(r *Request) { r.Params["thread"] = "--force" },
		"thread with newline":     func(r *Request) { r.Params["thread"] = "a\nb" },
		"task not a number":       func(r *Request) { r.Params["task"] = "4; rm" },
		"task zero":               func(r *Request) { r.Params["task"] = "0" },
		"host uppercase":          func(r *Request) { r.Host = "Laptop" },
		"host empty":              func(r *Request) { r.Host = "" },
		"instance with slash":     func(r *Request) { r.Instance = "a/b" },
		"ttl below the minimum":   func(r *Request) { r.TTL = 30 * time.Second },
		"ttl above the maximum":   func(r *Request) { r.TTL = 25 * time.Hour },
		"invalid utf-8":           func(r *Request) { r.Params["text"] = "a\xffb" },
		"poll takes no text":      func(r *Request) { r.Flow = "muse.poll" },
		"thread over 120 runes":   func(r *Request) { r.Params["thread"] = strings.Repeat("t", 121) },
		"instance over 32 chars":  func(r *Request) { r.Instance = strings.Repeat("i", 33) },
		"instance leading hyphen": func(r *Request) { r.Instance = "-x" },
	}
	// Positive control: the unmodified request validates, so each refusal
	// below is caused by its one change.
	r0 := ok()
	if f, err := Validate(&r0); err != nil || f.Name != "muse.send" || r0.TTL != DefaultTTL {
		t.Fatalf("control: the valid request was refused (%v) or not defaulted (ttl %s)", err, r0.TTL)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := ok()
			mutate(&r)
			if _, err := Validate(&r); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v; want ErrInvalid", err)
			}
		})
	}
}

func TestValidateDropsAnEmptyOptionalParam(t *testing.T) {
	r := Request{Flow: "muse.poll", Host: "laptop", Instance: "personal", Params: map[string]string{"thread": "", "task": "7"}}
	if _, err := Validate(&r); err != nil {
		t.Fatal(err)
	}
	if _, has := r.Params["thread"]; has || r.Params["task"] != "7" {
		t.Fatalf("params = %v; want only task=7", r.Params)
	}
}

func TestRunnerTokenShape(t *testing.T) {
	tok, err := NewRunnerToken()
	if err != nil {
		t.Fatal(err)
	}
	if !RunnerTokenShapeOK(tok) {
		t.Fatalf("a minted token %q fails its own shape check", tok)
	}
	for _, bad := range []string{"", "mst_" + strings.TrimPrefix(tok, RunnerTokenPrefix), tok + "A", RunnerTokenPrefix + strings.Repeat("!", 43)} {
		if RunnerTokenShapeOK(bad) {
			t.Errorf("RunnerTokenShapeOK(%q) = true", bad)
		}
	}
}

// Muse-dispatch parameters (session, marker, wait). The fixture values are
// pairwise distinct, and the wait is not the 3600 bound, so a swapped field or
// a hardcoded literal shows.
const (
	dispatchSession = "3f2a9c1e-8b4d-4e7a-9c3b-1d2e3f4a5b6c"
	dispatchMarker  = "md-a1b2c3"
	dispatchWait    = "917"
)

func dispatchReq() Request {
	return Request{Flow: "muse.send", Host: "laptop", Instance: "personal", Params: map[string]string{
		"text": "do the thing", "session": dispatchSession, "marker": dispatchMarker, "wait": dispatchWait, "task": "42"}}
}

func TestValidateAcceptsAMuseDispatch(t *testing.T) {
	r := dispatchReq()
	if _, err := Validate(&r); err != nil {
		t.Fatalf("a well-formed dispatch was refused: %v", err)
	}
	want := map[string]string{"text": "do the thing", "session": dispatchSession, "marker": dispatchMarker, "wait": dispatchWait, "task": "42"}
	if len(r.Params) != len(want) {
		t.Fatalf("params = %v; want %v", r.Params, want)
	}
	for k, v := range want {
		if r.Params[k] != v {
			t.Errorf("param %s = %q; want %q", k, r.Params[k], v)
		}
	}
	// The wait's numeric boundary and a low value are accepted.
	for _, w := range []string{"3600", "1", "60"} {
		r := dispatchReq()
		r.Params["wait"] = w
		if _, err := Validate(&r); err != nil {
			t.Errorf("wait %s refused: %v", w, err)
		}
	}
	// A send with neither session nor marker (the pre-dispatch shape) still works.
	plain := Request{Flow: "muse.send", Host: "laptop", Instance: "personal", Params: map[string]string{"text": "hi", "wait": "30"}}
	if _, err := Validate(&plain); err != nil {
		t.Fatalf("a plain send with a wait: %v", err)
	}
}

func TestValidateRefusesMalformedDispatchParams(t *testing.T) {
	cases := map[string]func(r *Request){
		"uppercase uuid":          func(r *Request) { r.Params["session"] = strings.ToUpper(dispatchSession) },
		"short uuid":              func(r *Request) { r.Params["session"] = dispatchSession[:35] },
		"uuid without dashes":     func(r *Request) { r.Params["session"] = strings.ReplaceAll(dispatchSession, "-", "") },
		"uuid with braces":        func(r *Request) { r.Params["session"] = "{" + dispatchSession + "}" },
		"marker 5 hex":            func(r *Request) { r.Params["marker"] = "md-12345" },
		"marker 7 hex":            func(r *Request) { r.Params["marker"] = "md-1234567" },
		"marker uppercase":        func(r *Request) { r.Params["marker"] = "md-ABCDEF" },
		"marker wrong prefix":     func(r *Request) { r.Params["marker"] = "mx-a1b2c3" },
		"wait zero":               func(r *Request) { r.Params["wait"] = "0" },
		"wait over the bound":     func(r *Request) { r.Params["wait"] = "3601" },
		"wait far over the bound": func(r *Request) { r.Params["wait"] = "9999" },
		"wait negative":           func(r *Request) { r.Params["wait"] = "-5" },
		"wait with a unit":        func(r *Request) { r.Params["wait"] = "60s" },
		"wait leading zero":       func(r *Request) { r.Params["wait"] = "060" },
		"session and thread":      func(r *Request) { r.Params["thread"] = "Side chat" },
		"marker without session":  func(r *Request) { delete(r.Params, "session") },
		"marker on muse.poll": func(r *Request) {
			r.Flow = "muse.poll"
			r.Params = map[string]string{"marker": dispatchMarker}
		},
		"wait on muse.poll": func(r *Request) {
			r.Flow = "muse.poll"
			r.Params = map[string]string{"wait": dispatchWait}
		},
		// Round-0 D4: nothing uses session on muse.poll, so neither catalogue
		// takes it (the runner's own copy refuses it too).
		"session on muse.poll": func(r *Request) {
			r.Flow = "muse.poll"
			r.Params = map[string]string{"session": dispatchSession, "task": "7"}
		},
	}
	// Each case names the refusal it must produce, so a case refused for some
	// OTHER reason (an earlier check winning) does not pass as this one.
	wantMsg := map[string]string{
		"session and thread":      `"session" and "thread" are exclusive`,
		"session on muse.poll":    `takes no parameter "session"`,
		"marker without session":  `"marker" needs "session"`,
		"marker on muse.poll":     `takes no parameter "marker"`,
		"wait on muse.poll":       `takes no parameter "wait"`,
		"wait over the bound":     `the limit is 3600`,
		"wait far over the bound": `the limit is 3600`,
	}
	r0 := dispatchReq()
	if _, err := Validate(&r0); err != nil {
		t.Fatalf("control: the valid dispatch was refused: %v", err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := dispatchReq()
			mutate(&r)
			_, err := Validate(&r)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v; want ErrInvalid", err)
			}
			if m := wantMsg[name]; m != "" && !strings.Contains(err.Error(), m) {
				t.Fatalf("Validate = %v; want the refusal %q", err, m)
			}
		})
	}
}

// TestEveryFlowAndParamHasAPlainName: the /flows form names a flow by its
// PlainName and a parameter by its Label, so each must be set and plain (the
// shared plain scan); the technical Help stays for the ⓘ.
func TestEveryFlowAndParamHasAPlainName(t *testing.T) {
	if plainscan.Jargon("the API token") == "" {
		t.Fatal("CONTROL: the plain scan flags nothing")
	}
	for _, f := range Catalogue() {
		if f.PlainName == "" || plainscan.Jargon(f.PlainName) != "" || strings.Contains(f.PlainName, f.Name) {
			t.Errorf("flow %s: PlainName %q is empty, not plain, or its machine name", f.Name, f.PlainName)
		}
		for _, p := range f.Params {
			if p.Label == "" || plainscan.Jargon(p.Label) != "" || strings.EqualFold(p.Label, p.Name) {
				t.Errorf("flow %s param %s: Label %q is empty, not plain, or its raw key", f.Name, p.Name, p.Label)
			}
			if p.Help == "" {
				t.Errorf("flow %s param %s: no Help for the ⓘ", f.Name, p.Name)
			}
		}
	}
}

// TestValidationRefusalsKeepTheirMessagesAndNameTheirProblem: the page
// translates a refusal by its Problem; the API answers its message verbatim,
// and scripts and `muster flow` read that, so the message must not move.
// (The messages are an INVARIANT guard: this change never altered them.)
func TestValidationRefusalsKeepTheirMessagesAndNameTheirProblem(t *testing.T) {
	ok := func() Request {
		return Request{Flow: "muse.send", Host: "laptop", Instance: "personal", Params: map[string]string{"text": "hi"}}
	}
	cases := []struct {
		name    string
		mut     func(r *Request)
		problem Problem
		param   string
		msg     string
	}{
		{"empty host", func(r *Request) { r.Host = "" }, ProblemHost, "",
			`invalid flow request: host "" is not a host label (lowercase letters, digits and '-')`},
		{"bad instance", func(r *Request) { r.Instance = "a/b" }, ProblemInstance, "",
			`invalid flow request: instance "a/b" is not an instance label (lowercase letters, digits, '_' and '-')`},
		{"unknown flow", func(r *Request) { r.Flow = "browser.nav" }, ProblemFlow, "",
			`invalid flow request: unknown flow "browser.nav" (the catalogue: muse.poll, muse.send)`},
		{"ttl", func(r *Request) { r.TTL = 30 * time.Second }, ProblemTTL, "",
			`invalid flow request: ttl 30s is outside 1m0s..24h0m0s`},
		{"missing text", func(r *Request) { delete(r.Params, "text") }, ProblemMissing, "text",
			`invalid flow request: flow muse.send needs parameter "text"`},
		{"unknown param", func(r *Request) { r.Params["url"] = "x" }, ProblemValue, "url",
			`invalid flow request: flow muse.send takes no parameter "url"`},
		{"bad task", func(r *Request) { r.Params["task"] = "0" }, ProblemValue, "task",
			`invalid flow request: parameter "task" has a value this flow does not accept`},
		{"wait over", func(r *Request) {
			r.Params["session"] = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
			r.Params["wait"] = "3601"
		}, ProblemValue, "wait", `invalid flow request: parameter "wait" is 3601; the limit is 3600`},
		{"thread and session", func(r *Request) {
			r.Params["thread"] = "Side chat"
			r.Params["session"] = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
		}, ProblemThreadAndSession, "",
			`invalid flow request: parameters "session" and "thread" are exclusive; name the thread by one of them`},
		{"marker alone", func(r *Request) { r.Params["marker"] = "md-0f9e8d" }, ProblemMarkerNeedsSession, "",
			`invalid flow request: parameter "marker" needs "session" (a dispatch targets its thread by uuid)`},
	}
	r0 := ok()
	if _, err := Validate(&r0); err != nil {
		t.Fatalf("control: the valid request was refused: %v", err)
	}
	for _, c := range cases {
		r := ok()
		c.mut(&r)
		_, err := Validate(&r)
		var ie *InvalidError
		if !errors.Is(err, ErrInvalid) || !errors.As(err, &ie) {
			t.Errorf("%s: %v is not an InvalidError wrapping ErrInvalid", c.name, err)
			continue
		}
		if err.Error() != c.msg {
			t.Errorf("%s: message %q, want %q", c.name, err.Error(), c.msg)
		}
		if ie.Problem != c.problem || ie.Param != c.param {
			t.Errorf("%s: problem %q param %q, want %q %q", c.name, ie.Problem, ie.Param, c.problem, c.param)
		}
	}
}
