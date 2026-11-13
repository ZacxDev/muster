package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uploads"
)

// A PLUGIN BOUND TO AN OFFICER, MUSTER'S HALF (internal/plugins outbound.go
// holds the rules; docs/plugins.md "A plugin bound to an officer"):
//
//   - the binding (deliver_to): the /plugins card's form, the admin REST door
//     and `muster plugins deliver-to`, all one store call (SetDeliverTo);
//   - the media upload at the pre-approval check, into the object store;
//   - the owner's view of each medium on the Reply drafts card;
//   - officer_post_draft, a builtin officer's tool;
//   - what a result means for the officer's item.

// officerPluginLink is plugins.OfficerLink over the builtin officer store;
// now is the officer runtime's clock (the one leases are written by).
type officerPluginLink struct {
	store *builtin.Store
	now   func() time.Time
}

var _ plugins.OfficerLink = officerPluginLink{}

func (l officerPluginLink) Defined(ctx context.Context, owner int64, officer string) (bool, error) {
	return l.store.Defined(ctx, owner, officer)
}

func (l officerPluginLink) EnqueueTx(ctx context.Context, tx pgx.Tx, owner int64, it plugins.OfficerItem) (int64, error) {
	id, err := l.store.EnqueueTx(ctx, tx, owner, builtin.NewItem{Officer: it.Officer, Kind: it.Kind, SubjectKey: it.SubjectKey,
		Payload: it.Payload}, time.Now())
	if errors.Is(err, builtin.ErrNotFound) {
		return 0, plugins.ErrOfficerUnavailable
	}
	return id, err
}

func (l officerPluginLink) WaitOnDraftTx(ctx context.Context, tx pgx.Tx, owner int64, officer string, item int64, instance string) error {
	err := l.store.WaitOnDraftTx(ctx, tx, owner, officer, item, instance, l.now())
	if errors.Is(err, builtin.ErrNotHeld) {
		return plugins.ErrItemNotHeld
	}
	return err
}

// linkPluginsToOfficers wires the plugin store to the builtin officer store
// (EnableOfficerStore): a plugin can then be bound to an officer.
func (s *Server) linkPluginsToOfficers(store *builtin.Store) {
	if ps, ok := s.ext.Plugins.(*plugins.PGStore); ok && store != nil {
		ps.WithOfficerLink(officerPluginLink{store: store, now: func() time.Time {
			if s.officerRT != nil {
				return s.officerRT.clock()
			}
			return time.Now()
		}})
	}
}

// --- the binding ----------------------------------------------------------------

// pluginDeliverToAPIPath is the admin REST door's route.
const pluginDeliverToAPIPath = "/api/plugins/{id}/deliver-to"

// setDeliverTo is the one binding change every door makes: 200 with the
// plugin, or the refusal's status and sentence.
func (s *Server) setDeliverTo(ctx context.Context, id int64, deliverTo string) (plugins.Plugin, int, string) {
	p, changed, err := s.ext.Plugins.SetDeliverTo(ctx, id, strings.TrimSpace(deliverTo))
	switch {
	case err == nil:
		if changed {
			s.logger.Printf("plugins: plugin %d deliver_to is now %q", id, deliverTo)
		}
		return p, http.StatusOK, ""
	case errors.Is(err, plugins.ErrNotFound):
		return p, http.StatusNotFound, "No such live plugin."
	case errors.Is(err, plugins.ErrOfficerUnavailable):
		return p, http.StatusBadRequest, "That officer is not defined in the built-in store for this plugin's owner " +
			"(muster officer def put), or the built-in store is off (MUSTER_OFFICERS unset)."
	case errors.Is(err, plugins.ErrInvalid):
		return p, http.StatusBadRequest, strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")
	}
	s.logger.Printf("plugins: plugin %d deliver_to: %v", id, err)
	return p, http.StatusInternalServerError, "The binding could not be changed."
}

