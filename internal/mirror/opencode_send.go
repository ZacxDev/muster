package mirror

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strconv"
	"time"
)

// THE GATED SEND, SERVER HALF (docs/design/opencode-first-class.md): a message
// to a window of a runtime whose send is its host's own (RuntimeSpec.GatedSend
// — opencode, through its server's API). Hub.PasteWindow refuses it here,
// before anything reaches the host, unless
//
//   - the host ADVERTISED the send (Msg.OpenCodeSend: -interactive,
//     -opencode-send, a token) — else the runtime's SendOff;
//   - the window is one its host sends to (Window.AgentSend: launcher-bound,
//     or Window.AgentBind verified: a hand-opened pane its host bound through
//     opencode's API) — else Unbound, in the host's reason's words;
//   - the window is IDLE with a status time — else *OpenCodeNotIdleError (a
//     queued paste is superseded instead).
//
// The paste then carries the runtime, the StatusSince it was decided on, the
// session the window list names (Msg.AgentSession: the only one the host may
// post to) and the message id (OpenCodeMessageID), and the host checks every
// one of these again at the moment it posts.

// openCodePasteTimeout is how long a gated send to opencode waits for its
// host: the host's own post is bounded well inside it (mirrorhost's
// openCodeSendTimeout), so a host that answers at all answers in time.
const openCodePasteTimeout = 20 * time.Second

var (
	// ErrOpenCodeSendOff: the window runs opencode and its host did not
	// advertise Msg.OpenCodeSend. Nothing was sent.
	ErrOpenCodeSendOff = errors.New("mirror: muster did not send it: this window runs opencode, and its host's muster-mirror does not send to opencode")
	// ErrOpenCodeUnbound: the window runs opencode but its host does not send
	// to it (not started by the launcher). Nothing was sent.
	ErrOpenCodeUnbound = errors.New("mirror: muster did not send it: this opencode window was not started by `muster-mirror opencode`")
	// ErrOpenCodeNotIdle is the cause of every *OpenCodeNotIdleError.
	ErrOpenCodeNotIdle = errors.New("mirror: muster did not send it: the opencode session is not idle")
)

// OpenCodeNotIdleError refuses a send to an opencode window that is not idle
// (Hub.PasteWindow). Nothing was sent to the host. Status is the window's (""
// unknown).
type OpenCodeNotIdleError struct{ Status string }

func (e *OpenCodeNotIdleError) Error() string {
	switch e.Status {
	case StatusBusy:
		return PasteRefusedOpenCodeBusyText
	case StatusWaiting:
		return PasteRefusedOpenCodeWaitingText
	}
	return PasteRefusedOpenCodeUnknownText
}

func (e *OpenCodeNotIdleError) Unwrap() error { return ErrOpenCodeNotIdle }

// MessageKey has a gated send post its message under the id derived from key
// (OpenCodeMessageID): what makes a RETRY of the same send — the composer's
// Send again of a message that may have landed, a queued row tried again —
// land at most once. internal/api passes the send's nonce, or the nonce of the
// first try of a message it holds as may-have-landed.
func MessageKey(key string) PasteOption { return func(o *PasteOptions) { o.Key = key } }

// openCodeIDKey keys the message-id derivation: fixed, because the id must be
// the same for the same send across muster's restarts and replicas. It is no
// secret: an id is only ever compared, never trusted.
var openCodeIDKey = []byte("muster opencode message id v1")

