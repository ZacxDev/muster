package mirrorhost

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// HOW AN IDLE OPENCODE WINDOW'S LATEST TURN ENDED (opencode_turnerror.go):
// expected values are literals worked out from the fixture's times — prompt
// at T, failure written d ms later — with d on either side of
// mirror.QuickTurnMS (10000) and never equal to it.

func ocMsgs(t *testing.T, ss ...string) []opencodehost.Message {
	t.Helper()
	var out []opencodehost.Message
	for _, s := range ss {
		var m opencodehost.Message
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		out = append(out, m)
	}
	return out
}

const (
	teUser1   = `{"id":"msg_u1","type":"user","text":"first","time":{"created":1700000100000}}`
	teFail1   = `{"id":"msg_i1","type":"idle","outcome":"failed","error":{"name":"APIError","data":{"message":"overloaded"}},"time":{"created":1700000102345}}`
	teUser2   = `{"id":"msg_u2","type":"user","text":"second","time":{"created":1700000200000}}`
	teFail2   = `{"id":"msg_i2","type":"idle","outcome":"failed","error":{"name":"ProviderAuthError"},"time":{"created":1700000201234}}`
	teOK      = `{"id":"msg_i0","type":"idle","outcome":"succeeded","time":{"created":1700000050000}}`
	ocTeReply = `{"id":"msg_a1","type":"assistant","finish":"stop","time":{"created":1700000200100,"completed":1700000200200},"content":[{"type":"text","text":"partial work"}]}`
	teSlow    = `{"id":"msg_i3","type":"idle","outcome":"failed","error":{"name":"APIError"},"time":{"created":1700000212345}}`
	teAbort   = `{"id":"msg_i4","type":"idle","outcome":"interrupted","time":{"created":1700000201000}}`
	teAsstErr = `{"id":"msg_a2","type":"assistant","finish":"error","error":{"type":"rate_limit","message":"slow down"},"time":{"created":1700000200300,"completed":1700000203456},"content":[]}`
)

// TestAnOpenCodeTurnErrorIsReadFromItsSessionsMessages: each shape, pinned.
func TestAnOpenCodeTurnErrorIsReadFromItsSessionsMessages(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []string
		want *mirror.TurnError
	}{
		{"one quick failure", []string{teOK, teUser1, teFail1}, &mirror.TurnError{Kind: "api_error", At: 1700000102345, Quick: 1}},
		{"two quick failures in a row", []string{teUser1, teFail1, teUser2, teFail2}, &mirror.TurnError{Kind: "provider_auth_error", At: 1700000201234, Quick: 2}},
		{"a failure after a real reply", []string{teUser1, teFail1, teUser2, ocTeReply, teFail2}, &mirror.TurnError{Kind: "provider_auth_error", At: 1700000201234}},
		{"a slow failure (12345 ms)", []string{teUser1, teFail1, teUser2, teSlow}, &mirror.TurnError{Kind: "api_error", At: 1700000212345}},
		{"an assistant error with no idle", []string{teUser2, teAsstErr}, &mirror.TurnError{Kind: "rate_limit", At: 1700000203456, Quick: 1}},
		{"an interrupted turn", []string{teUser1, teFail1, teUser2, teAbort}, nil},
		{"a turn that succeeded", []string{teUser1, teFail1, teUser2, ocTeReply, teOK}, nil},
		{"no prompt", []string{teFail1}, nil},
		{"a prompt not answered yet", []string{teUser1, teFail1, teUser2}, nil},
	} {
		got := openCodeTurnError(ocMsgs(t, c.in...))
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestAnOpenCodeErrorsKindIsItsNameInSnakeCase: and never "usage_limit"
// (not derived for opencode), and "error" for a name the wire cannot carry.
func TestAnOpenCodeErrorsKindIsItsNameInSnakeCase(t *testing.T) {
	for raw, want := range map[string]string{
		`{"name":"APIError"}`:              "api_error",
		`{"name":"ProviderAuthError"}`:     "provider_auth_error",
		`{"_tag":"UnknownError"}`:          "unknown_error",
		`{"type":"rate_limit"}`:            "rate_limit",
		`{"name":"usage_limit"}`:           mirror.TurnErrorOther,
		`{"name":"Bad Name! with spaces"}`: "bad_name_with_spaces",
		`{"message":"no name"}`:            mirror.TurnErrorOther,
		`null`:                             mirror.TurnErrorOther,
		// 35 characters: past the wire's 32.
		`{"name":"x2345678901234567890123456789012345"}`: mirror.TurnErrorOther,
	} {
		if got := turnErrorKindOf(json.RawMessage(raw)); got != want {
			t.Errorf("turnErrorKindOf(%s) = %q, want %q", raw, got, want)
		}
	}
}

// TestTwoQuickOpenCodeFailuresHoldTheQueue: the queue's own Decide, fed the
// window state a host now reports, holds — with the circuit breaker's words,
// as for a Claude Code window — instead of sending into the same failure.
func TestTwoQuickOpenCodeFailuresHoldTheQueue(t *testing.T) {
	e := openCodeTurnError(ocMsgs(t, teUser1, teFail1, teUser2, teFail2))
	now := time.UnixMilli(1700000260000)
	st := sendqueue.AgentState{Idle: true, Since: 1700000201234, Observed: now.Add(-10 * time.Second), TurnError: e}
	d := sendqueue.Decide(sendqueue.Head{}, st, now, time.Second)
	if d.Send || !d.Held || d.Why != "Held — the last 2 messages each failed at once (provider auth error); retries at" {
		t.Fatalf("Decide = %+v", d)
	}
	// The control: with no turn error the same window is sent to.
	st.TurnError = nil
	if d := sendqueue.Decide(sendqueue.Head{}, st, now, time.Second); !d.Send {
		t.Fatalf("control: %+v", d)
	}
}

// TestAnIdleOpenCodeWindowCarriesItsTurnErrorOnTheWire: through the host's
// listing, an idle opencode window's TurnError reaches the window list.
func TestAnIdleOpenCodeWindowCarriesItsTurnErrorOnTheWire(t *testing.T) {
	d, srv, b := handHost(t)
	for _, raw := range []string{teUser1, teFail1, teUser2, teFail2} {
		var m map[string]any
		_ = json.Unmarshal([]byte(raw), &m)
		srv.PutMessage("ses_fixtureAlpha", m)
	}
	waitUntil(t, "the turn error listed", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.client.RefreshNow(ctx)
		for _, ms := range d.of(mirror.TypeSessions) {
			for _, w := range ms.Windows {
				if w.ID == "@7" && w.TurnError != nil {
					return *w.TurnError == mirror.TurnError{Kind: "provider_auth_error", At: 1700000201234, Quick: 2}
				}
			}
		}
		return false
	})
}