// handlePluginDeliverToForm is POST /plugins/{id}/deliver-to {deliver_to}:
// the /plugins card's form (revoke's gate). It answers the page again.
func (s *Server) handlePluginDeliverToForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such live plugin.")
		return
	}
	if _, code, why := s.setDeliverTo(r.Context(), id, r.FormValue("deliver_to")); code != http.StatusOK {
		s.renderPluginsPage(w, r, code, "", why)
		return
	}
	http.Redirect(w, r, pluginsPath, http.StatusSeeOther)
}

// handlePluginDeliverToAPI is PUT /api/plugins/{id}/deliver-to
// {"deliver_to": "officer:<id>" | ""}: `muster plugins deliver-to`, behind
// the hook tier, acting as the caller.
func (s *Server) handlePluginDeliverToAPI(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such plugin"})
		return
	}
	var in struct {
		DeliverTo *string `json:"deliver_to"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.DeliverTo == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"deliver_to": "officer:<id>" or ""}`})
		return
	}
	p, code, why := s.setDeliverTo(r.Context(), id, *in.DeliverTo)
	if code != http.StatusOK {
		s.writeJSON(w, code, map[string]any{"error": why})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"plugin_id": p.ID, "plugin_name": p.Name, "deliver_to": p.DeliverTo})
}

// --- the media upload -------------------------------------------------------------

// MediaSHA256Header is the upload's declared SHA-256.
const MediaSHA256Header = "X-Content-SHA256"

// handlePluginDraftMedia is POST /plugin/v1/drafts/{id}/media?ref=<ref>
// (CapDraftChecks): one ref's bytes for an outbound draft awaiting its check,
// the raw body typed by Content-Type and hashed by X-Content-SHA256. muster
// sniffs and hashes the bytes itself and refuses any disagreement; it writes
// them to the object store under a random key, then records them (the store
// refuses a second, different upload for a ref, and the object is removed).
func (s *Server) handlePluginDraftMedia(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such outbound draft of this plugin's"})
		return
	}
	if s.ext.Uploads == nil || s.ext.Uploads.Objects == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no object store (MUSTER_UPLOAD_* unset), so it cannot take media"})
		return
	}
	ref := r.URL.Query().Get("ref")
	ctype := strings.TrimSpace(strings.ToLower(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	want := strings.TrimSpace(r.Header.Get(MediaSHA256Header))
	max, ok := plugins.MediaTypes[ctype]
	switch {
	case !plugins.ValidMediaRef(ref):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ref is the draft's media ref, <angle>/<id>"})
		return
	case !ok:
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Content-Type is image/png, image/jpeg, image/webp or video/mp4"})
		return
	case !bodySHA256Shape.MatchString(want):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": MediaSHA256Header + " is the body's SHA-256, 64 lower-case hex digits"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max+1))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) || int64(len(body)) > max {
		s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("a %s medium is at most %d bytes", ctype, max)})
		return
	}
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body did not arrive whole"})
		return
	}
	sum := sha256.Sum256(body)
	switch got := hex.EncodeToString(sum[:]); {
	case len(body) == 0:
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is empty"})
		return
	case got != want:
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body's SHA-256 is " + got + ", not the " + MediaSHA256Header + " sent"})
		return
	case uploads.Sniff(body) != ctype:
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the bytes are " + uploads.Sniff(body) + ", not the Content-Type sent"})
		return
	}
	p := pluginFromContext(r.Context())
	key, err := mediaObjectKey(id)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the medium could not be stored"})
		return
	}
	if err := s.ext.Uploads.Objects.Put(r.Context(), key, bytes.NewReader(body), int64(len(body)), ctype); err != nil {
		s.logger.Printf("plugins: plugin %d draft %d media %s: object store: %v", p.ID, id, ref, err)
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the object store did not take the medium"})
		return
	}
	m, fresh, err := s.ext.Plugins.UploadDraftMedia(r.Context(), p.ID, id, plugins.DraftMedia{Ref: ref, SHA256: want,
		ContentType: ctype, Size: int64(len(body)), Key: key})
	if err != nil || !fresh {
		// not recorded (refused, or these bytes were already stored): the new
		// object is nobody's
		if derr := s.ext.Uploads.Objects.Delete(context.WithoutCancel(r.Context()), key); derr != nil {
			s.logger.Printf("plugins: draft %d media %s: remove the unrecorded object: %v", id, ref, derr)
		}
	}
	switch {
	case err == nil:
		code := http.StatusOK
		if fresh {
			code = http.StatusCreated
		}
		s.draftStatusMoved(p.OwnerID, id)
		s.writeJSON(w, code, map[string]any{"draft_id": id, "ref": m.Ref, "sha256": m.SHA256, "content_type": m.ContentType, "size": m.Size})
	case errors.Is(err, plugins.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")})
	case errors.Is(err, plugins.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such outbound draft of this plugin's"})
	case errors.Is(err, plugins.ErrMediaConflict):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "other bytes are already stored for this ref; a ref's bytes are fixed once uploaded"})
	case errors.Is(err, plugins.ErrDraftState):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the draft is not awaiting its check, so its media are fixed"})
	default:
		s.logger.Printf("plugins: plugin %d draft %d media %s: %v", p.ID, id, ref, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the medium could not be recorded"})
	}
}

