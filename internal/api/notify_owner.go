package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// notify_owner (docs/push.md#an-agent-notifies-you): one of the owner's agents
// — a mirrored session, the chief, a dispatched agent, an agent the chief
// spawned on a host — sends its OWNER a push that opens a link when tapped.
//
// 🔴 EVERY PUSH SAYS WHO SENT IT. Its body starts "From <sender>", and the
// sender is muster's own name for the CALLER (notifySenderOf), never anything
// the caller wrote: the title is the caller's, so a title can claim anything;
// the "From" line is how the owner tells. A dispatched agent and an agent the
// chief spawned on a host are marked as such.
//
// 🔴 THE LINK IS A MUSTER PATH OR AN ALLOWLISTED https ADDRESS, nothing else
// (notifyLinkFor). An external address is never put in a payload as the tap
// target: the tap opens muster's own /notify/open, which checks the address
// against the CURRENT allowlist again and only then redirects — so the
// service worker and the Android app only ever open a muster path.
//
// 🔴 LIMITS, IN ORDER: the sender's own cap (notifySenderBurst per
// notifySenderWindow, per sending agent or session, its own bucket), then the
// owner's own toggle (agent_notify, Account → Notifications) and the
// per-owner cap (pushAllowed), then the owner's devices. The tool waits for
// the fan-out and SAYS which of these decided it; every call writes one
// audit_log row the owner reads on Account → Notifications.
// ---------------------------------------------------------------------------

const (
	notifyOwnerTool = "notify_owner"

	// notifySenderBurst per notifySenderWindow is one sender's cap, per bucket
	// (notify_owner; page_update's push), on top of the owner's own cap.
	notifySenderBurst  = 6
	notifySenderWindow = 10 * time.Minute

	notifyTitleMax  = 80
	notifyBodyMax   = 200
	notifySenderMax = 60
	notifyURLMax    = 2048

	// notifyOpenPath is the tap target of a notification that links an
	// external address (handleNotifyOpen).
	notifyOpenPath = "/notify/open"

	// notifyAuditAction is the audit_log action of one notify_owner call.
	notifyAuditAction = users.NotifyAuditAction
	// notifyAuditShown is how many of them Account → Notifications lists.
	notifyAuditShown = 20

	// notifyWaitMax bounds how long the tool waits for the fan-out: the
	// fan-out's own bound, and a little.
	notifyWaitMax = pushBroadcastTimeout + 2*time.Second
)

// The sender kinds a notification is labelled with.
const (
	notifyKindChief      = "chief"
	notifyKindDispatched = "dispatched"
	notifyKindMirrored   = "mirrored"
	notifyKindSpawned    = "spawned"
)

// notifyOwnerToolDef is the tool, as both doors offer it.
func notifyOwnerToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: notifyOwnerTool,
		Description: "Send your owner a notification on their phone and browsers, which opens a link when they tap it. Use it for something they " +
			"should look at now — a result, a finished piece of work, a page to review — not for progress or chatter. It always says it is from you. " +
			"url is a muster path (\"/tasks/42\", \"/pages/7/preview\") or an https address on a host your owner's server allows " +
			"(MUSTER_NOTIFY_LINK_HOSTS; none by default); anything else is refused. At most " + strconv.Itoa(notifySenderBurst) + " per " +
			strconv.Itoa(int(notifySenderWindow/time.Minute)) + " minutes from you, and your owner can turn these off. The result's status says what happened: " +
			"\"delivered\" (a push service accepted it for that many devices — not proof they read it), \"suppressed\" (with the reason: your cap, their cap, or they turned these off), " +
			"\"no_device\" (they have no device to receive it), \"failed\", or \"not_configured\".",
		Parameters: objSchema(map[string]any{
			"title": strProp("The notification's title: one short line, self-contained (a lock screen shows it without context). Markdown is flattened; up to " + strconv.Itoa(notifyTitleMax) + " characters."),
			"body":  strProp("Optional: a line or two more. Flattened to plain text and cut to fit."),
			"url":   strProp("Optional: what a tap opens — a muster path starting with \"/\", or an https address on an allowed host. Omitted: your own page (a dispatched agent) or muster's home."),
		}, "title")}
}

// notifySender is who is sending: the key its cap is counted under, the label
// the push names it by, and its kind.
type notifySender struct {
	key, label, kind string
}

