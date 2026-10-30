package chiefnudge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestTheRubricsAreTheApprovedOnes pins the three rubric strings and the
// instructions, byte for byte, to the operator-approved, backtested ones in
// the reference ask_classifier.py. The hash was computed from that file's RUBRICS and
// INSTRUCTIONS (each label, NUL, its rubric, NUL, in label order, then the
// instructions). A reword is a different classifier than the one measured.
func TestTheRubricsAreTheApprovedOnes(t *testing.T) {
	keys := make([]string, 0, len(JevRubrics))
	for k := range JevRubrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "\x00" + JevRubrics[k] + "\x00")
	}
	b.WriteString(jevInstructions)
	sum := sha256.Sum256([]byte(b.String()))
	if got := hex.EncodeToString(sum[:]); got != "ad778451d2c7ec068b28903a0e6106b7233627d65063202fdb2c239d28fec18b" {
		t.Fatalf("the rubrics or instructions changed (hash %s)", got)
	}
	if JevModel != "~typesafe/jev-latest" || DecisionsURL != "https://openrouter.ai/api/alpha/decisions" || MaxClassifyBytes != 8192 {
		t.Fatal("the model, endpoint or input cap drifted from the reference")
	}
}

func TestTailBytesNeverSplitsARune(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	got := TailBytes(s, 5)
	if got != "éé" {
		t.Fatalf("%q", got)
	}
	if TailBytes("short", 10) != "short" {
		t.Fatal("a short text was cut")
	}
}

func jevServer(t *testing.T, status int, body string, delay time.Duration, seen *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if seen != nil {
			m := map[string]any{}
			_ = json.Unmarshal(raw, &m)
			m["_auth"] = r.Header.Get("Authorization")
			m["_path"] = r.URL.Path
			*seen = m
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const jevOK = `{"model":"typesafe/jev-1.14-test","answers":{"ask":{"type":"choice","choice":"neither",` +
	`"probabilities":{"needs_input":0.71234,"asks_permission":0.2,"neither":0.08766}}}}`

// TestClassifySendsTheReferenceBodyAndReadsTheArgmax: the request is the
// reference's (model, the text's tail as state.text, one choice question with
// the approved criteria, the owner's key); the label is the ARGMAX of the
// probabilities, not the response's own `choice`; scores are rounded to 4
// places as the reference does; the served model is recorded.
func TestClassifySendsTheReferenceBodyAndReadsTheArgmax(t *testing.T) {
	var seen map[string]any
	srv := jevServer(t, 200, jevOK, 0, &seen)
	c := &Classifier{URL: srv.URL, Key: func(context.Context, int64) (string, error) { return "sk-owner", nil }}
	long := strings.Repeat("x", MaxClassifyBytes) + "Shall I merge it?"
	r := c.Classify(context.Background(), 7, long)
	if !r.OK() || r.Label != LabelNeedsInput || r.Model != "typesafe/jev-1.14-test" ||
		r.Scores[LabelNeedsInput] != 0.7123 || r.Scores[LabelNeither] != 0.0877 || r.AskScore < 0.91229 || r.AskScore > 0.91231 {
		t.Fatalf("%+v", r)
	}
	if seen["model"] != JevModel || seen["_auth"] != "Bearer sk-owner" {
		t.Fatalf("request %v", seen)
	}
	text := seen["state"].(map[string]any)["text"].(string)
	if len(text) != MaxClassifyBytes || !strings.HasSuffix(text, "Shall I merge it?") {
		t.Fatalf("state.text is not the %d-byte tail (%d bytes)", MaxClassifyBytes, len(text))
	}
	raw, _ := BuildJevRequest("x")
	if i, j, k := strings.Index(string(raw), `"needs_input":`), strings.Index(string(raw), `"asks_permission":`), strings.Index(string(raw), `"neither":`); !(i < j && j < k) {
		t.Fatalf("the criteria are not in the reference's order: %s", raw)
	}
	q := seen["questions"].(map[string]any)["ask"].(map[string]any)
	crit := q["criteria"].(map[string]any)
	if q["type"] != "choice" || q["instructions"] != jevInstructions || len(crit) != 3 {
		t.Fatalf("question %v", q)
	}
	for k, v := range JevRubrics {
		if crit[k] != v {
			t.Fatalf("criterion %s sent as %v", k, crit[k])
		}
	}
}

// TestClassifyNeverFailsTheCaller: every failure is a Result.Err class, never
// an error or a panic, and never a usable label.
func TestClassifyNeverFailsTheCaller(t *testing.T) {
	key := func(context.Context, int64) (string, error) { return "k", nil }
	cases := []struct {
		name string
		c    *Classifier
		text string
		want string
	}{
		{"http 500", &Classifier{URL: jevServer(t, 500, "boom", 0, nil).URL, Key: key}, "Shall I?", "http-500"},
		{"bad json", &Classifier{URL: jevServer(t, 200, "{not json", 0, nil).URL, Key: key}, "Shall I?", "bad-json"},
		{"missing label", &Classifier{URL: jevServer(t, 200, `{"answers":{"ask":{"probabilities":{"needs_input":1}}}}`, 0, nil).URL, Key: key}, "Shall I?", "bad-response"},
		{"timeout", &Classifier{URL: jevServer(t, 200, jevOK, 300*time.Millisecond, nil).URL, Key: key, Timeout: 50 * time.Millisecond}, "Shall I?", "timeout"},
		{"no key", &Classifier{URL: "http://127.0.0.1:1", Key: func(context.Context, int64) (string, error) { return "", nil }}, "Shall I?", "no-key"},
		{"missing key", &Classifier{URL: "http://127.0.0.1:1", Missing: func(error) bool { return true },
			Key: func(context.Context, int64) (string, error) { return "", errors.New("none") }}, "Shall I?", "no-key"},
		{"key lookup", &Classifier{URL: "http://127.0.0.1:1", Key: func(context.Context, int64) (string, error) { return "", errors.New("db down") }}, "Shall I?", "key-lookup"},
		{"empty text", &Classifier{Key: key}, "  ", "empty-text"},
		{"nil classifier", nil, "Shall I?", "no-key"},
	}
	for _, c := range cases {
		r := c.c.Classify(context.Background(), 1, c.text)
		if r.Err != c.want || r.OK() || AskStall(r, 0) {
			t.Errorf("%s: %+v, want error %s", c.name, r, c.want)
		}
	}
}

// TestTheAskStallRuleIsTheBacktestedLine: ask score 0.91 clears 0.9, 0.89
// does not; exactly the threshold clears it.
func TestTheAskStallRuleIsTheBacktestedLine(t *testing.T) {
	r := func(score float64) Result { return Result{Label: LabelNeedsInput, AskScore: score} }
	if !AskStall(r(0.91), DefaultAskThreshold) || AskStall(r(0.89), DefaultAskThreshold) || !AskStall(r(0.9), DefaultAskThreshold) {
		t.Fatal("the 0.9 line is not where the backtest put it")
	}
}

// TestTheLabelIsTheArgmaxNotTheResponsesChoice: a response whose `choice`
// disagrees with its own probabilities cannot steer the label.
func TestTheLabelIsTheArgmaxNotTheResponsesChoice(t *testing.T) {
	label, scores, model, err := ParseJevResponse([]byte(`{"model":"m","answers":{"ask":{"choice":"needs_input",` +
		`"probabilities":{"needs_input":0.1,"asks_permission":0.2,"neither":0.7}}}}`))
	if err != nil || label != LabelNeither || scores[LabelNeither] != 0.7 || model != "m" {
		t.Fatalf("%q %v %q %v", label, scores, model, err)
	}
}