// mediaObjectKey is a fresh object key for one of draft id's media: the
// fixed prefix, the draft's number and a random id — nothing a plugin sent.
func mediaObjectKey(id int64) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return plugins.MediaKeyPrefix + strconv.FormatInt(id, 10) + "/" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// handleDraftMediaServe is GET /plugins/drafts/{id}/media/{n}: the owner's
// view of an outbound draft's n-th medium (0-based, the refs' order) on its
// card. Row-level security decides whose; the bytes are served inert
// (uploads.SetServeHeaders) with range support, so a video plays and seeks.
func (s *Server) handleDraftMediaServe(w http.ResponseWriter, r *http.Request) {
	id, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	n, err2 := strconv.Atoi(r.PathValue("n"))
	if err1 != nil || err2 != nil || id <= 0 || n < 0 || s.ext.Uploads == nil || s.ext.Uploads.Objects == nil {
		http.NotFound(w, r)
		return
	}
	c, err := s.ext.Plugins.DraftCard(r.Context(), id)
	if err != nil || !c.Draft.IsOutbound() || n >= len(c.Draft.MediaRefs) {
		http.NotFound(w, r)
		return
	}
	m, ok := c.Draft.MediaFor(c.Draft.MediaRefs[n])
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, _, err := s.ext.Uploads.Objects.Get(r.Context(), m.Key)
	if err != nil {
		s.logger.Printf("plugins: draft %d medium %d: %v", id, n, err)
		http.Error(w, "the medium could not be read right now", http.StatusBadGateway)
		return
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, plugins.MaxVideoBytes+1))
	if err != nil {
		http.Error(w, "the medium could not be read right now", http.StatusBadGateway)
		return
	}
	uploads.SetServeHeaders(w.Header(), m.ContentType, m.Ref, -1)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// draftMediaLink is the card's link to an outbound draft's n-th medium.
func draftMediaLink(id int64, n int) string {
	return pluginsPath + "/drafts/" + strconv.FormatInt(id, 10) + "/media/" + strconv.Itoa(n)
}

// --- the card ---------------------------------------------------------------------

