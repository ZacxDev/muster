package mirrorhost

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// THE HOST SPAWN'S opencode HALF (docs/design/chief-dispatch.md 3.6,
// "opencode"): what spawnRuntime does differently for runtime opencode.
//
//   - BEFORE ANY GIT CALL (openCodeSpawnCheck): this host sends to opencode
//     (-opencode-send) and holds a token; the service answers; the TUI's
//     version is the service's (the launcher would refuse a mismatch, and a
//     window that then ends is a worse refusal); the model is in the host's
//     OWN opencode's list under OpenRouter (GET /api/model) — measured: opencode
//     creates a session on a model nobody lists, so this check is the bound. A
//     list with no OpenRouter model at all is `unavailable` ("not ready"),
//     never `bad_model`: the chief must not tell its owner a model is wrong
//     when the host's list is merely empty.
//   - The session is created in the worktree, on the model, BEFORE the window:
//     `<muster-mirror> opencode -s <id>` then binds the pane to it (the
//     launcher record), and the host waits for that bind in its own listing.
//   - THE PROMPT IS POSTED AFTER THE BIND, NEVER BEFORE (postFirstPrompt):
//     posting first would start the agent's run — and spend OpenRouter credit
//     — with no window for the owner to see or close. It never travels in an
//     argv word. A spawn that fails before the post deletes the session it
//     created (spawnPlan.undo); once the window exists and the post fails, the
//     window is KEPT (the owner may be typing in it).

// openCodeBindWait bounds the wait for the launcher's bind (measured: the
// second listing, about 0.35 s after the window opened, on the scratch rig).
const openCodeBindWait = 20 * time.Second

// openCodeVersionWait bounds `opencode --version`.
const openCodeVersionWait = 10 * time.Second

// openCodeSpawn is one opencode spawn's checked inputs.
type openCodeSpawn struct {
	cl        *opencodehost.Client
	src       *OpenCodeSource
	mirrorBin string
	model     opencodehost.ModelRef
}

// openCodeSourceOf is this host's opencode source, nil when it has none.
func (c *Client) openCodeSourceOf() *OpenCodeSource {
	s, ok := capabilityOf[OpenCodeSourcer](c.cfg.Backend)
	if !ok {
		return nil
	}
	return s.OpenCodeSource()
}

// mirrorBinary is the launcher an opencode window runs: the seam, else this
// process's own executable, resolved, so the launcher and the daemon are one
// build. It must be an absolute path tmux leaves alone.
func (c *Client) mirrorBinary() (string, error) {
	b := c.cfg.AgentSpawn.MirrorBin
	if b == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		if b, err = filepath.EvalSymlinks(exe); err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(b) || !hostWordRE.MatchString(b) {
		return "", errors.New("the muster-mirror binary is not an absolute path tmux would leave alone")
	}
	return b, nil
}

// openCodeSpawnOff is why this host does not OFFER opencode in its hello, ""
// when it does: sendOn is Msg.OpenCodeSend (-opencode-send, -interactive, v2,
// a token), and both opencode and the launcher resolve. It makes no request:
// the service, its version and its models are checked at each spawn.
func (c *Client) openCodeSpawnOff(ctx context.Context, sendOn bool) string {
	if !sendOn {
		return "this host does not send to opencode (-opencode-send and a token)"
	}
	if _, err := c.serverPathBinary(ctx, "opencode", c.cfg.AgentSpawn.OpenCodeBin); err != nil {
		return err.Error()
	}
	if _, err := c.mirrorBinary(); err != nil {
		return err.Error()
	}
	return ""
}

// openCodeSendOff is why this host cannot post to opencode at all, "" when it
// can: -opencode-send, and a token. Checked with the request's own fields,
// before the rate limit and any git call, and again by openCodeSpawnCheck.
func (c *Client) openCodeSendOff() string {
	if !c.cfg.OpenCodeSend {
		return "this host does not send to opencode (-opencode-send is off)"
	}
	if src := c.openCodeSourceOf(); src == nil || !src.paired() {
		return "no opencode token on this host (muster-mirror opencode-pair)"
	}
	return ""
}

