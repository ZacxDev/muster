package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// THE GATED SEND, HOST HALF (docs/design/opencode-first-class.md): a message
// muster sends to an opencode window is POSTED to its session through the
// opencode server's API (opencodehost.Client.Prompt), never typed: the paste
// guard keeps refusing keys into an opencode pane, the Terminal view's
// composer included.
//
// 🔴 EVERY CHECK IS MADE HERE AGAIN, whatever muster checked, at the moment of
// posting:
//
//   - this host sends to opencode (openCodeSend: v2, -interactive,
//     -opencode-send, a token);
//   - the window is listed (allowed) and the text a paste muster sends;
//   - the window's pane, re-resolved NOW, runs opencode bound by its launcher
//     record (AgentPane.Send) — never a title match;
//   - its session is idle since exactly the status time muster decided on
//     (Msg.Since), with nothing pending;
//   - the message does not start with one of opencode's slash commands, which
//     a prompt would hand the model as text.
//
// The post carries muster's message id (Msg.MessageID): a retry of the same
// send posts the same id, which opencode admits once.

// openCodeSendTimeout bounds the host's whole send — the re-resolve, the
// slash-command read and the post — well inside muster's wait
// (mirror's openCodePasteTimeout), so a host that answers at all answers in time.
// A var only so a test can shorten it.
var openCodeSendTimeout = 12 * time.Second

// gatedSender is a Runtime whose send is its own (Msg.Runtime names it on a
// paste): the one deliberate exception to typeInto, with its own checks.
type gatedSender interface {
	gatedSend(h *hostSide, m mirror.Msg)
}

// gatedSend posts m to the window's opencode session.
func (openCodeRuntime) gatedSend(h *hostSide, m mirror.Msg) { h.sendOpenCode(m) }

// openCodeSend is what this host advertises as Msg.OpenCodeSend.
func (h *hostSide) openCodeSend() bool {
	if h.version < 2 || !h.c.cfg.Interactive || !h.c.cfg.OpenCodeSend {
		return false
	}
	src, ok := h.openCodeSource()
	return ok && src.paired()
}

// errOpenCodeSend is a refusal of a gated send, of kind refusal.
type errOpenCodeSend struct {
	why, refusal string
}

func (e *errOpenCodeSend) Error() string { return e.why }

func ocRefusal(refusal, why string) error { return &errOpenCodeSend{why: why, refusal: refusal} }

