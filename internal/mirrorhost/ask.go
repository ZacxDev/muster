package mirrorhost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/tmuxver"
)

// ANSWERING AN AskUserQuestion PICKER FROM MUSTER: the keys go to the pane the
// way they would from a keyboard, through internal/askdrive, which reads the
// pane before and after every one.
//
// 🔴 THE KEYS ARE PASTED, UNSANITIZED, NEVER `send-keys`. Measured (tmux
// 3.7c): with a read-only client attached to the session — which muster's own
// Terminal view is — `send-keys` is refused with "client is read-only", for a
// pane target as for a session one. `paste-buffer` has no such check; on 3.7+
// without -S it SANITIZES control characters (an arrow arrived as nothing and
// a Backspace as the text "^?"), with -S the bytes arrive as the keys they
// are. A tmux BEFORE 3.7 has no -S and never sanitized: its plain
// `paste-buffer` writes the bytes as they are (tmuxver.MinRawPaste), so there
// the keys are pasted without the flag (KeyPaste). No -p: a bracketed paste is
// text to the picker, not keys.

// AskBackend answers an AskUserQuestion picker in a pane, continuing an
// answer an earlier one left part-typed in it (askdrive.DriveFrom): prior is
// what that one sent (nil: nothing), and keys how many keys of choices' plan
// the picker has received when it returns.
type AskBackend interface {
	AnswerAsk(ctx context.Context, pane string, qs []askq.Question, choices []askdrive.Choice, prior *askq.Progress) (keys int, err error)
}

// AskOf is b's AskBackend, if it has one (through any wrapper).
func AskOf(b Backend) (AskBackend, bool) { return capabilityOf[AskBackend](b) }

// AnswerAsk implements AskBackend: askdrive.DriveFrom on the pane.
//
// 🔴 ONE DRIVE PER PANE AT A TIME: a second answer for a pane being typed
// into is refused (ErrPaneBusy) before any key — two drives' keys would
// interleave in one picker. And a tmux found unable to paste keys raw
// (KeyPasteNone) is refused (ErrNoRawPaste) before any key.
func (t Tmux) AnswerAsk(ctx context.Context, pane string, qs []askq.Question, choices []askdrive.Choice, prior *askq.Progress) (int, error) {
	if !strings.HasPrefix(pane, "%") {
		return 0, fmt.Errorf("muster-mirror: %q is not a tmux pane id", pane)
	}
	release, ok := lockPane(strings.Join(append(t.server(), pane), " "))
	if !ok {
		return 0, ErrPaneBusy
	}
	defer release()
	kp := t.KeyPaste(ctx)
	if kp == KeyPasteNone {
		return 0, ErrNoRawPaste
	}
	t.dropLeftoverBuffers(ctx)
	return askdrive.DriveFrom(ctx, tmuxPane{t: t, id: pane, paste: kp}, qs, choices, prior, askdrive.Options{})
}

// ErrPaneBusy: another answer is being typed into the pane.
var ErrPaneBusy = errors.New("muster-mirror: another answer is being typed into this pane")

// ErrNoRawPaste: this tmux cannot paste keys raw (KeyPasteNone): its client
// has no `paste-buffer -S` while the server it talks to is tmux 3.7 or later,
// which sanitizes a paste without it.
var ErrNoRawPaste = fmt.Errorf("%w: this tmux client has no `paste-buffer -S` but the running tmux server is %s or later (which sanitizes a paste without it) or reports no version it can read; restart the tmux server with, or put first on PATH, a single tmux version", errCannotAnswer, MinRawPasteTmux)

// drives is the panes being typed into (lockPane), by server and pane id.
var drives = struct {
	sync.Mutex
	panes map[string]bool
}{panes: map[string]bool{}}

// lockPane takes pane for one drive; false when a drive has it.
func lockPane(pane string) (release func(), ok bool) {
	drives.Lock()
	defer drives.Unlock()
	if drives.panes[pane] {
		return nil, false
	}
	drives.panes[pane] = true
	return func() {
		drives.Lock()
		delete(drives.panes, pane)
		drives.Unlock()
	}, true
}

// askBufferPrefix names every tmux buffer a key is pasted through.
const askBufferPrefix = "muster-mirror-ask-"

// liveBuffers is the buffers this process has loaded and not yet pasted:
// dropLeftoverBuffers never deletes one of them.
var liveBuffers = struct {
	sync.Mutex
	names map[string]bool
}{names: map[string]bool{}}