// outboundCardHTML is the part of an OUTBOUND draft's card above its text:
// the venue and target — for a reply target, the human reply muster received
// with that thread, escaped and labelled untrusted — and every uploaded
// medium rendered (an image) or playable (a video), with its ref and SHA-256.
func outboundCardHTML(c plugins.DraftCard) string {
	esc := html.EscapeString
	d := c.Draft
	t := d.Target()
	var b strings.Builder
	what := "New post"
	if t.Kind == plugins.TargetReply {
		what = "Reply to " + esc(flows.VisibleEscape(t.Ref))
	}
	b.WriteString(`<div class="` + ui.AsksQuoteClass + `" data-draft-outbound data-draft-venue="` + esc(d.VenueText()) +
		`" data-draft-target-kind="` + esc(t.Kind) + `">
<span class="break-all text-xs text-fg2"><span class="font-semibold">Outbound · ` + esc(d.VenueText()) + `</span> · ` + what + `</span>
`)
	if t.Kind == plugins.TargetReply {
		if m := c.ReplyTo; m != nil {
			b.WriteString(`<span class="break-all text-xs text-fg2" data-draft-reply-to>From ` + esc(m.Sender) + ` · untrusted · message ` +
				strconv.FormatInt(m.ID, 10) + `</span>
<pre class="whitespace-pre-wrap break-words text-xs text-fg2" data-draft-reply-to-body>` + esc(m.Body) + `</pre>
`)
		} else {
			b.WriteString(`<span class="text-xs text-fg2" data-draft-reply-to-missing>muster received no reply with this reference: check the target before you approve.</span>
`)
		}
	}
	b.WriteString("</div>\n")
	if len(d.MediaRefs) == 0 {
		return b.String()
	}
	b.WriteString(`<ul class="flex flex-col gap-2" data-draft-media>
`)
	for i, ref := range d.MediaRefs {
		m, ok := d.MediaFor(ref)
		b.WriteString(`<li class="flex flex-col gap-1" data-draft-medium data-ref="` + esc(ref) + `"`)
		if !ok {
			b.WriteString(`><span class="break-all text-xs text-fg2">` + esc(ref) + ` · <span class="font-semibold" data-draft-medium-missing>not uploaded yet</span></span></li>
`)
			continue
		}
		b.WriteString(` data-sha256="` + esc(m.SHA256) + `" data-content-type="` + esc(m.ContentType) + `">`)
		src := esc(draftMediaLink(d.ID, i))
		if strings.HasPrefix(m.ContentType, "video/") {
			b.WriteString(`<video controls preload="metadata" class="max-h-80 w-full rounded-md bg-bg" src="` + src + `" data-draft-video></video>`)
		} else {
			b.WriteString(`<img alt="` + esc(ref) + `" class="max-h-80 w-auto max-w-full rounded-md object-contain" src="` + src + `" data-draft-image>`)
		}
		b.WriteString(`
<span class="break-all font-mono text-xs text-fg2">` + esc(ref) + ` · ` + esc(m.ContentType) + ` · ` + esc(uploads.HumanSize(m.Size)) +
			` · sha256 ` + esc(m.SHA256) + `</span></li>
`)
	}
	b.WriteString("</ul>\n")
	return b.String()
}

// outboundApproveFields are the approve form's hidden fields for an outbound
// draft: the body's and media's SHA-256 values the card showed, which the
// approval binds (plugins.DraftDecision).
func outboundApproveFields(d plugins.Draft) string {
	return `<input type="hidden" name="body_sha256" value="` + plugins.DraftBodySHA256(d.Body) + `">` +
		`<input type="hidden" name="media_sha256" value="` + html.EscapeString(strings.Join(d.MediaSHA256(), ",")) + `">`
}

// formMediaSHA256 is the approve form's media_sha256: nil when the field is
// absent, else its comma-separated values ([] for "").
func formMediaSHA256(r *http.Request) []string {
	if _, ok := r.PostForm["media_sha256"]; !ok {
		return nil
	}
	v := strings.TrimSpace(r.PostForm.Get("media_sha256"))
	if v == "" {
		return []string{}
	}
	return strings.Split(v, ",")
}

// --- officer_post_draft -------------------------------------------------------------