// fromLine is the push body's first words, "From <sender>".
func (n notifySender) fromLine() string {
	label := truncate(pushLine(n.label), notifySenderMax)
	switch n.kind {
	case notifyKindChief:
		if strings.EqualFold(label, "chief") {
			return "From your chief"
		}
		return "From your chief, " + label
	case notifyKindDispatched:
		return "From agent " + label + " (dispatched)"
	case notifyKindSpawned:
		return "From " + label + " (spawned on a host)"
	}
	return "From " + label
}

// notifySenderOf is THE rule for naming a notify_owner caller (one rule, one
// place): muster's own name for it, decided from what the door proved — the
// agent row a dispatched agent's token resolved to, or the session a mirror
// host lists — never from the arguments. ok is false for a caller that is
// neither.
func (s *Server) notifySenderOf(ctx context.Context, c toolCaller) (notifySender, bool) {
	if c.agent != nil && c.agent.ID > 0 {
		a := *c.agent
		kind := notifyKindDispatched
		if s.isChiefAgent(ctx, a) {
			kind = notifyKindChief
		}
		return notifySender{key: c.askerKey(), label: firstNonBlank(a.DisplayName, a.Name), kind: kind}, true
	}
	if c.asker.System || c.asker.AgentID > 0 || c.asker.MirrorHost == "" || c.asker.MirrorSession == "" {
		return notifySender{}, false
	}
	label := firstNonBlank(c.asker.Label, "a session on "+c.asker.MirrorHost)
	kind := notifyKindMirrored
	// A window the chief started on a host (host dispatch): marked spawned.
	if _, err := s.callerHostDispatch(ctx, c.asker.MirrorHost, c.asker.MirrorSession, c.mirrorWindow); err == nil {
		kind = notifyKindSpawned
	}
	return notifySender{key: c.askerKey(), label: label, kind: kind}, true
}

// --- the link ---

// notifyPathChars is every byte a muster path may hold: RFC 3986's path and
// query characters, '%' for an escape and '#' for a fragment — the set the
// Android app's MusterPath accepts — and nothing else (no space, no control
// byte, no backslash, no non-ASCII).
func notifyPathChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:@%/?#", c) >= 0
}

// notifyLinkFor is THE rule for a notify_owner link (one rule, one place: the
// tool and /notify/open both decide by it). raw is a muster path or an https
// address; hosts the operator's MUSTER_NOTIFY_LINK_HOSTS.
//
// It answers the tap target the payload carries (path, always a muster path)
// and, for an external address, the address itself (external, canonical);
// or why it is refused.
//
//   - A MUSTER PATH: "/" then path characters (notifyPathChar) only, at most
//     notifyURLMax bytes; never "//" or "/\" (a scheme-relative address to
//     another host) anywhere, nor once its escapes are decoded (%2F, %5C);
//     no "." or ".." segment, encoded or not; and not /notify/open itself
//     (an external address is given as itself).
//   - AN EXTERNAL ADDRESS: "https://" (no other scheme: not http, javascript:,
//     data:), no user name or password, no port, and a host EXACTLY one of
//     hosts (no subdomain, no trailing dot). The tap target is
//     /notify/open?to=<address>.
func notifyLinkFor(raw string, hosts []string) (path, external, refusal string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ""
	}
	if len(raw) > notifyURLMax {
		return "", "", "url is longer than " + strconv.Itoa(notifyURLMax) + " characters"
	}
	for i := 0; i < len(raw); i++ {
		if !notifyPathChar(raw[i]) {
			return "", "", "url may hold only URL characters (no space, backslash, control or non-ASCII character)"
		}
	}
	if raw[0] == '/' {
		if bad := notifyPathRefusal(raw); bad != "" {
			return "", "", bad
		}
		return raw, "", ""
	}
	if !strings.HasPrefix(strings.ToLower(raw), "https://") {
		return "", "", "url must be a muster path starting with \"/\" or an https:// address on an allowed host"
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil, u.Scheme != "https", u.Opaque != "", u.Host == "":
		return "", "", "url is not a well-formed https:// address"
	case u.User != nil:
		return "", "", "url may not carry a user name or password"
	case u.Port() != "":
		return "", "", "url may not name a port"
	}
	host := strings.ToLower(u.Hostname())
	if len(hosts) == 0 {
		return "", "", "no external host is allowed on this server (MUSTER_NOTIFY_LINK_HOSTS is empty): link a muster path instead"
	}
	if !slices.Contains(hosts, host) {
		return "", "", "host " + host + " is not one this server allows (MUSTER_NOTIFY_LINK_HOSTS): link a muster path, or an allowed host"
	}
	external = u.String()
	return notifyOpenPath + "?to=" + url.QueryEscape(external), external, ""
}

