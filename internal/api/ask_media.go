package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/askattach"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/redact"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uploads"
)

// ---------------------------------------------------------------------------
// WHAT AN AGENT'S ASK CARRIES BESIDE ITS QUESTION (migration 0143;
// docs/attention.md "Attachments and context"): files, and why it is here.
//
//	POST /mirror/ask-attachments   a mirror host sends a session's file (mirror token)
//	POST /agent/ask-attachments    a dispatched pod sends its agent's file (agent token)
//	GET  /attention/asks/{id}/attachments/{upload}   the file, to whoever may read the ask
//
// FILES. muster cannot read an agent's filesystem, so the file is sent from the
// agent's side into muster's ONE upload store (internal/uploads, the composer's
// paperclip's) and the ask carries the upload's id (internal/askattach says
// who sends it). attention_ask's attachment_ids are then checked HERE, before
// the ask is filed: each must be an upload the ask's owner can read (row-level
// security: the principal the call runs as), not revoked, of an Allowed type
// and within askattach.MaxBytes. An id that is not — another owner's, a
// revoked one, one that does not exist — is refused with ONE sentence, so
// nothing says which.
//
// 🔴 AN ATTACHMENT IS SHOWN ONLY TO WHO MAY READ ITS ASK, NEVER THROUGH AN
// UNGUESSABLE LINK ALONE. GET /attention/asks/{id}/attachments/{upload} needs a
// signed-in session (requireSession); it reads the ask under the request's
// principal (row-level security: the owner, or a member of the team that owns
// it, at viewer — 0127's policy on attention_asks) AND checks the same again
// in Go (askViewerSees), then serves only an upload the ask itself names. The
// signed /f/ link is never handed out for one.
//
// 🔴 THE BYTES ARE SERVED INERT (uploads.SetServeHeaders, the one function
// every route that serves uploaded bytes calls): only PNG, JPEG, GIF and WebP
// inline (an <img> thumbnail); SVG, PDF, text and the rest are attachments
// typed so no browser renders them as a page, with nosniff and the sandbox CSP.
//
// WHY. attention_ask's optional `context` is the agent's own short why
// (attention.CleanWhy: one line, refused past attention.MaxWhyRunes). When it
// gives none, muster stores a fallback read when the ask is filed: the asking
// session's focus title and its last assistant message (askFallbackWhy),
// secrets redacted as a notification's text is (redact.Secrets). The agent's
// context is never mixed with the fallback.
// ---------------------------------------------------------------------------

// askAttachmentPathPrefix is the attachment route's prefix under an ask.
const askAttachmentPathPrefix = "/attachments/"

// askAttachmentPath is the URL of ask id's attachment aid.
func askAttachmentPath(id int64, aid string) string {
	return askPagePrefix + strconv.FormatInt(id, 10) + askAttachmentPathPrefix + aid
}

func (s *Server) registerAskMediaRoutes(mux Mux) {
	mux.HandleFunc("POST "+askattach.MirrorPath, s.requireMirrorHostToken(s.handleMirrorAskAttachment))
	mux.HandleFunc("DELETE "+askattach.MirrorPath+"/{id}", s.requireMirrorHostToken(s.handleMirrorAskAttachmentRevoke))
	mux.HandleFunc("POST "+askattach.AgentPath, s.requireRelayAgent(s.handleAgentAskAttachment))
	mux.HandleFunc("GET "+askPagePrefix+"{id}"+askAttachmentPathPrefix+"{upload}", s.requireSession(s.handleAskAttachment))
}

// askUploadsOff is why an ask's file cannot be stored or attached, "" when it
// can: the upload store must be configured and the feature on.
func (s *Server) askUploadsOff() string {
	if s.uploadPath() == "" {
		return "file attachments are off on this muster (its upload store is not configured), so an ask cannot carry files; ask without them"
	}
	return ""
}