// openCodeSpawnCheck is every opencode precondition and the model check, in
// order, each refusal with its own detail. It touches no git and creates
// nothing.
func (c *Client) openCodeSpawnCheck(ctx context.Context, wireModel string) (*openCodeSpawn, error) {
	unavailable := func(why string) error { return refuseSpawn(mirror.SpawnUnavailable, "%s", why) }
	if why := c.openCodeSendOff(); why != "" {
		return nil, unavailable(why)
	}
	src := c.openCodeSourceOf()
	cl, err := src.client()
	if err != nil {
		return nil, unavailable("the opencode service cannot be reached")
	}
	service, err := cl.Info(ctx)
	switch {
	case errors.Is(err, opencodehost.ErrUnauthorized):
		return nil, unavailable("the opencode service refused this host's token")
	case err != nil:
		return nil, unavailable("the opencode service cannot be reached")
	}
	bin, err := c.serverPathBinary(ctx, "opencode", c.cfg.AgentSpawn.OpenCodeBin)
	if err != nil {
		return nil, unavailable("opencode not found")
	}
	vctx, cancel := context.WithTimeout(ctx, openCodeVersionWait)
	out, verr := exec.CommandContext(vctx, bin, "--version").Output()
	cancel()
	// 🔴 FAIL CLOSED, unlike the launcher (which starts a TUI whose version it
	// cannot read): a spawn is unattended, and a TUI of another version would
	// restart the shared service, signing every other client out.
	if verr != nil {
		return nil, unavailable("opencode's version cannot be read")
	}
	if opencodehost.NormalizeVersion(string(out)) != opencodehost.NormalizeVersion(service) {
		return nil, unavailable("opencode TUI and service versions differ")
	}
	mb, err := c.mirrorBinary()
	if err != nil {
		return nil, unavailable("the muster-mirror launcher cannot be run")
	}
	provider, id, ok := mirror.ParseSpawnModel(wireModel)
	if !ok {
		return nil, refuseSpawn(mirror.SpawnBadModel, "%q is not an OpenRouter model (openrouter/<vendor>/<model>)", capField(wireModel, 140))
	}
	models, err := cl.Models(ctx)
	if err != nil {
		return nil, unavailable("opencode's model list cannot be read")
	}
	listed, found := 0, false
	for _, m := range models {
		if m.ProviderID != provider || (m.Enabled != nil && !*m.Enabled) {
			continue
		}
		listed++
		if m.ID == id {
			found = true
		}
	}
	switch {
	case listed == 0:
		return nil, unavailable("opencode's model list is not ready (it lists no OpenRouter model)")
	case !found:
		return nil, refuseSpawn(mirror.SpawnBadModel, "this host's opencode lists no OpenRouter model %q", id)
	}
	return &openCodeSpawn{cl: cl, src: src, mirrorBin: mb, model: opencodehost.ModelRef{ID: id, ProviderID: provider}}, nil
}

// createSession creates the spawn's session sid in worktree dir, on the model.
func (o *openCodeSpawn) createSession(ctx context.Context, sid, dir string) error {
	if _, err := o.cl.CreateSession(ctx, sid, dir, &o.model); err != nil {
		return refuseSpawn(mirror.SpawnFailed, "the opencode session could not be created")
	}
	// The source's session list may predate it: read it again, so the bind is
	// seen on the next listing rather than after the list's cache ages out.
	o.src.sessionsChanged()
	return nil
}

// removeSession deletes the spawn's session sid (nothing was posted to it).
func (o *openCodeSpawn) removeSession(sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = o.cl.RemoveSession(ctx, sid)
}

// waitOpenCodeBind reports whether window win's pane got bound to opencode
// session sid by its launcher (the listing: Send, Gen sid — Send is set only on
// a launcher-bound pane, and sid is the opencode session this spawn created)
// within the bind wait.
func (c *Client) waitOpenCodeBind(ctx context.Context, win, sid string) bool {
	wl, ok := WindowListerOf(c.cfg.Backend)
	if !ok {
		return false
	}
	wait := c.cfg.AgentSpawn.BindWait
	if wait <= 0 {
		wait = openCodeBindWait
	}
	bctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		lctx, lcancel := context.WithTimeout(bctx, 10*time.Second)
		ws, err := wl.Windows(lctx)
		lcancel()
		if err == nil {
			for _, w := range ws {
				if w.ID == win && w.Agent != nil && w.Agent.Send && w.Agent.Gen == sid {
					return true
				}
			}
		}
		select {
		case <-bctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// spawnMessageID is the first prompt's message id: derived from the dispatch
// and the session this host created, so a retry of the post lands at most once
// (opencode answers a second post of an id with the first record, measured).
func spawnMessageID(dispatch int64, sid string) string {
	return mirror.OpenCodeMessageID(0, "agent-spawn/"+strconv.FormatInt(dispatch, 10)+"/"+sid)
}

// postFirstPrompt posts prompt to session sid as its first message, delivery
// "queue". An answer lost after the request went out (a timeout, a dropped
// connection: it MAY have been admitted) is retried ONCE with the same id; a
// refusal (400, 404, 409, 401) is not retried.
func (o *openCodeSpawn) postFirstPrompt(dispatch int64, sid, prompt string) error {
	id := spawnMessageID(dispatch, sid)
	var err error
	for try := 0; try < 2; try++ {
		ctx, cancel := context.WithTimeout(context.Background(), openCodeSendTimeout)
		_, err = o.cl.Prompt(ctx, sid, id, prompt)
		cancel()
		if err == nil || !promptMayHaveLanded(err) {
			return err
		}
	}
	return err
}

// promptMayHaveLanded reports whether a failed post may still have been
// admitted: anything but the server's own refusals.
func promptMayHaveLanded(err error) bool {
	for _, e := range []error{opencodehost.ErrPromptRefused, opencodehost.ErrSessionNotFound, opencodehost.ErrPromptConflict,
		opencodehost.ErrUnauthorized, opencodehost.ErrNotAdmitted} {
		if errors.Is(err, e) {
			return false
		}
	}
	return !isDialError(err)
}