// dropLeftoverBuffers deletes the askBufferPrefix buffers no key of this
// process is in flight through: what a muster-mirror that died between
// load-buffer and paste-buffer left behind. Best effort.
func (t Tmux) dropLeftoverBuffers(ctx context.Context) {
	p := tmuxPane{t: t}
	out, err := p.run(ctx, nil, "list-buffers", "-F", "#{buffer_name}")
	if err != nil {
		return
	}
	for _, name := range strings.Fields(string(out)) {
		if !strings.HasPrefix(name, askBufferPrefix) {
			continue
		}
		liveBuffers.Lock()
		live := liveBuffers.names[name]
		liveBuffers.Unlock()
		if !live {
			_, _ = p.run(ctx, nil, "delete-buffer", "-b", name)
		}
	}
}

// MinRawPasteTmux is the first tmux release with `paste-buffer -S`, and the
// first whose plain `paste-buffer` sanitizes (tmuxver.MinRawPaste).
const MinRawPasteTmux = tmuxver.MinRawPaste

// KeyPaste is how a tmux pastes a picker's keys so they arrive as keys.
type KeyPaste int

const (
	// KeyPasteUnknown: not known (no tmux server running and a `tmux -V` it
	// cannot read, or a server this client cannot talk to). Typed as
	// KeyPasteRawFlag: a tmux that turns out to lack -S refuses the paste, so
	// no key goes in sanitized.
	KeyPasteUnknown KeyPaste = iota
	// KeyPasteRawFlag: `paste-buffer -S` (tmux 3.7 and later).
	KeyPasteRawFlag
	// KeyPastePlain: `paste-buffer` with no flag — a tmux before 3.7, whose
	// paste never sanitized (tmuxver.MinRawPaste).
	KeyPastePlain
	// KeyPasteNone: cannot. The tmux CLIENT has no -S while the SERVER it
	// talks to is 3.7 or later, or the server's version cannot be read: a
	// plain paste could be sanitized, and -S cannot be sent. Not observed —
	// a 3.4 client cannot talk to a 3.7c server at all (measured: "server
	// exited unexpectedly", KeyPasteUnknown) — and refused for any client and
	// server pair that can.
	KeyPasteNone
)

// String names it in test failures.
func (k KeyPaste) String() string {
	switch k {
	case KeyPasteRawFlag:
		return "paste-buffer -S"
	case KeyPastePlain:
		return "paste-buffer (tmux before " + MinRawPasteTmux + " pastes raw)"
	case KeyPasteNone:
		return "none"
	}
	return "unknown"
}

// KeyPaste reports how this tmux can paste a picker's keys raw. Probed first:
// pasting a buffer that does not exist is refused as "no buffer" by a tmux
// that knows -S, and as "unknown flag -S" by one that does not — nothing is
// pasted either way. "unknown flag" alone does not make the plain paste raw:
// the flag is parsed by the tmux CLIENT, the paste is done by the SERVER, so
// the server's own #{version} decides — before 3.7 it is KeyPastePlain, 3.7 or
// later (or unreadable) KeyPasteNone. With no tmux server running the probe
// cannot tell ("no server running"), so `tmux -V` is read instead — the server
// that binary starts is that version; a version it cannot read, or any other
// failure, is KeyPasteUnknown, NOT "old": the caller asks again later.
func (t Tmux) KeyPaste(ctx context.Context) KeyPaste {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	p := tmuxPane{t: t}
	_, err := p.run(ctx, nil, "paste-buffer", "-S", "-b", askBufferPrefix+"probe-"+hex.EncodeToString(rnd[:]))
	switch {
	case err == nil:
		return KeyPasteRawFlag // a buffer by that name existed and was pasted nowhere useful; -S is known
	case strings.Contains(err.Error(), "no buffer"):
		return KeyPasteRawFlag
	case strings.Contains(err.Error(), "unknown flag"):
		out, verr := p.run(ctx, nil, "display-message", "-p", "#{version}")
		if verr != nil {
			return KeyPasteNone
		}
		if ok, known := tmuxver.AtLeast(string(out), MinRawPasteTmux); known && !ok {
			return KeyPastePlain
		}
		return KeyPasteNone
	case !strings.Contains(err.Error(), "no server running") && !strings.Contains(err.Error(), "error connecting to"):
		// A server this client cannot talk to — measured: a tmux 3.4 client
		// against a 3.7c server gets "server exited unexpectedly" for every
		// command — is not "no server": its version is not this binary's.
		return KeyPasteUnknown
	}
	cmd := exec.CommandContext(ctx, t.bin(), "-V")
	cmd.Env = tmuxEnv()
	out, verr := cmd.Output()
	if verr != nil {
		return KeyPasteUnknown
	}
	switch ok, known := tmuxver.AtLeast(string(out), MinRawPasteTmux); {
	case !known:
		return KeyPasteUnknown
	case ok:
		return KeyPasteRawFlag
	}
	return KeyPastePlain
}