// handleMirrorAskAttachment is POST /mirror/ask-attachments?filename=<name>: a
// mirror host's muster-mirror sends one file of the calling session's
// (X-Muster-Claude-Session, which this host must list running now —
// mirrorSessionOf, the MCP door's rule) as the raw body.
func (s *Server) handleMirrorAskAttachment(w http.ResponseWriter, r *http.Request) {
	t, _ := r.Context().Value(mirrorHostCtxKey{}).(mirror.Token)
	sid := strings.TrimSpace(r.Header.Get(mirrorSessionHeader))
	if _, ok := s.mirrorSessionOf(r.Context(), t.Host, sid); !ok {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "name the calling session (" + mirrorSessionHeader + "): one this host lists running now"})
		return
	}
	s.storeAskAttachment(w, r)
}

// handleAgentAskAttachment is POST /agent/ask-attachments?filename=<name>: a
// dispatched agent's pod (`ccd ask-attach`) sends one workspace file, on the
// agent's own token, as the raw body.
// handleMirrorAskAttachmentRevoke is DELETE /mirror/ask-attachments/{id}: a
// mirror host takes back a file it uploaded for an attention_ask that was not
// filed (internal/mirrorhost revokeAskUploads) — the upload is revoked and its
// object deleted (uploads.Service.Revoke), as the composer's × does. Under the
// host owner's principal (row-level security: only that owner's uploads), for
// a session this host lists, as the upload itself was; any other id is 404.
func (s *Server) handleMirrorAskAttachmentRevoke(w http.ResponseWriter, r *http.Request) {
	if s.askUploadsOff() != "" {
		http.NotFound(w, r)
		return
	}
	t, _ := r.Context().Value(mirrorHostCtxKey{}).(mirror.Token)
	if _, ok := s.mirrorSessionOf(r.Context(), t.Host, strings.TrimSpace(r.Header.Get(mirrorSessionHeader))); !ok {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "name the calling session (" + mirrorSessionHeader + "): one this host lists running now"})
		return
	}
	id := r.PathValue("id")
	objErr, err := s.ext.Uploads.Revoke(r.Context(), id)
	switch {
	case errors.Is(err, uploads.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.logger.Printf("ask attachments: revoking %s: %v", loggedID(id), err)
		http.Error(w, "the upload could not be revoked", http.StatusInternalServerError)
		return
	case objErr != nil:
		s.logger.Printf("ask attachments: %s revoked; %v", loggedID(id), objErr)
	}
	s.logger.Printf("ask attachments: host %s revoked %s (its ask was not filed)", t.Host, loggedID(id))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentAskAttachment(w http.ResponseWriter, r *http.Request) {
	s.storeAskAttachment(w, r)
}

// storeAskAttachment stores the request body as one upload of the request's
// owner, for an ask, and answers askattach.Record with 201. 🔴 THE BYTES ARE
// CHECKED HERE, WHATEVER SENT THEM: capped at askattach.MaxBytes (and the
// deployment's own cap) while they stream, typed by sniffing them, and refused
// unless askattach.Allowed — the sender checks the same, but anything holding
// the token can call this route. It shares the composer's in-flight limits.
func (s *Server) storeAskAttachment(w http.ResponseWriter, r *http.Request) {
	if why := s.askUploadsOff(); why != "" {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": why, "code": "uploads_off"})
		return
	}
	svc := s.ext.Uploads
	limit := min(askattach.MaxBytes, svc.MaxBytes)
	if r.ContentLength > limit {
		s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": askattach.ErrTooLarge.Error(), "code": "too_large"})
		return
	}
	owner := requestOwner(r.Context())
	leave, err := s.uploadLimits.enter(owner)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": UploadBusy, "code": "busy"})
		return
	}
	defer leave()
	deadline := time.Now().Add(s.uploadLimits.timeout())
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	ctx, cancel := context.WithDeadline(r.Context(), deadline.Add(30*time.Second))
	defer cancel()
	name := r.URL.Query().Get("filename")
	st, err := svc.PutChecked(ctx, http.MaxBytesReader(w, r.Body, limit+1), name, limit, attention.MaxTimeout, askattach.Allowed)
	if err != nil {
		var refused *uploads.RefusedTypeError
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &refused):
			s.writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": askattach.Refused("the file", refused.ContentType), "code": "unsupported_media_type"})
		case errors.Is(err, uploads.ErrTooLarge), errors.As(err, &tooBig):
			s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": askattach.ErrTooLarge.Error(), "code": "too_large"})
		case errors.Is(err, uploads.ErrEmpty):
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": UploadEmpty, "code": "empty"})
		case isTimeout(err):
			s.writeJSON(w, http.StatusRequestTimeout, map[string]any{"error": UploadTooSlow, "code": "too_slow"})
		default:
			s.logger.Printf("ask attachments: storing a file failed: %v", err)
			s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": UploadStoreFailed, "code": "store_failed"})
		}
		return
	}
	s.logger.Printf("ask attachments: user=%d stored %s type=%s bytes=%d", owner, st.PublicID, st.ContentType, st.Size)
	s.writeJSON(w, http.StatusCreated, askattach.Record{ID: st.PublicID, Name: st.Filename, Type: st.ContentType, Size: st.Size})
}