// notifyPathRefusal is why muster path p is refused, or "".
func notifyPathRefusal(p string) string {
	rest := p
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return "url has a malformed %-escape"
	}
	for _, form := range []string{p, rest, decoded} {
		if strings.Contains(form, "//") || strings.Contains(form, "\\") {
			return "url may not hold \"//\" or a backslash (a path on another host), escaped or not"
		}
	}
	for i := 0; i < len(decoded); i++ {
		if decoded[i] < 0x20 || decoded[i] == 0x7f {
			return "url may not hold a control character, escaped or not"
		}
	}
	for _, seg := range strings.Split(decoded, "/") {
		if seg == "." || seg == ".." {
			return "url may not hold a \".\" or \"..\" segment"
		}
	}
	if decoded == notifyOpenPath || strings.HasPrefix(decoded, notifyOpenPath+"/") {
		return "give an external address as itself (https://…), not through " + notifyOpenPath
	}
	return ""
}

// handleNotifyOpen is GET /notify/open?to=<address>: the tap target of a
// notification that links an external address. Session-only (requireSession):
// it re-checks the address against the CURRENT allowlist (a host taken out of
// MUSTER_NOTIFY_LINK_HOSTS stops working at once) and redirects to it with no
// referrer; anything else is a 400 and goes nowhere.
func (s *Server) handleNotifyOpen(w http.ResponseWriter, r *http.Request) {
	to := r.URL.Query().Get("to")
	_, external, refused := notifyLinkFor(to, s.ext.NotifyLinkHosts)
	if refused == "" && external == "" {
		refused = "the link names no external address"
	}
	if refused != "" {
		s.logger.Printf("notify: refused to open a notification's link: %s", refused)
		s.renderNotice(w, r, http.StatusBadRequest, "This link is not allowed", "muster will not open it: "+refused+".")
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, external, http.StatusSeeOther)
}

// --- the per-sender cap ---

// senderRateLimit is a fixed window per key (notifySenderBurst per
// notifySenderWindow): ownerRateLimit's shape, keyed by a sender's bucket.
type senderRateLimit struct {
	mu   sync.Mutex
	wins map[string]*ownerWindow
}

// allow reports whether key may send one more at now, and records it.
func (l *senderRateLimit) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wins == nil {
		l.wins = map[string]*ownerWindow{}
	}
	// Forget windows that have rolled, so the map holds only live senders.
	for k, w := range l.wins {
		if now.Sub(w.start) >= notifySenderWindow {
			delete(l.wins, k)
		}
	}
	w := l.wins[key]
	if w == nil {
		w = &ownerWindow{start: now}
		l.wins[key] = w
	}
	if w.count >= notifySenderBurst {
		return false
	}
	w.count++
	return true
}

// The buckets a sender's cap is counted in: one per kind of notification, so
// an agent iterating on a page never spends the cap of its own notify_owner.
const (
	notifyBucketNotify     = "notify:"
	notifyBucketPageUpdate = "page_update:"
)

// --- the tool ---

// notifyOutcomeStatus is what the result's status says for an outcome.
func notifyOutcomeStatus(delivered int, drops []string) (status, reason string) {
	if delivered > 0 {
		return "delivered", ""
	}
	// The most telling reason first: a choice (a toggle, a cap) explains
	// silence better than a missing device does.
	for _, want := range []string{pushDropMuted, pushDropSenderCap, pushDropOwnerCap, pushDropNoRecipient} {
		if slices.Contains(drops, want) {
			return "suppressed", want
		}
	}
	switch {
	case slices.Contains(drops, pushDropNoDevice):
		return "no_device", pushDropNoDevice
	case slices.Contains(drops, pushDropFailed):
		return "failed", pushDropFailed
	case slices.Contains(drops, pushDropOff), slices.Contains(drops, pushDropRouterOnly):
		return "not_configured", firstNonBlank(func() string {
			if slices.Contains(drops, pushDropOff) {
				return pushDropOff
			}
			return ""
		}(), pushDropRouterOnly)
	case len(drops) > 0:
		return "failed", drops[0]
	}
	return "unknown", ""
}

