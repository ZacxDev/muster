package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
)

// A PLUGIN'S INSTRUCTION SECTION FOR ITS OWNER'S CHIEF (migration 0141,
// internal/plugins chief_instructions.go, docs/plugins.md#chief-instructions).
//
//   - The plugin, with chief_extension: PUT /plugin/v1/chief/instructions
//     {markdown} stores its one section (pending), GET reads it back with its
//     sha, approved_sha and approved_at.
//   - The owner, on /plugins: the plugin's card shows the pending text AS TEXT
//     (escaped, never HTML) and an Approve that names the sha the page showed.
//     It needs a fresh sign-in (step-up), as approving a pairing does: the
//     text becomes standing instructions to the owner's chief. A sha that is
//     no longer the stored one is refused (409) — the plugin changed it after
//     the page was drawn.
//   - Revoking the plugin, or turning chief_extension off, deletes it.
//
// What renders, and where, is internal/agentprovision's and
// internal/agentspec's (PluginSectionsBlock): only an approved section of a
// live plugin, after every core rule, at the chief's next provision or
// restart. The chief's tools and allow list do not change.

const (
	pluginChiefInstructionsPath = pluginAPIPrefix + "/chief/instructions"
	// pluginChiefInstructionsBodyMax bounds the PUT's JSON: the 4 KiB section
	// with every byte escaped as \uXXXX (6×), and room for the key.
	pluginChiefInstructionsBodyMax = 6*plugins.MaxChiefInstructionsBytes + 1024
)

// chiefInstructionsStore is the plugin store's chief-instructions half
// (plugins.PGStore). A store without it (a test fake) serves none of it:
// the routes answer 404 and /plugins draws nothing for it.
type chiefInstructionsStore interface {
	PutChiefInstructions(ctx context.Context, pluginID int64, text string) (plugins.ChiefInstructions, bool, error)
	ChiefInstructionsOf(ctx context.Context, pluginID int64) (plugins.ChiefInstructions, bool, error)
	ApproveChiefInstructions(ctx context.Context, pluginID int64, sha string) (plugins.ChiefInstructions, error)
	SetChiefExtension(ctx context.Context, id int64, on bool) (plugins.Plugin, bool, error)
}

func (s *Server) chiefInstructions() chiefInstructionsStore {
	cs, _ := s.ext.Plugins.(chiefInstructionsStore)
	return cs
}

func (s *Server) registerPluginChiefInstructionRoutes(mux Mux) {
	mux.HandleFunc("PUT "+pluginChiefInstructionsPath, s.requirePluginToken(s.handlePluginChiefInstructionsPut, plugins.CapChiefExtension))
	mux.HandleFunc("GET "+pluginChiefInstructionsPath, s.requirePluginToken(s.handlePluginChiefInstructionsGet, plugins.CapChiefExtension))
	// The owner's card: chief_extension on or off (draft-checks' gate), and
	// the approval of one exact text (a fresh sign-in, checked in the handler).
	mux.HandleFunc("POST "+pluginsPath+"/{id}/chief-extension", s.requirePluginsOn(s.requireOperatorWrite(s.handlePluginChiefExtensionToggle)))
	mux.HandleFunc("POST "+pluginsPath+"/{id}/approve-chief-instructions", s.requirePluginsOn(s.requireOperatorWrite(s.handleChiefInstructionsApprove)))
}

// pluginChiefInstructionsView is the plugin door's answer: its own section.
type pluginChiefInstructionsView struct {
	plugins.ChiefInstructions
	// State is "approved" when the owner approved THIS text, else "pending".
	State string `json:"state"`
	// Changed (PUT only) is whether this PUT stored new text.
	Changed *bool `json:"changed,omitempty"`
}

func chiefInstructionsView(c plugins.ChiefInstructions) pluginChiefInstructionsView {
	v := pluginChiefInstructionsView{ChiefInstructions: c, State: "pending"}
	if c.Approved() {
		v.State = "approved"
	}
	return v
}