// idAlphabet is opencode's identifier alphabet.
const idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// OpenCodeMessageID is the opencode message id a send with key by owner is
// posted under: "msg_" and 26 characters of HMAC-SHA256(owner, key), shaped
// like upstream's own ids (12 hex digits, 14 of its alphabet). DETERMINISTIC
// on purpose: opencode answers a second post of an id with the first one's
// record (measured, opencodetest/testdata/prompt-v2.0.32.json), so the same
// key can land once whatever retries it. Upstream orders a session's
// messages by their sequence, not their id (packages/core/src/session/
// projector.ts), so an id need not encode the time. "" for an empty key.
func OpenCodeMessageID(owner int64, key string) string {
	if key == "" {
		return ""
	}
	m := hmac.New(sha256.New, openCodeIDKey)
	m.Write([]byte(strconv.FormatInt(owner, 10)))
	m.Write([]byte{0})
	m.Write([]byte(key))
	sum := m.Sum(nil)
	out := make([]byte, 0, 4+26)
	out = append(out, "msg_"...)
	const hexd = "0123456789abcdef"
	for _, b := range sum[:6] {
		out = append(out, hexd[b>>4], hexd[b&15])
	}
	// 14 alphabet characters: 7 from each of two 64-bit words.
	for _, x := range []uint64{binary.BigEndian.Uint64(sum[6:14]), binary.BigEndian.Uint64(sum[14:22])} {
		for range 7 {
			out = append(out, idAlphabet[x%62])
			x /= 62
		}
	}
	return string(out)
}

// gatedSendCheck is the gated-send switch and binding for (session, window
// id) on hc: the runtime's refusal when the window runs a runtime with a gated
// send and hc did not advertise it (SendOff), or the window is not one hc
// sends to (Unbound); nil otherwise, also for a window of any other runtime.
func (hc *hostConn) gatedSendCheck(session, id string) error {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	for _, w := range hc.windows {
		if w.Session != session || w.ID != id {
			continue
		}
		r, ok := w.Spec()
		if !ok || !r.GatedSend {
			return nil
		}
		if !hc.gatedSends[r.Name] {
			return r.SendOff()
		}
		if !agentSendsTo(w.AgentSend, w.AgentBind) {
			return unboundErr(r, w.AgentBind)
		}
	}
	return nil
}

// gatedStatus is the runtime, Status, StatusSince and AgentSession hc lists
// for (session, window id), and whether that window runs a runtime with a
// gated send.
func (hc *hostConn) gatedStatus(session, id string) (runtime, status string, since int64, agentSession string, ok bool) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	for _, w := range hc.windows {
		if w.Session != session || w.ID != id {
			continue
		}
		if r, ok := w.Spec(); ok && r.GatedSend {
			return r.Name, w.Status, w.StatusSince, w.AgentSession, true
		}
	}
	return "", "", 0, "", false
}

// unboundErr is r's Unbound refusal, saying why in bind's words
// (AgentBindWords) when the host gave a reason.
func unboundErr(r RuntimeSpec, bind string) error {
	err := r.Unbound()
	var pr *PasteRefusedError
	if errors.As(err, &pr) {
		cp := *pr
		cp.Why = AgentBindWords(bind)
		return &cp
	}
	return err
}

// SendsOn reports whether h's host advertised r's gated send (r.Sends).
func (h HostView) SendsOn(r RuntimeSpec) bool {
	return r.Sends != nil && r.Sends(Msg{OpenCodeSend: h.OpenCodeSend})
}

// SendBlocked is why a Rendered-view message cannot be sent to entry e of
// host h through its runtime's gated send — the words the refusal would say
// (the runtime's SendOff, or Unbound) — or "" when it can be, or e's runtime
// has no gated send. The PAGE's reading (its composer drawn disabled, and
// why) of the rule the hub ENFORCES on its own window list (gatedSendCheck);
// TestThePageAndTheHubAgreeOnWhatCanBeSent pins that the two agree.
func SendBlocked(h HostView, e SessionView) string {
	r, ok := e.Spec()
	if !ok || !r.GatedSend {
		return ""
	}
	var err error
	switch {
	case !h.SendsOn(r):
		err = r.SendOff()
	case !agentSendsTo(e.AgentSend, e.AgentBind):
		err = unboundErr(r, e.AgentBind)
	default:
		return ""
	}
	var pr *PasteRefusedError
	if errors.As(err, &pr) {
		return pr.Why
	}
	return err.Error()
}