// keyPaster is a backend that can say how it pastes raw keys
// (Tmux.KeyPaste).
type keyPaster interface {
	KeyPaste(ctx context.Context) KeyPaste
}

// tmuxPane is a tmux pane as an askdrive.Pane.
type tmuxPane struct {
	t  Tmux
	id string
	// paste is how Send pastes (Tmux.KeyPaste); the zero value is
	// KeyPasteUnknown, which pastes with -S.
	paste KeyPaste
}

func (p tmuxPane) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.t.bin(), append(p.t.server(), args...)...)
	cmd.Env = tmuxEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Capture is the pane's screen with wrapped lines joined.
func (p tmuxPane) Capture(ctx context.Context) (string, error) {
	b, err := p.run(ctx, nil, "capture-pane", "-p", "-J", "-t", p.id)
	return string(b), err
}

// LastInput implements askdrive.Activity: the latest #{client_activity} of
// the clients that can type (not read-only — muster's own viewer is one)
// whose current pane is this one; zero when there are none. Measured (tmux
// 3.7c): a keystroke through a client moves it, and so do a focus-in event
// and mouse moves and clicks (tmux has no keystroke-only clock); a
// paste-buffer (muster's own keys) moves no client's. It is whole seconds,
// rounded down (askdrive.Options.Grain).
func (p tmuxPane) LastInput(ctx context.Context) (time.Time, error) {
	out, err := p.run(ctx, nil, "list-clients", "-F", "#{pane_id} #{client_readonly} #{client_activity}")
	if err != nil {
		return time.Time{}, err
	}
	var last time.Time
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 || f[0] != p.id || f[1] == "1" {
			continue
		}
		at, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			continue
		}
		if t := time.Unix(at, 0); t.After(last) {
			last = t
		}
	}
	return last, nil
}