// postDraftToolDef is officer_post_draft's definition.
func postDraftToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: officers.ToolPostDraft,
		Description: "Ask to post something outside — a new post, or a reply on an upstream thread — through the plugin bound to your officer. " +
			"NOTHING IS SENT by this call: it files a draft. The plugin checks it and uploads the media, your owner reads the exact text and sees every image and video, " +
			"and only an approval lets the plugin post exactly that, once. Write the body exactly as it should appear. " +
			"Public links (for example the project's repository) are allowed; internal host names, IP addresses and secrets are refused. " +
			"Give item_id when the draft is for an item you hold: the item then WAITS ON THE DRAFT and leaves your hands (no holder), so do not complete or release it. " +
			"muster completes it with the post's reference once it is posted; if the plugin refuses the draft, your owner rejects it, or it expires, muster closes it and " +
			"queues a draft.refused item saying why (your owner's reason, the plugin's, or muster's), which is where you redraft. " +
			"If the plugin took it and never reported a result, it may have been posted: muster escalates the item to your owner and you must not redraft it.",
		Parameters: objSchema(map[string]any{
			"plugin": strProp("The name of the plugin bound to your officer."),
			"venue":  strProp("Where it goes, as the plugin names it: 1-64 lower-case letters, digits, '.', '_' and '-'."),
			"target": objSchema(map[string]any{
				"kind": map[string]any{"type": "string", "enum": []string{plugins.TargetPost, plugins.TargetReply}},
				"ref":  strProp("For a reply: the upstream reference of what it answers (an item's thread). Omit for a post."),
			}, "kind"),
			"body": strProp("The exact text to post, at most 16 KiB."),
			"media": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": plugins.MaxMediaRefs,
				"description": "Optional, at most 4: media refs as <angle>/<id>, which the plugin resolves on its machine."},
			"item_id": strProp("Optional: the id of an item you hold that this draft is for."),
		}, "plugin", "venue", "target", "body")}
}

// officerOutboundGranted reports whether builtin officer o's type grants
// outbound_drafts (read now, as its owner): THE gate on officer_post_draft
// beside officers.ToolOffered, for the listing and the dispatch.
func (s *Server) officerOutboundGranted(ctx context.Context, o officers.Officer) bool {
	if s.officerStore == nil || !o.IsBuiltin() || s.ext.Plugins == nil {
		return false
	}
	ok, err := s.officerStore.TypeGrants(s.officerPrincipal(ctx, o.Owner), o.Owner, o.ID, builtin.CapOutboundDrafts)
	if err != nil {
		s.logger.Printf("officers: %s: read its type's outbound_drafts grant: %v", o.ID, err)
		return false
	}
	return ok
}

