package chiefnudge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// THE STALL CLASSIFIER: how does a stalled session's last message END?
//
// Before the chief is asked about a candidate, its last assistant message is
// classified by Jev (TypeSafe) through OpenRouter's Decisions API — one
// `choice` question over three labels whose rubrics were operator-approved and
// backtested (590 real idle stalls). The rule the sweep applies (Rule) came out
// of that backtest: when P(needs_input) + P(asks_permission) clears the
// threshold, the stall is an unasked question or permission request, and the
// default outcome is an ASK.
//
// A port of the operator's reference classifier (ask_classifier.py `classify()`): the
// same endpoint, body, model and rubric strings, so the backtest's numbers are
// this code's numbers. It never fails the sweep: every error is a Result.Err,
// and the sweep then decides with the chief alone.
// ---------------------------------------------------------------------------

// DecisionsURL is OpenRouter's Decisions API (not chat completions).
const DecisionsURL = "https://openrouter.ai/api/alpha/decisions"

// JevModel is the model asked; the response names the model actually served
// (Result.Model), which is what a decision records.
const JevModel = "~typesafe/jev-latest"

// The labels.
const (
	LabelNeedsInput     = "needs_input"
	LabelAsksPermission = "asks_permission"
	LabelNeither        = "neither"
)

const (
	jevQuestion     = "ask"
	jevInstructions = "This is the final message an AI coding assistant wrote before " +
		"ending its turn and handing control back to its human operator. " +
		"Which description fits how the message ends?"
)

// JevRubrics is THE one place the three rubric strings live — copied
// VERBATIM from ask_classifier.py's RUBRICS, which the operator approved and
// backtested. A reword is a different classifier: TestTheRubricsAreTheApprovedOnes
// pins every byte.
var JevRubrics = map[string]string{
	LabelNeedsInput: "Does the assistant end by asking the human a question, requesting " +
		"information or a decision it needs, or stating that progress is waiting on " +
		"a decision or an action only the human can take (e.g. 'still waiting on your " +
		"call', 'next step for you: …', 'once you've merged and run the SQL I'll…') — " +
		"other than asking permission to proceed with a step it has already proposed?",
	LabelAsksPermission: "Is the assistant holding its own next planned step until the human approves " +
		"it (e.g. 'shall I…', 'want me to start…', 'say proceed and I'll…', 'your call " +
		"to proceed')?",
	LabelNeither: "Is this a finished report, summary or status update — including waiting on a " +
		"background job, narrating work in progress, or ending with an OPTIONAL extra " +
		"offer after the requested work is done (e.g. 'I can look into it if you " +
		"want') — that needs nothing from the human to continue?",
}

// jevLabels is the label set, in the reference's order (argmax ties go to the
// first, as Python's max does).
var jevLabels = []string{LabelNeedsInput, LabelAsksPermission, LabelNeither}

// MaxClassifyBytes caps the text sent: its TAIL, where the ask is (Jev runs
// long inputs in a slower, less accurate mode).
const MaxClassifyBytes = 8 * 1024

// DefaultClassifyTimeout is a hard cap on one classification.
const DefaultClassifyTimeout = 5 * time.Second

// DefaultAskThreshold is the backtested ask-score line
// (MUSTER_CHIEF_NUDGE_ASK_THRESHOLD).
const DefaultAskThreshold = 0.9

// Result is one classification. On any failure Err is a short class (never a
// body or a key) and Label is "".
type Result struct {
	Label     string
	Scores    map[string]float64
	AskScore  float64 // P(needs_input) + P(asks_permission)
	Model     string  // the model the response says it served
	LatencyMS int
	Err       string
}

// OK reports a usable classification.
func (r Result) OK() bool { return r.Err == "" && r.Label != "" }

// Classifier is the Jev call. Key is the owner's OpenRouter key (the focus
// summarizer's resolution); Missing tells "no key" from a lookup failure.
type Classifier struct {
	Key     func(ctx context.Context, owner int64) (string, error)
	Missing func(error) bool
	URL     string // "" = DecisionsURL
	Client  *http.Client
	Timeout time.Duration // 0 = DefaultClassifyTimeout
}

// TailBytes is the last limit UTF-8 bytes of s, never splitting a code point.
func TailBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	t := s[len(s)-limit:]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	return t
}

type jevQuestionBody struct {
	Type         string         `json:"type"`
	Instructions string         `json:"instructions"`
	Criteria     orderedRubrics `json:"criteria"`
}

// orderedRubrics marshals JevRubrics in the REFERENCE's order (needs_input,
// asks_permission, neither), not Go's sorted map order: the criteria are a
// prompt to the model, and the parity check against the backtest measured a
// difference when they were sent sorted.
type orderedRubrics struct{}