// askAttachmentsRefused is the ONE sentence for an attachment id the caller
// cannot attach: another owner's, revoked, or missing alike.
func askAttachmentsRefused(id string) string {
	return "no such upload to attach: " + strconv.Quote(id) + " (attach only files you uploaded for this ask)"
}

// askAttachmentsFor is ids as the ask stores them (attention.Attachment), read
// under ctx's principal, or the refusal: uploads off, too many, an id the
// caller cannot read or that is revoked, a type askattach.Allowed refuses, or
// one over askattach.MaxBytes. Neither errs nor reads for no ids.
func (s *Server) askAttachmentsFor(ctx context.Context, ids []string) ([]attention.Attachment, string) {
	if len(ids) == 0 {
		return nil, ""
	}
	if why := s.askUploadsOff(); why != "" {
		return nil, why
	}
	if len(ids) > askattach.MaxCount {
		return nil, "at most " + strconv.Itoa(askattach.MaxCount) + " attachments"
	}
	out := make([]attention.Attachment, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		u, err := s.ext.Uploads.Store.Get(ctx, id)
		switch {
		case errors.Is(err, uploads.ErrNotFound), err == nil && u.Revoked():
			return nil, askAttachmentsRefused(id)
		case err != nil:
			s.logger.Printf("ask attachments: read %s: %v", loggedID(id), err)
			return nil, "the attachments could not be read; try again"
		case !askattach.Allowed(u.ContentType):
			return nil, askattach.Refused(strconv.Quote(id), u.ContentType)
		case u.Size > askattach.MaxBytes:
			return nil, askattach.ErrTooLarge.Error()
		}
		out = append(out, attention.Attachment{ID: u.PublicID, Name: u.Filename, Type: u.ContentType, Size: u.Size})
	}
	return out, ""
}

// askPathsRefusal is attention_ask's answer to attachments (PATHS) that reached
// muster itself: muster cannot read the caller's files. With uploads off, that
// is the answer. A mirrored session's host uploads them and sends ids instead
// (an older muster-mirror does not); a claude-code agent uploads each with
// `ccd ask-attach` (shipsCcd); any other kind of agent has no way to, and is
// told attachments are not available to it.
func (s *Server) askPathsRefusal(c toolCaller) string {
	if why := s.askUploadsOff(); why != "" {
		return why
	}
	if c.agent != nil {
		if !shipsCcd(*c.agent) {
			return askAttachUnavailable + "; ask without attachments"
		}
		return "muster cannot read files in your workspace: run `ccd ask-attach <path>` in your shell for each file (it prints its id), then call attention_ask with attachment_ids: [those ids] instead of attachments"
	}
	return "this host's muster-mirror did not upload the attachments (it is older than muster's attachment support): update muster-mirror on this host, or ask without attachments"
}

// shipsCcd reports whether agent a's pod carries ccd — and so `ccd
// ask-attach`: the claude-code kind's image alone.
func shipsCcd(a agents.Agent) bool { return a.Kind == agents.KindClaudeCode }

// askAttachUnavailable is what an agent with no way to upload a file is told.
const askAttachUnavailable = "Attachments are not available to you: an ask from you carries text only"