// officerPostDraft is officer_post_draft for instance a of officer o.
func (s *Server) officerPostDraft(ctx context.Context, a agents.Agent, o officers.Officer, c officers.Backend, arguments string) string {
	if !s.officerOutboundGranted(ctx, o) {
		return toolErr("%s is not one of officer %s's tools: its type does not grant %s", officers.ToolPostDraft, o.ID, builtin.CapOutboundDrafts)
	}
	var in struct {
		Plugin string                 `json:"plugin"`
		Venue  string                 `json:"venue"`
		Target plugins.OutboundTarget `json:"target"`
		Body   string                 `json:"body"`
		Media  []string               `json:"media"`
		ItemID officers.ItemID        `json:"item_id"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("arguments are {plugin, venue, target: {kind, ref?}, body, media?, item_id?}: %v", err)
	}
	n := plugins.NewOutbound{Officer: o.ID, Plugin: in.Plugin, Venue: in.Venue, Target: in.Target, Body: in.Body, Media: in.Media}
	if named := strings.TrimSpace(string(in.ItemID)); named != "" {
		// Whether this instance holds it is decided in the filing's
		// transaction, under the item's row lock (plugins.FileOutbound).
		item, err := strconv.ParseInt(named, 10, 64)
		if err != nil || item <= 0 {
			return toolErr("you hold no live claim on item %s: claim it first, or leave item_id out", named)
		}
		n.ItemID = &item
	}
	d, err := s.ext.Plugins.FileOutbound(s.officerPrincipal(ctx, o.Owner), a.ID, o.Owner, a.Name, n)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrItemNotHeld):
		return toolErr("you hold no live claim on item %s: claim it first, or leave item_id out", in.ItemID)
	case errors.Is(err, plugins.ErrNotFound):
		return toolErr("plugin %q is not a live plugin of your owner's bound to officer %s", in.Plugin, o.ID)
	case errors.Is(err, plugins.ErrInvalid), errors.Is(err, plugins.ErrTooManyPendingDrafts):
		return toolErr("%s", strings.TrimPrefix(strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "), "plugins: "))
	default:
		s.logger.Printf("officers: %s %s: %v", a.Name, officers.ToolPostDraft, err)
		return toolErr("the draft could not be filed")
	}
	s.logger.Printf("officers: %s filed outbound draft %d (%s %s on %s)", a.Name, d.ID, d.Target().Kind, d.VenueText(), in.Plugin)
	s.draftStatusMoved(o.Owner, d.ID)
	out := map[string]any{"draft_id": d.ID, "status": d.Status, "check": d.CheckState,
		"note": "filed, NOT sent: the plugin checks it and uploads its media, then your owner decides. Do not say it was posted."}
	if n.ItemID != nil {
		out["item"] = fmt.Sprintf("item %d now waits on this draft with no holder: do not complete or release it. muster completes it when the post goes out, "+
			"or closes it and queues a draft.refused item if the draft is refused, rejected or expires.", *n.ItemID)
	}
	return toolJSON(out)
}

// --- the result, for the officer -----------------------------------------------------

// Who settled an outbound draft unsent, as outboundSettled's caller knows it
// (never inferred from the row: a check refusal and an owner rejection are
// both status rejected).
const (
	// settledByPlugin: a failed send, or a refused check (draftRefused).
	settledByPlugin = "plugin"
	// settledByOwner: the owner rejected it, or withdrew an unclaimed
	// approval (decideDraftAs).
	settledByOwner = "owner"
)

// OwnerRejectedNoReason is a draft.refused item's detail for an owner
// rejection that gave no reason.
const OwnerRejectedNoReason = "rejected by the owner, no reason given"

// settledByMuster: muster ended it by its own rules — it expired, or the
// claim withdrew it for a broken binding (plugins.OutboundEnd).
const settledByMuster = "muster"

// ExpiredDetail is the detail of the draft.refused item for an outbound
// draft that EXPIRED: nobody approved and sent it within plugins.DraftTTL.
const ExpiredDetail = "not sent within 7 days of being written: it expired undecided, or approved but never collected by its plugin"

// draftEndFor is builtin.DraftEnd for OUTBOUND draft d of officer, ended
// unsent — how: plugins.OutboundEnd's values, or "" for a decision (by:
// settledByPlugin or settledByOwner, with detail). THE ONE PLACE an
// outbound draft's unsent end is worded and the officer's follow-up built,
// for every path that ends one: outboundSettled (a plugin's refusal or
// failed send, the owner's rejection) and officerPluginLink.DraftEndedTx
// (the sweep's expiry and lapsed lease, the claim's broken binding, Checks
// replies turned off).
//
//   - a REFUSED, REJECTED, EXPIRED, BINDING-BROKEN or CHECKS-OFF draft can
//     be redrafted:
//     a draft.refused item for the officer, bounded by maxOpenRefusedItems;
//   - an UNCONFIRMED draft cannot: its plugin collected it and never said
//     whether it posted it, so it MAY be live. No follow-up is queued (a
//     redraft would be a blind re-post) and the item is escalated to the
//     officer's owner, who checks the venue and answers.
func draftEndFor(d plugins.Draft, officer string, how plugins.OutboundEnd, by, detail string) builtin.DraftEnd {
	id := strconv.FormatInt(d.ID, 10)
	e := builtin.DraftEnd{Officer: officer, Draft: d.ID, Max: maxOpenRefusedItems}
	if d.OfficerItemID != nil {
		e.Item = *d.OfficerItemID
	}
	switch how {
	case plugins.OutboundUnconfirmed:
		e.Ended = "Draft " + id + " may have been posted: its plugin collected it and reported no result before its lease lapsed, " +
			"so it is unconfirmed. muster will not offer it again and the officer is not asked to redraft it, which could post it twice. " +
			"Check " + d.VenueText() + " for it."
		return e
	case plugins.OutboundExpired:
		e.Ended, by, detail = "draft "+id+" expired, not posted.", settledByMuster, ExpiredDetail
	case plugins.OutboundBindingBroken:
		e.Ended, by, detail = "draft "+id+" was withdrawn at its claim, not posted: "+plugins.BindingBrokenDetail+".", settledByMuster, plugins.BindingBrokenDetail
	case plugins.OutboundChecksOff:
		e.Ended, by, detail = "draft "+id+" was refused unchecked, not posted: "+plugins.ChecksOffDetail+".", settledByMuster, plugins.ChecksOffDetail
	case "":
		ended := "rejected by the owner"
		if by == settledByPlugin {
			ended = "refused by the plugin"
		}
		e.Ended = "draft " + id + " was " + ended + ", not posted."
	}
	// The sweep and the claim have no push to tell the owner with: past the
	// bound their item is escalated to the owner instead.
	e.EscalateAtBound = how != ""
	payload, _ := json.Marshal(map[string]any{"draft_id": d.ID, "item_id": d.OfficerItemID, "venue": d.VenueText(),
		"target": d.Target(), "media": d.MediaRefs, "status": d.Status, "by": by, "detail": detail})
	e.FollowUp = &builtin.NewItem{Officer: officer, Kind: draftRefusedKind, SubjectKey: "draft:" + id, Payload: payload}
	return e
}

// outboundSettled is what follows a fresh result, refusal or rejection of
// OUTBOUND draft d, for its officer: a send reported ok completes the item it
// was filed for (done, with the post's reference — the item waits on the
// draft, builtin.WaitOnDraftTx); anything that ended it unsent (by: the
// plugin's failed send or refused check, or the owner's rejection) queues a
// draft.refused item saying why — the plugin's detail, or the owner's reason
// — so the officer can write a better one, and closes the item the draft was
// for, naming that follow-up (draftEndFor, builtin.DraftEnded). notTold:
// the bound kept the officer from being told, so the CALLER tells the owner.
// Best effort: the draft's own state is already recorded.
func (s *Server) outboundSettled(ctx context.Context, owner int64, d plugins.Draft, by string) (notTold bool) {
	if s.officerStore == nil || d.Officer == nil || !d.IsOutbound() {
		return false
	}
	officer, now := *d.Officer, time.Now()
	ctx = context.WithoutCancel(ctx)
	if d.Status == plugins.DraftSent {
		if d.OfficerItemID != nil {
			// A LATE result (the draft was unconfirmed, its item escalated to
			// the owner asking whether it was posted) settles it too, and the
			// owner's now-answered escalation asks are withdrawn.
			summary := "posted " + d.ExternalRef + " (draft " + strconv.FormatInt(d.ID, 10) + ")"
			settled, withdraw, err := s.officerStore.DraftPosted(ctx, owner, officer, *d.OfficerItemID, d.ID, summary, now)
			if err != nil || !settled {
				s.logger.Printf("plugins: draft %d sent; item %d of %s left as it is (%v)", d.ID, *d.OfficerItemID, officer, err)
			}
			for _, w := range withdraw {
				if _, err := s.cancelAsk(s.attentionPrincipal(ctx, w.Recipient), w.AttentionID); err != nil && !errors.Is(err, attention.ErrNotOpen) {
					s.logger.Printf("plugins: draft %d sent late; withdraw user %d's escalation ask %d: %v", d.ID, w.Recipient, w.AttentionID, err)
				}
			}
		}
		return false
	}
	var detail string
	switch by {
	case settledByOwner:
		if d.Status != plugins.DraftRejected {
			return false
		}
		detail = OwnerRejectedNoReason
		if d.DecisionReason != nil && strings.TrimSpace(*d.DecisionReason) != "" {
			detail = *d.DecisionReason
		}
	case settledByPlugin:
		if d.Status != plugins.DraftFailed && !d.RefusedByCheck() {
			return false
		}
		detail = plugins.RefusalText(d.Detail)
	default:
		return false
	}
	// 🔴 BOUNDED, IN ONE TRANSACTION (builtin.DraftEndedTx): a plugin that
	// refuses every draft would otherwise turn each refusal into a new item
	// and each item into a new draft, with no human in the loop. At most
	// maxOpenRefusedItems wait at once, counted and inserted under one lock so
	// two settling at once cannot both pass the count. Past it nothing is
	// queued, the item the draft was for is STILL finished (saying the
	// officer was not told), and the caller pushes the owner saying so.
	res, err := s.officerStore.DraftEnded(ctx, owner, draftEndFor(d, officer, "", by, detail), now)
	if err != nil {
		s.logger.Printf("plugins: draft %d ended unsent (by %s); %s and its item left as they are: %v", d.ID, by, officer, err)
		return false
	}
	s.logger.Printf("plugins: draft %d ended unsent (by %s); %s item %d for %s (at the bound: %v), its item %q",
		d.ID, by, draftRefusedKind, res.FollowUp, officer, res.AtBound, res.Item)
	return res.AtBound
}

// DraftEndedTx implements plugins.OfficerLink: outbound draft d ended unsent
// by muster's own hand (how), inside the transaction that ended it.
func (l officerPluginLink) DraftEndedTx(ctx context.Context, tx pgx.Tx, d plugins.Draft, how plugins.OutboundEnd) error {
	if d.Officer == nil || !d.IsOutbound() {
		return nil
	}
	_, err := l.store.DraftEndedTx(ctx, tx, d.OwnerID, draftEndFor(d, *d.Officer, how, "", ""), l.now())
	return err
}

// draftRefusedKind is the kind of the item an officer gets for its outbound
// draft that ended unsent.
const draftRefusedKind = "draft.refused"

// maxOpenRefusedItems bounds the draft.refused items one officer may have
// waiting (draftEndFor).
const maxOpenRefusedItems = 3

// outboundAttentionText is an outbound draft's Attention line.
func outboundAttentionText(c plugins.DraftCard) string {
	d := c.Draft
	t := d.Target()
	what := "new post"
	if t.Kind == plugins.TargetReply {
		what = "reply"
	}
	who := ""
	if d.Officer != nil {
		who = " by officer " + *d.Officer
	}
	return "Outbound " + what + " on " + d.VenueText() + who
}

// pluginDeliverToFieldHTML is the create form's Deliver to field, drawn only
// while the built-in officer store is on (a binding needs it).
func (s *Server) pluginDeliverToFieldHTML() string {
	if s.officerStore == nil {
		return ""
	}
	return `<label for="plugin-deliver-to" class="text-sm font-medium text-fg">Deliver to <span class="text-xs text-fg2">optional: officer:&lt;id&gt; to give its messages to a builtin officer instead of your chief</span></label>
<input id="plugin-deliver-to" name="deliver_to" maxlength="120" placeholder="officer:example:main" class="` + flowsInputClass + `" data-testid="plugin-deliver-to">
`
}

// pluginDeliverToFormHTML is a live messages plugin's card form to change
// its binding ("" clears it), drawn while the built-in store is on or the
// plugin is bound.
func (s *Server) pluginDeliverToFormHTML(p plugins.Plugin) string {
	if !p.Has(plugins.CapMessages) || (s.officerStore == nil && p.DeliverTo == nil) {
		return ""
	}
	cur := ""
	if p.DeliverTo != nil {
		cur = *p.DeliverTo
	}
	return `<form method="POST" action="` + pluginsPath + `/` + strconv.FormatInt(p.ID, 10) + `/deliver-to" class="flex flex-wrap items-center gap-2" data-plugin-deliver-to>
<label class="text-xs text-fg2">Deliver to <input name="deliver_to" maxlength="120" value="` + html.EscapeString(cur) + `" placeholder="your chief" class="` + flowsInputClass + `"></label>
<button type="submit" class="` + flowsQuietButton + `">Save</button></form>`
}