func (orderedRubrics) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, name := range jevLabels {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(JevRubrics[name])
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type jevRequest struct {
	Model     string                     `json:"model"`
	State     map[string]string          `json:"state"`
	Questions map[string]jevQuestionBody `json:"questions"`
}

// BuildJevRequest is the Decisions request for text: the reference's body.
func BuildJevRequest(text string) ([]byte, error) {
	return json.Marshal(jevRequest{
		Model: JevModel,
		State: map[string]string{"text": TailBytes(text, MaxClassifyBytes)},
		Questions: map[string]jevQuestionBody{jevQuestion: {
			Type: "choice", Instructions: jevInstructions,
		}},
	})
}

// ParseJevResponse is (label, scores, served model) from a response. The label
// is the argmax over OUR labels, never the response's own `choice`, so a
// response cannot steer the rule with a label it was not asked about.
func ParseJevResponse(body []byte) (string, map[string]float64, string, error) {
	var resp struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Probabilities map[string]json.RawMessage `json:"probabilities"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", nil, "", errors.New("bad-json")
	}
	ans, ok := resp.Answers[jevQuestion]
	if !ok || ans.Probabilities == nil {
		return "", nil, "", errors.New("bad-response")
	}
	scores := map[string]float64{}
	label := ""
	for _, name := range jevLabels {
		var v float64
		raw, ok := ans.Probabilities[name]
		if !ok || json.Unmarshal(raw, &v) != nil {
			return "", nil, "", errors.New("bad-response")
		}
		v = math.Round(v*1e4) / 1e4 // the reference's rounding, so scores and the rule match it
		scores[name] = v
		if label == "" || v > scores[label] {
			label = name
		}
	}
	return label, scores, resp.Model, nil
}

// Classify classifies text for owner. It never returns an error: a failure is
// Result.Err (no-key, key-lookup, empty-text, timeout, network, http-<code>,
// bad-json, bad-response).
func (c *Classifier) Classify(ctx context.Context, owner int64, text string) Result {
	var r Result
	if strings.TrimSpace(text) == "" {
		r.Err = "empty-text"
		return r
	}
	if c == nil || c.Key == nil {
		r.Err = "no-key"
		return r
	}
	key, err := c.Key(ctx, owner)
	switch {
	case err != nil && c.Missing != nil && c.Missing(err):
		r.Err = "no-key"
		return r
	case err != nil:
		r.Err = "key-lookup"
		return r
	case strings.TrimSpace(key) == "":
		r.Err = "no-key"
		return r
	}
	body, err := BuildJevRequest(text)
	if err != nil {
		r.Err = "internal"
		return r
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultClassifyTimeout
	}
	url := c.URL
	if url == "" {
		url = DecisionsURL
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		r.Err = "internal"
		return r
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	start := time.Now()
	resp, err := client.Do(req)
	r.LatencyMS = int(time.Since(start).Milliseconds())
	if err != nil {
		if errors.Is(tctx.Err(), context.DeadlineExceeded) {
			r.Err = "timeout"
		} else {
			r.Err = "network"
		}
		return r
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	r.LatencyMS = int(time.Since(start).Milliseconds())
	if err != nil {
		r.Err = "network"
		return r
	}
	if resp.StatusCode/100 != 2 {
		r.Err = fmt.Sprintf("http-%d", resp.StatusCode)
		return r
	}
	label, scores, model, err := ParseJevResponse(raw)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.Label, r.Scores, r.Model = label, scores, model
	r.AskScore = scores[LabelNeedsInput] + scores[LabelAsksPermission]
	return r
}

// AskStall reports the backtested rule: the classification says the session
// stopped on an unasked question or permission request — its ask score is at
// least threshold.
func AskStall(r Result, threshold float64) bool {
	return r.OK() && r.AskScore >= threshold
}

// The GENERIC ASK: raised for an ask-stall when the chief gave no question
// (it said leave, or proposed a nudge the rule refuses). Muster puts NO session
// text in it — only the focus title (the summarizer's, which the Asks page
// already shows) and its own sentence; its only words about the session are the
// chief's own reason (GenericAskRead) — and its two options are not an answer:
// choosing one relays nothing (IsGenericChoice).
const genericAskBody = "Its last turn looks like it asked you something."

// GenericAskOptions are the generic ask's options.
var GenericAskOptions = []string{"Open session", "Dismiss"}

// MaxGenericReadRunes caps the chief's reason on the generic ask: a sentence
// or two.
const MaxGenericReadRunes = 300

// GenericAskQuestion is the generic ask's text for a session titled title,
// with GenericAskRead(read).
func GenericAskQuestion(title, read string) string {
	t := strings.TrimSpace(oneLine(title))
	if t == "" {
		t = "A session"
	}
	return t + " is waiting on you. " + genericAskBody + GenericAskRead(read)
}

// GenericAskRead is the generic ask's line for the chief's read of the
// session — read is the chief's OWN reason from its answer (Proposal.Reason),
// never the reason muster builds around it — on one line, cut to
// MaxGenericReadRunes; "" for none.
//
// It is the chief's words, held as the normal ask path holds the chief's
// question: ParseProposal's one-line cut, no check for session text. The
// generic ask's own sentence and options still carry none.
func GenericAskRead(read string) string {
	r := strings.TrimSpace(oneLine(read))
	if r == "" {
		return ""
	}
	return "\n\nThe chief's read: " + cut(r, MaxGenericReadRunes)
}

// IsGenericAsk reports whether options are the generic ask's.
func IsGenericAsk(options []string) bool {
	if len(options) != len(GenericAskOptions) {
		return false
	}
	for i := range options {
		if options[i] != GenericAskOptions[i] {
			return false
		}
	}
	return true
}

// IsGenericChoice reports whether answer, to a decision whose options are
// options, is one of the generic ask's options — which is not an answer to
// forward to the session.
func IsGenericChoice(options []string, answer string) bool {
	if !IsGenericAsk(options) {
		return false
	}
	a := strings.TrimSpace(answer)
	for _, o := range GenericAskOptions {
		if a == o {
			return true
		}
	}
	return false
}