// askWhyFor is the why ask stores: the agent's own context when it gave one,
// else the fallback (askFallbackWhy).
func (s *Server) askWhyFor(ctx context.Context, c toolCaller, given string) *attention.AskWhy {
	if strings.TrimSpace(given) != "" {
		return &attention.AskWhy{Text: given}
	}
	return s.askFallbackWhy(ctx, c)
}

// askFallbackTimeout bounds the fallback's transcript read: the ask is filed
// without the last message rather than late.
const askFallbackTimeout = 3 * time.Second

// askFallbackWhy is the why muster stores for an ask that names none: the
// asking session's focus title (internal/focus) and its last assistant message
// (lastAssistantWords), each with anything shaped like a secret redacted. nil
// when there is neither, and for a caller that is not an agent or a mirrored
// Claude Code session.
func (s *Server) askFallbackWhy(ctx context.Context, c toolCaller) *attention.AskWhy {
	var sub focus.Subject
	switch {
	case c.agent != nil:
		sub = focus.AgentSubject(c.agent.ID)
	case c.asker.MirrorSession != "" && !c.openCodeSession():
		sub = focus.MirrorSubject(c.asker.MirrorSession)
	default:
		return nil
	}
	var w attention.AskWhy
	if s.ext.Focus != nil {
		if f, ok, err := s.ext.Focus.Get(ctx, sub); err == nil && ok && f.Has() {
			w.Focus = redact.Secrets(f.Title)
		}
	}
	w.Last = redact.Secrets(s.lastAssistantWords(ctx, c))
	if w.Focus == "" && w.Last == "" {
		return nil
	}
	return &w
}

// lastAssistantWords is the newest assistant message of c's conversation, ""
// when none is read in askFallbackTimeout. It reads through nudgeTranscript —
// the chief nudge's ONE transcript read: a dispatched agent's delivery
// session's recent messages, or a mirrored session's backfill from its host.
func (s *Server) lastAssistantWords(ctx context.Context, c toolCaller) string {
	rctx, cancel := context.WithTimeout(ctx, askFallbackTimeout)
	defer cancel()
	var cand nudgeCandidate
	switch {
	case c.agent != nil:
		if s.ext.Agents == nil {
			return ""
		}
		sess, err := s.deliverySession(rctx, *c.agent)
		if err != nil {
			return ""
		}
		cand.Session = chiefnudge.Session{Kind: chiefnudge.KindAgent, Key: strconv.FormatInt(c.agent.ID, 10), AgentID: c.agent.ID, ChatSessionID: sess.ID}
	default:
		e, ok := s.mirrorSessionOf(rctx, c.asker.MirrorHost, c.asker.MirrorSession)
		if !ok {
			return ""
		}
		cand.Session = chiefnudge.Session{Kind: chiefnudge.KindMirror, Key: c.asker.MirrorSession, Host: e.Host, TmuxSession: e.Session, Window: e.Window}
	}
	read, err := s.nudgeTranscript(rctx, cand)
	if err != nil && len(read.Msgs) == 0 {
		return ""
	}
	for i := len(read.Msgs) - 1; i >= 0; i-- {
		if m := read.Msgs[i]; m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
			return m.Text
		}
	}
	return ""
}

// askViewerSees reports whether ctx's viewer may see ask a's attachments: its
// owner, or a member (at viewer or above) of the team that owns it, through a
// door that carries teams — the rule row-level security already applied to the
// read, checked again so it holds with row-level security rolled back.
func askViewerSees(ctx context.Context, a attention.Ask) bool {
	if a.Owner > 0 && a.Owner == requestOwner(ctx) {
		return true
	}
	p, ok := authz.From(ctx)
	if !ok || p.All() || a.Owner <= 0 {
		return false
	}
	role, in := p.TeamScope().RoleIn(a.Owner)
	return in && role.AtLeast(authz.TeamViewer)
}