// handlePluginChiefInstructionsPut is PUT /plugin/v1/chief/instructions
// {markdown}: store the plugin's one section, replacing any earlier one. New
// text is pending until its owner approves it on /plugins; the same text
// again keeps its state. 400 for text over plugins.MaxChiefInstructionsBytes,
// blank, or carrying a control character.
func (s *Server) handlePluginChiefInstructionsPut(w http.ResponseWriter, r *http.Request) {
	cs := s.chiefInstructions()
	if cs == nil {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Markdown *string `json:"markdown"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, pluginChiefInstructionsBodyMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Markdown == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is {\"markdown\": \"<at most " +
			strconv.Itoa(plugins.MaxChiefInstructionsBytes) + " bytes>\"}"})
		return
	}
	p := pluginFromContext(r.Context())
	c, changed, err := cs.PutChiefInstructions(r.Context(), p.ID, *in.Markdown)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")})
		return
	case errors.Is(err, plugins.ErrNotFound):
		// Revoked, or chief_extension turned off, after the door admitted it.
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": PluginCapabilityRefusal(plugins.CapChiefExtension)})
		return
	default:
		s.logger.Printf("plugins: plugin %d chief instructions: %v", p.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the instructions could not be stored"})
		return
	}
	v := chiefInstructionsView(c)
	v.Changed = &changed
	s.writeJSON(w, http.StatusOK, v)
}

// handlePluginChiefInstructionsGet is GET /plugin/v1/chief/instructions: the
// plugin's own section, its sha, approved_sha and approved_at (404 for none).
func (s *Server) handlePluginChiefInstructionsGet(w http.ResponseWriter, r *http.Request) {
	cs := s.chiefInstructions()
	if cs == nil {
		http.NotFound(w, r)
		return
	}
	p := pluginFromContext(r.Context())
	c, ok, err := cs.ChiefInstructionsOf(r.Context(), p.ID)
	switch {
	case err != nil:
		s.logger.Printf("plugins: plugin %d chief instructions read: %v", p.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the instructions could not be read"})
	case !ok:
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "this plugin has stored no chief instructions"})
	default:
		s.writeJSON(w, http.StatusOK, chiefInstructionsView(c))
	}
}

// handlePluginChiefExtensionToggle is POST /plugins/{id}/chief-extension
// {on: "1" turns it on, anything else off}: Chief instructions on or off for
// one of the owner's LIVE plugins, keeping its token (plugins.PGStore
// SetChiefExtension). Gated as draft-checks is — a signed-in session and a
// same-origin form, no fresh sign-in: turning it on lets the plugin PROPOSE
// text, and nothing reaches the chief until the owner approves it (which
// does need one). Off deletes the plugin's section. 404 as revoke's.
func (s *Server) handlePluginChiefExtensionToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	cs := s.chiefInstructions()
	if err != nil || id <= 0 || cs == nil {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	}
	on := r.PostFormValue("on") == "1"
	_, changed, err := cs.SetChiefExtension(r.Context(), id, on)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	case errors.Is(err, plugins.ErrInvalid):
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "))
		return
	default:
		s.logger.Printf("plugins: chief_extension %v for plugin %d: %v", on, id, err)
		s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "Chief instructions could not be changed.")
		return
	}
	if changed {
		s.logger.Printf("plugins: plugin %d: chief_extension turned %v by its owner", id, on)
	}
	http.Redirect(w, r, pluginsPath, http.StatusSeeOther)
}

// ChiefInstructionsStepUpWhy is the confirm dialog's sentence for approving a
// plugin's chief instructions on /plugins.
func ChiefInstructionsStepUpWhy() string {
	return "Approving a plugin's chief instructions makes them standing guidance to your chief, so it needs a sign-in from the last " +
		stepup.WindowText() + "."
}

// ChiefInstructionsStaleRefusal answers an approval of text the plugin has
// since replaced.
const ChiefInstructionsStaleRefusal = "The plugin changed its chief instructions after this page was drawn, so nothing was approved. Read the new text, then approve it."

// handleChiefInstructionsApprove is POST /plugins/{id}/approve-chief-instructions
// {sha}: the owner approves the plugin's section whose sha256 the card showed.
// A fresh sign-in, as a pairing approval (decidePairForm). 🔴 THE sha IS THE
// TEXT THE OWNER READ: one that is no longer stored is 409 and approves
// nothing (plugins.ErrStaleApproval).
func (s *Server) handleChiefInstructionsApprove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	cs := s.chiefInstructions()
	if err != nil || id <= 0 || cs == nil {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	}
	if !stepup.Fresh(r.Context(), s.stepUpNow()) {
		if isDocumentNavigation(r) {
			w.Header().Set("X-Muster-Step-Up", "required")
			s.renderPluginsPage(w, r, http.StatusForbidden, "", "Approving a plugin's chief instructions needs a recent sign-in. Confirm it's you, then approve again.")
			return
		}
		refuseStale(w)
		return
	}
	_, err = cs.ApproveChiefInstructions(r.Context(), id, strings.TrimSpace(r.PostFormValue("sha")))
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	case errors.Is(err, plugins.ErrStaleApproval):
		s.renderPluginsPage(w, r, http.StatusConflict, "", ChiefInstructionsStaleRefusal)
		return
	default:
		s.logger.Printf("plugins: plugin %d chief instructions approve: %v", id, err)
		s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "The approval could not be recorded.")
		return
	}
	http.Redirect(w, r, pluginsPath, http.StatusSeeOther)
}

// pluginChiefExtensionToggleHTML is a live plugin's Chief instructions
// control on its /plugins card: one form posting the other state.
//
// A plugin whose ONLY capability is chief_extension draws no Turn off: a
// token needs one capability (the mint's rule), so it is revoked instead.
func pluginChiefExtensionToggleHTML(p plugins.Plugin) string {
	if len(p.Capabilities) == 1 && p.Has(plugins.CapChiefExtension) {
		return ""
	}
	on, label := "1", "Turn on Chief instructions"
	if p.Has(plugins.CapChiefExtension) {
		on, label = "0", "Turn off Chief instructions"
	}
	return `<form method="POST" action="` + pluginsPath + `/` + strconv.FormatInt(p.ID, 10) + `/chief-extension" data-plugin-chief-extension>
<input type="hidden" name="on" value="` + on + `"><button type="submit" class="` + flowsQuietButton + `">` + label + `</button></form>`
}

// pluginChiefInstructionsHTML is a live plugin's stored section on its
// /plugins card, and whether it draws an Approve (a step-up form). 🔴 THE
// TEXT IS DRAWN AS TEXT: escaped inside a <pre>, never rendered as markdown or
// HTML — the owner approves the exact bytes the chief will read.
func (s *Server) pluginChiefInstructionsHTML(r *http.Request, p plugins.Plugin) (string, bool) {
	cs := s.chiefInstructions()
	if cs == nil || !p.Has(plugins.CapChiefExtension) {
		return "", false
	}
	c, ok, err := cs.ChiefInstructionsOf(r.Context(), p.ID)
	if err != nil {
		s.logger.Printf("plugins: page chief instructions %d: %v", p.ID, err)
		return problemNode("This plugin's chief instructions could not be read."), false
	}
	if !ok {
		return `<span class="text-xs text-fg2" data-chief-instructions="none">no chief instructions sent yet</span>`, false
	}
	esc := html.EscapeString
	state, when := "pending your approval", ""
	if c.Approved() {
		state, when = "approved", " on "+c.ApprovedAt.UTC().Format("2006-01-02")
	}
	out := `<div class="flex flex-col gap-1" data-chief-instructions="` + map[bool]string{true: "approved", false: "pending"}[c.Approved()] + `" data-chief-instructions-sha="` + esc(c.SHA) + `">
<span class="text-xs text-fg2">Chief instructions: ` + state + when + ` · sha256 <span class="font-mono">` + esc(c.SHA[:12]) + `</span> · your chief reads approved text from its next start</span>
<pre class="whitespace-pre-wrap break-words text-xs text-fg2" data-chief-instructions-text>` + esc(c.Text) + `</pre>
`
	gated := false
	if !c.Approved() {
		gated = true
		out += `<form method="POST" action="` + pluginsPath + `/` + strconv.FormatInt(p.ID, 10) + `/approve-chief-instructions" data-step-up-form>` +
			`<input type="hidden" name="sha" value="` + esc(c.SHA) + `"><button type="submit" class="` + flowsButtonClass + `" data-chief-instructions-approve>Approve these instructions</button></form>
`
	}
	return out + "</div>\n", gated
}

// pluginChiefExtensionFieldHTML is the create form's chief_extension
// checkbox, drawn only when the store serves chief instructions.
func (s *Server) pluginChiefExtensionFieldHTML() string {
	if s.chiefInstructions() == nil {
		return ""
	}
	return pluginChiefExtensionField
}

// pluginChiefExtensionField is the create form's chief_extension choice
// (plugins.CapChiefExtension). It can change after the mint, on the plugin's
// card (handlePluginChiefExtensionToggle). Unticked, the token is as before.
const pluginChiefExtensionField = `<label class="flex min-h-[44px] items-center gap-2 text-sm text-fg" data-testid="plugin-chief-extension"><input type="checkbox" name="chief_extension" value="1"> Chief instructions: the plugin may propose one section of instructions for your chief, which reaches it only once you approve the exact text <span class="text-xs text-fg2">you can change it later on the plugin's card</span></label>
`