// sendOpenCode posts paste m to its window's opencode session and answers
// pasted.
func (h *hostSide) sendOpenCode(m mirror.Msg) {
	reply := func(err error, session, msgID string) {
		out := mirror.Msg{Type: mirror.TypePasted, ID: m.ID}
		if err != nil {
			out.Error = err.Error()
			var oe *errOpenCodeSend
			if errors.As(err, &oe) {
				out.Refusal = oe.refusal
			}
			h.c.log.Printf("muster-mirror: opencode send to %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		} else {
			// 🔴 AUDIT LINE: which session and message id, never the text.
			h.c.log.Printf("muster-mirror: sent a message to opencode session %s in %q%s (message %s)", session, m.Session, windowSuffix(m.Window), msgID)
		}
		_ = h.send(out)
	}
	if !h.openCodeSend() {
		reply(ocRefusal(mirror.PasteRefusalOpenCodeOff, mirror.PasteRefusedOpenCodeOffText), "", "")
		return
	}
	if err := h.allowed(m); err != nil {
		reply(err, "", "")
		return
	}
	text, err := mirror.SanitizePaste(m.Text)
	if err != nil {
		reply(err, "", "")
		return
	}
	src, _ := h.openCodeSource()
	ctx, cancel := context.WithTimeout(h.ctx, openCodeSendTimeout)
	defer cancel()
	wi, err := h.window(ctx, m.Session, m.Window)
	if err != nil {
		reply(err, "", "")
		return
	}
	ag := wi.Agent
	if ag == nil || ag.Runtime != mirror.RuntimeOpenCode || ag.Gen == "" || !ag.Send {
		reply(ocRefusal(mirror.PasteRefusalOpenCodeUnbound, mirror.PasteRefusedOpenCodeUnboundText), "", "")
		return
	}
	switch {
	case ag.Status == mirror.StatusBusy:
		reply(ocRefusal(mirror.PasteRefusalOpenCodeNotIdle, mirror.PasteRefusedOpenCodeBusyText), ag.Gen, "")
		return
	case ag.Status == mirror.StatusWaiting || ag.Pending != nil:
		reply(ocRefusal(mirror.PasteRefusalOpenCodeNotIdle, mirror.PasteRefusedOpenCodeWaitingText), ag.Gen, "")
		return
	case ag.Status != mirror.StatusIdle || ag.StatusSince <= 0 || ag.StatusSince != m.Since:
		reply(ocRefusal(mirror.PasteRefusalOpenCodeNotIdle, mirror.PasteRefusedOpenCodeUnknownText), ag.Gen, "")
		return
	}
	cl, err := src.client()
	if err != nil {
		reply(err, ag.Gen, "")
		return
	}
	if err := refuseSlashCommand(ctx, cl, ag.Gen, text); err != nil {
		reply(err, ag.Gen, "")
		return
	}
	msgID := m.MessageID
	if !opencodehost.ValidMessageID(msgID) {
		// A paste with no key (a caller with no nonce): a fresh id, which
		// dedupes nothing.
		msgID = opencodehost.NewMessageID(time.Now())
	}
	_, err = cl.Prompt(ctx, ag.Gen, msgID, text)
	switch {
	case err == nil:
		reply(nil, ag.Gen, msgID)
	case errors.Is(err, opencodehost.ErrSessionNotFound):
		reply(ocRefusal(mirror.PasteRefusalOpenCodeGone, mirror.PasteRefusedOpenCodeGoneText), ag.Gen, msgID)
	case errors.Is(err, opencodehost.ErrUnauthorized):
		reply(errors.New(unauthorizedWhy), ag.Gen, msgID)
	case errors.Is(err, opencodehost.ErrPromptConflict), errors.Is(err, opencodehost.ErrPromptRefused), isDialError(err):
		// Refused, or never reached the server: nothing was admitted.
		reply(fmt.Errorf("Not sent: %v", err), ag.Gen, msgID)
	default:
		// 🔴 The post went out and no answer came (a timeout, a dropped
		// connection): it MAY have been admitted. Unconfirmed, never "not
		// sent" — and a Send again posts the same id, which opencode admits once.
		reply(ocRefusal(mirror.PasteRefusalOpenCodeUnconfirmed,
			"Sent to opencode, but it did not confirm it: "+err.Error()), ag.Gen, msgID)
	}
}

// refuseSlashCommand refuses text whose first word is "/<name>" for a name in
// the session's slash commands (GET /api/command for its directory): a prompt
// would hand the model the command as text. Any other text — a path such as
// "/etc/hosts is wrong" — passes. A read that fails refuses a text starting
// with "/", since what it would do cannot be told.
func refuseSlashCommand(ctx context.Context, cl *opencodehost.Client, session, text string) error {
	first, _, _ := strings.Cut(strings.TrimLeft(text, " \t\n"), " ")
	first, _, _ = strings.Cut(first, "\n")
	name, ok := strings.CutPrefix(first, "/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return nil
	}
	sess, err := cl.GetSession(ctx, session)
	if err != nil {
		return ocRefusal(mirror.PasteRefusalOpenCodeSlash, mirror.PasteRefusedOpenCodeSlashText+" (muster could not read opencode's commands: "+err.Error()+")")
	}
	cmds, err := cl.Commands(ctx, sess.Location.Directory)
	if err != nil {
		return ocRefusal(mirror.PasteRefusalOpenCodeSlash, mirror.PasteRefusedOpenCodeSlashText+" (muster could not read opencode's commands: "+err.Error()+")")
	}
	for _, c := range cmds {
		if c.Name == name {
			return ocRefusal(mirror.PasteRefusalOpenCodeSlash, mirror.PasteRefusedOpenCodeSlashText)
		}
	}
	return nil
}