func (s *Server) toolNotifyOwner(ctx context.Context, c toolCaller, arguments string) string {
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("arguments are {title, body?, url?}")
	}
	from, ok := s.notifySenderOf(ctx, c)
	if !ok {
		return toolErr("%s is for your owner's agents and mirrored sessions", notifyOwnerTool)
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return toolErr("%s: your owner could not be read", notifyOwnerTool)
	}
	// 🔴 A TEAM HAS NO ONE OWNER TO NOTIFY: its members hear of a team's work
	// through its asks. Refused here for a team-owned agent; a team host's
	// sessions are not even listed the tool (teamHostTools).
	if p.Team {
		return toolErr("%s is not offered to a team's agents: a team's members are reached through attention_ask", notifyOwnerTool)
	}
	owner := p.UserID
	title := truncate(textplain.Title(in.Title), notifyTitleMax)
	if title == "" {
		return toolErr("title is required: one short line your owner reads on a lock screen")
	}
	link, external, refused := notifyLinkFor(in.URL, s.ext.NotifyLinkHosts)
	if refused != "" {
		return toolErr("%s", refused)
	}
	if link == "" && c.agent != nil {
		link = ui.AgentPath(c.agent.Name)
	}
	if link == "" {
		link = "/"
	}
	body := from.fromLine()
	if b := textplain.Title(in.Body); b != "" {
		body = truncate(body+": "+b, notifyBodyMax)
	} else {
		body += "."
	}

	var status, reason string
	delivered, devices := 0, 0
	if !s.notifySenders.allow(notifyBucketNotify+from.key, s.now()) {
		s.logPushDrop(notifyTag(from), push.EventAgentNotify, owner, pushDropSenderCap)
		status, reason = "suppressed", pushDropSenderCap
	} else {
		data := map[string]string{"url": link}
		if external != "" {
			// The service worker opens it in a new window (sw.js), never in a
			// muster tab; the app opens the muster path, which redirects.
			data["open"] = "external"
		}
		out := &pushOutcome{}
		var wg sync.WaitGroup
		s.goNotify(owner, push.EventAgentNotify, RouterNotification{
			Type: taskNotificationType, Title: title, Body: body, Tag: notifyTag(from), Data: data,
			wait: &wg, outcome: out,
		}, nil)
		done := make(chan struct{})
		safeGo(s.logger, "notify_owner wait", func() { wg.Wait(); close(done) })
		select {
		case <-done:
		case <-time.After(notifyWaitMax):
		case <-ctx.Done():
		}
		var drops []string
		delivered, devices, drops = out.read()
		status, reason = notifyOutcomeStatus(delivered, drops)
	}
	s.auditNotify(ctx, p, from, title, link, external, status, reason, delivered)
	res := map[string]any{"status": status, "from": from.fromLine(), "title": title, "url": firstNonBlank(external, link)}
	switch status {
	case "delivered":
		res["devices"] = delivered
		res["note"] = "A push service accepted it for " + strconv.Itoa(delivered) + " of " + strconv.Itoa(devices) +
			" device(s). That is delivery to the device, not proof your owner has read it."
	case "unknown":
		res["note"] = "The send did not finish in time; it may still arrive."
	default:
		res["reason"] = reason
		res["note"] = "Not delivered: " + pushDropSentence(reason) + "."
	}
	return toolJSON(res)
}

// notifyTag is a sender's notifications' tag: one per sender, so a newer one
// from the same sender replaces the older on the device instead of stacking.
func notifyTag(from notifySender) string {
	sum := sha256.Sum256([]byte(from.key))
	return "notify-" + hex.EncodeToString(sum[:6])
}

// --- the audit row, and the owner's read of it ---

// notifyAuditStore is the users store's audit writer and the owner's own
// read (users.PGStore), asserted like teamAuditor.
type notifyAuditStore interface {
	Audit(ctx context.Context, by users.Actor, action, object string, detail map[string]any) error
	OwnAudit(ctx context.Context, uid int64, action string, limit int) ([]users.AuditEntry, error)
}