// Send pastes keys unsanitized and unbracketed (see the file comment), with
// -S or without it as p.paste says.
func (p tmuxPane) Send(ctx context.Context, keys string) error {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	buf := askBufferPrefix + hex.EncodeToString(rnd[:])
	liveBuffers.Lock()
	liveBuffers.names[buf] = true
	liveBuffers.Unlock()
	defer func() {
		liveBuffers.Lock()
		delete(liveBuffers.names, buf)
		liveBuffers.Unlock()
	}()
	if _, err := p.run(ctx, []byte(keys), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	args := []string{"paste-buffer", "-S", "-d", "-b", buf, "-t", p.id}
	if p.paste == KeyPastePlain {
		args = append(args[:1], args[2:]...) // no -S: tmux before 3.7 does not know it, and pastes raw without it
	}
	if _, err := p.run(ctx, nil, args...); err != nil {
		_, _ = p.run(ctx, nil, "delete-buffer", "-b", buf)
		return err
	}
	return nil
}

// askAnswer is what this host advertises as Msg.AskAnswer: an -interactive
// host whose backend can both type into a picker and find a Claude pane.
//
// A tmux that cannot paste keys raw (KeyPasteNone) cannot type a picker's
// keys: the host says so once and does not advertise it. A tmux before 3.7
// can (KeyPastePlain), and the host says once which way it types.
func (h *hostSide) askAnswer() bool {
	ab, ask := AskOf(h.c.cfg.Backend)
	_, claude := h.claudeBackend()
	if !ask || !claude || !h.c.cfg.Interactive {
		return false
	}
	if kp, ok := ab.(keyPaster); ok {
		ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
		defer cancel()
		// Not KNOWN (no tmux server yet, a version it cannot read) is not
		// "cannot": it is advertised, and each answer asks again before
		// typing (Tmux.AnswerAsk).
		switch kp.KeyPaste(ctx) {
		case KeyPasteNone:
			h.c.log.Printf("muster-mirror: this tmux client has no `paste-buffer -S` but the running tmux server is %s or later (which sanitizes a paste without -S), so muster cannot type answers into Claude Code's pickers on this host; run one tmux version for the client and the server", MinRawPasteTmux)
			return false
		case KeyPastePlain:
			h.c.log.Printf("muster-mirror: this tmux is older than %s: answers are typed with plain `paste-buffer`, which tmux before %s pastes raw", MinRawPasteTmux, MinRawPasteTmux)
		}
		if t, ok := ab.(Tmux); ok {
			t.dropLeftoverBuffers(ctx)
		}
	}
	return true
}

// answer types m.Ask into the AskUserQuestion picker of the Claude pane of
// what m names, and answers answered.
//
// 🔴 REFUSED UNLESS THIS HOST OPTED IN (-interactive), whatever muster asks —
// muster refuses it too — and only for a listed session, into a pane that
// runs Claude Code NOW (re-resolved here, as for a paste to the rendered view),
// through typeInto and so the Hermes paste guard.
// The driver itself refuses to touch a picker that is not this question,
// untouched — or exactly as muster's own earlier typing left it (m.Ask.Prior;
// internal/askdrive's DriveFrom).
func (h *hostSide) answer(m mirror.Msg) {
	if r, ok := runtimeNamed(m.Runtime); ok {
		if an, ok := r.(answerer); ok {
			// The runtime's own answer (a Hermes approval: through Hermes's
			// gateway, never by keys, hermes_asks.go).
			an.answer(h, m)
			return
		}
	}
	// keys is how many keys of the answer's plan the picker has received
	// (Msg.Keys): what muster hands the next answer, if this one stops.
	keys := 0
	reply := func(err error) {
		ans := mirror.Msg{Type: mirror.TypeAnswered, ID: m.ID}
		if err != nil {
			ans.Error, ans.Keys = err.Error(), keys
			switch {
			case errors.Is(err, askdrive.ErrNoPicker):
				ans.Refusal = mirror.AskRefusalNoPicker
			case errors.Is(err, ErrPaneBusy):
				ans.Refusal = mirror.AskRefusalBusy
			// Before touched and lost: it says what neither does — muster
			// could not read the question, nobody changed the picker.
			case errors.Is(err, askdrive.ErrUnreadable) && errors.Is(err, askdrive.ErrLost):
				ans.Refusal = mirror.AskRefusalUnreadableTyped
			case errors.Is(err, askdrive.ErrUnreadable):
				ans.Refusal = mirror.AskRefusalUnreadable
			case errors.Is(err, askdrive.ErrNotUntouched):
				ans.Refusal = mirror.AskRefusalTouched
			case errors.Is(err, askdrive.ErrLost):
				ans.Refusal = mirror.AskRefusalLost
			case errors.Is(err, askdrive.ErrUnrecognised):
				ans.Refusal = mirror.AskRefusalUnrecognised
				h.c.log.Printf("muster-mirror: 🔴 could not recognise Claude Code's AskUserQuestion picker in %q%s — its layout has probably changed; muster cannot type answers until internal/askdrive is updated (run `make test-liveclaude`)", m.Session, windowSuffix(m.Window))
			case errors.Is(err, errCannotAnswer):
				ans.Refusal = mirror.AskRefusalUnsupported
			}
			h.c.log.Printf("muster-mirror: answer in %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		} else {
			h.c.log.Printf("muster-mirror: typed an answer into %q%s", m.Session, windowSuffix(m.Window))
		}
		_ = h.send(ans)
	}
	if !h.c.cfg.Interactive {
		reply(fmt.Errorf("%w: this host is read-only (muster-mirror was not started with -interactive)", errCannotAnswer))
		return
	}
	if err := h.allowed(m); err != nil {
		reply(err)
		return
	}
	if m.Ask == nil || len(m.Ask.Questions) == 0 {
		reply(errors.New("no answer to type"))
		return
	}
	ab, ok := AskOf(h.c.cfg.Backend)
	cb, okc := h.claudeBackend()
	if !ok || !okc {
		reply(fmt.Errorf("%w: this host cannot type into a picker", errCannotAnswer))
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 50*time.Second)
	defer cancel()
	p, ok := h.claudePaneOf(cb, m)(ctx)
	if !ok {
		reply(fmt.Errorf("%w: %v", askdrive.ErrNoPicker, errNotClaudeNow))
		return
	}
	reply(h.typeInto(ctx, p.Pane, func() error {
		var err error
		keys, err = ab.AnswerAsk(ctx, p.Pane, m.Ask.Questions, m.Ask.Choices, m.Ask.Prior)
		return err
	}))
}

// errCannotAnswer marks a refusal that means this host cannot type answers at
// all (mirror.AskRefusalUnsupported).
var errCannotAnswer = errors.New("muster-mirror: cannot type an answer")