// handleAskAttachment is GET /attention/asks/{id}/attachments/{upload}: one file
// ask id carries, to who may read the ask (askViewerSees). Every refusal —
// another owner's ask, a missing one, an id the ask does not name, an object
// the store no longer holds — is the same 404.
func (s *Server) handleAskAttachment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.ext.Attention == nil || s.ext.Uploads == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := askIDParam(r)
	aid := r.PathValue("upload")
	if !ok || !uploads.ValidPublicID(aid) {
		http.NotFound(w, r)
		return
	}
	a, err := s.ext.Attention.Get(r.Context(), id)
	if err != nil || a.ToChief || !askViewerSees(r.Context(), a) {
		if err != nil && !errors.Is(err, attention.ErrNotFound) {
			s.logger.Printf("ask attachments: read ask %d: %v", id, err)
		}
		http.NotFound(w, r)
		return
	}
	var file *attention.Attachment
	for i := range a.Attachments {
		if a.Attachments[i].ID == aid {
			file = &a.Attachments[i]
			break
		}
	}
	if file == nil {
		http.NotFound(w, r)
		return
	}
	// 🔴 THE UPLOAD'S OWN ROW DECIDES TOO, ON EVERY GET: revoked, past its
	// expiry, gone, or not the ask's owner's is 404 — whether or not its object
	// is still in the store (a revoke whose object delete failed, a lifecycle
	// rule that has not run yet). Read by public id under the system principal
	// (uploads.Store.LinkTarget), because a team member reading the team's ask
	// cannot read the owner-isolated row; the ask was authorised above.
	if u, err := s.ext.Uploads.Store.LinkTarget(r.Context(), file.ID); err != nil || u.Revoked() || !s.now().Before(u.ExpiresAt) || u.OwnerID != a.Owner {
		if err != nil && !errors.Is(err, uploads.ErrNotFound) {
			s.logger.Printf("ask attachments: ask %d file %s row: %v", id, file.ID, err)
		}
		http.NotFound(w, r)
		return
	}
	body, size, err := s.ext.Uploads.Objects.Get(r.Context(), uploads.ObjectKeyFor(file.ID))
	if err != nil {
		s.logger.Printf("ask attachments: ask %d file %s: %v", id, file.ID, err)
		http.NotFound(w, r)
		return
	}
	defer body.Close()
	uploads.SetServeHeaders(w.Header(), file.Type, file.Name, size)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// askMediaView is ask a's attachments as ui draws them, each linked to its
// authorised route.
func askMediaView(askID int64, files []attention.Attachment) []ui.AskFile {
	out := make([]ui.AskFile, 0, len(files))
	for _, f := range files {
		out = append(out, ui.AskFile{Name: f.Name, Type: f.Type, Size: uploads.HumanSize(f.Size), URL: askAttachmentPath(askID, f.ID),
			Image: uploads.InlineImage(f.Type)})
	}
	return out
}

// askWhyView is ask a's why as ui draws it.
func askWhyView(w *attention.AskWhy) ui.AskWhy {
	if w == nil {
		return ui.AskWhy{}
	}
	return ui.AskWhy{Text: w.Text, Focus: w.Focus, Last: w.Last}
}

// heldWhy is a held AskUserQuestion's why, drawn at render time (operator
// decision on #767: "Context now"): the same fallback an attention_ask without
// context stores — the session's focus title and its last words before the
// question (attentionItem.heldLast) — redacted and bounded the same way
// (redact.Secrets, attention.CleanWhy). The zero value for any other item and
// for a held question with neither.
func heldWhy(it attentionItem) ui.AskWhy {
	if it.Kind != attentionKindQuestion || it.openCode != nil {
		return ui.AskWhy{}
	}
	w, err := attention.CleanWhy(&attention.AskWhy{Focus: redact.Secrets(it.Focus), Last: redact.Secrets(it.heldLast)})
	if err != nil {
		return ui.AskWhy{}
	}
	return askWhyView(w)
}

// askMediaHTML is an ask's why line and media strip, "" for neither: what the
// Asks card and the ask page draw under the question.
func askMediaHTML(askID int64, why *attention.AskWhy, files []attention.Attachment) string {
	return ui.AskContextLine(askWhyView(why)) + ui.AskMediaStrip(askMediaView(askID, files))
}