// auditNotify writes notify_owner's one audit_log row, as the owner (the
// principal the agent acts as), naming the sender, the title, the link and
// the outcome. A failed write is logged, never silent.
func (s *Server) auditNotify(ctx context.Context, p authz.Principal, from notifySender, title, link, external, status, reason string, delivered int) {
	a, ok := s.ext.Users.(notifyAuditStore)
	if !ok {
		s.logger.Printf("notify: %s → owner %d: %q %s (no audit store: not recorded)", from.key, p.UserID, title, status)
		return
	}
	detail := map[string]any{"from": from.label, "kind": from.kind, "title": title, "url": firstNonBlank(external, link), "status": status}
	if reason != "" {
		detail["reason"] = reason
	}
	if delivered > 0 {
		detail["devices"] = delivered
	}
	if err := a.Audit(ctx, users.Actor{UserID: p.UserID, Via: p.Via}, notifyAuditAction, from.key, detail); err != nil {
		s.logger.Printf("notify: audit %s → owner %d: %v", from.key, p.UserID, err)
	}
}

// accountNotifyLog is Account → Notifications' list of the notifications the
// owner's agents sent them: the newest notifyAuditShown audit rows, the
// owner's own (row-level security, and the owner named in the read).
func (s *Server) accountNotifyLog(r *http.Request) string {
	a, ok := s.ext.Users.(notifyAuditStore)
	p, signed := authz.From(r.Context())
	if !ok || !signed || p.UserID <= 0 {
		return ""
	}
	rows, err := a.OwnAudit(r.Context(), p.UserID, notifyAuditAction, notifyAuditShown)
	if err != nil {
		s.logger.Printf("notify: read the notify log of %d: %v", p.UserID, err)
		return problemNode("The notifications your agents sent could not be read.")
	}
	var b strings.Builder
	b.WriteString(`<div data-testid="notify-log" class="flex flex-col gap-2"><p class="text-sm font-medium text-fg">Notifications your agents sent you</p>`)
	if len(rows) == 0 {
		b.WriteString(`<p class="text-sm text-fg2">None yet. An agent sends one with its notify_owner tool.</p></div>`)
		return b.String()
	}
	b.WriteString(`<ul class="flex flex-col gap-2">`)
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	for _, e := range rows {
		outcome := str(e.Detail, "status")
		if why := str(e.Detail, "reason"); why != "" {
			outcome += " — " + pushDropSentence(why)
		}
		b.WriteString(`<li data-testid="notify-log-row" class="flex flex-col gap-1 rounded-md bg-bg px-3 py-2 ring-1 ring-inset ring-edge">` +
			`<span class="text-sm text-fg">` + html.EscapeString(str(e.Detail, "title")) + `</span>` +
			`<span class="text-xs text-fg2">` + html.EscapeString(e.At.UTC().Format("2006-01-02 15:04 UTC")+" · from "+str(e.Detail, "from")+
			" ("+str(e.Detail, "kind")+") · "+outcome) + `</span>` +
			`<span class="break-all text-xs text-fg2">` + html.EscapeString(str(e.Detail, "url")) + `</span></li>`)
	}
	b.WriteString(`</ul></div>`)
	return b.String()
}

// --- page_update's push ---

// pageUpdated tells the owner an agent published a NEW VERSION of a page:
// "Page updated: <title> (v<N>)", opening that version's preview, under the
// page_updated toggle and the sender's own cap (its page_update bucket), so
// an agent iterating on a page buzzes at most notifySenderBurst times per
// notifySenderWindow — and each version replaces the last on the device
// (one tag per page).
func (s *Server) pageUpdated(c toolCaller, p pageUpdatedRef) {
	tag := "page-" + strconv.FormatInt(p.id, 10) + "-updated"
	if !s.notifySenders.allow(notifyBucketPageUpdate+c.askerKey(), s.now()) {
		s.logPushDrop(tag, push.EventPageUpdated, p.owner, pushDropSenderCap)
		return
	}
	s.pushTask(p.owner, push.EventPageUpdated, RouterNotification{
		Type:  taskNotificationType,
		Title: "Page updated: " + truncate(p.title, 70) + " (v" + strconv.Itoa(p.version) + ")",
		Body:  "From " + truncate(p.label, 60) + ". Tap to open this version.",
		Tag:   tag,
		Data:  map[string]string{"url": ui.PagePreviewPath(p.id) + "?v=" + strconv.Itoa(p.version)},
	})
}

// pageUpdatedRef is what pageUpdated needs of the page and its new version.
type pageUpdatedRef struct {
	id, owner    int64
	version      int
	title, label string
}

// notifyOwnerOn reports whether notify_owner is offered: muster's own push is
// configured and the push feature is not called off.
func (s *Server) notifyOwnerOn() bool {
	return s.pushServed() && !s.featureOff(features.Push)
}
