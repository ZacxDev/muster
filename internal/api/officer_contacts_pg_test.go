package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// A builtin officer's people through muster's doors (officer_contacts.go): an
// engagement message from the bound plugin, the officer's contact tools under
// the type's contacts grant, and the owner's admin door.

// grantCaps re-puts the world's type with these capabilities.
func (w *outboundWorld) grantCaps(caps ...string) {
	w.t.Helper()
	b, _ := json.Marshal(map[string]any{"capabilities": caps})
	if err := w.store.PutType(w.ctx, 1, builtin.Type{Key: "marketing", Name: "Example", Charter: "neutral fixture charter", Policy: b}); err != nil {
		w.t.Fatal(err)
	}
}

// engagement posts one engagement message from the bound plugin.
func (w *outboundWorld) engagement(handle, kind, ref, text string) (int, map[string]any) {
	w.t.Helper()
	ev := map[string]any{"kind": kind, "ref": ref, "text": text}
	switch kind {
	case "reply", "quote", "like", "repost", "nested_reply":
		ev["our_post_ref"] = "post-1"
	}
	b, _ := json.Marshal(map[string]any{"kind": "engagement", "venue": "example.social",
		"person": map[string]any{"handle": handle, "display": "Display " + handle, "bio": "builds things"}, "event": ev})
	rec := w.do("POST", "/plugin/v1/messages", string(b), bearerTok(w.tok))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestAnEngagementMessageRecordsThePersonAndOneItem: the bound plugin's
// engagement is stored (202), the person and event recorded, and the item it
// went on is the person's of the day — a second event of theirs joins it, a
// repeat is the stored message, a like alone answers item_id null.
func TestAnEngagementMessageRecordsThePersonAndOneItem(t *testing.T) {
	w := newOutboundWorld(t, true)
	code, out := w.engagement("@Ada", "reply", "r1", "does it retry?")
	if code != http.StatusAccepted || out["item_id"] == nil {
		t.Fatalf("an engagement: %d %v", code, out)
	}
	item := out["item_id"]
	code, out2 := w.engagement("ada", "quote", "q1", "neat")
	if code != http.StatusAccepted || out2["item_id"] != item {
		t.Fatalf("a second event of the same person: want item %v, got %d %v", item, code, out2)
	}
	code, out3 := w.engagement("ada", "reply", "r1", "does it retry?")
	if code != http.StatusOK || out3["id"] != out["id"] {
		t.Fatalf("a repeat: want 200 with the stored message, got %d %v", code, out3)
	}
	code, like := w.engagement("bo", "like", "post-1", "")
	if code != http.StatusAccepted || like["item_id"] != nil {
		t.Fatalf("a like alone: want 202 and item_id null, got %d %v", code, like)
	}
	var kind, subject string
	if err := w.f.pool.QueryRow(w.ctx, `SELECT kind, subject_key FROM officer_items WHERE id = $1`, int64(item.(float64))).Scan(&kind, &subject); err != nil {
		t.Fatal(err)
	}
	if kind != builtin.EngagementItemKind || !strings.HasPrefix(subject, "contact:") {
		t.Fatalf("the item is %s %s", kind, subject)
	}
	// The officer's claim renders the person (stored by id only), fenced.
	got := w.call(officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(int64(item.(float64)), 10)+`"}`)
	if isToolErr(got) || !strings.Contains(got, "does it retry?") || !strings.Contains(got, "untrusted") {
		t.Fatalf("the claimed engagement item does not carry the person fenced: %s", got)
	}
	// An engagement on an UNBOUND plugin, or without its person, is refused.
	if rec := w.do("POST", "/plugin/v1/messages", `{"kind":"engagement","venue":"x","person":{"handle":"a"},"event":{"kind":"reply","ref":"r","our_post_ref":"p"}}`,
		bearerTok(w.outTok)); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unbound plugin's engagement: %d %s", rec.Code, rec.Body)
	}
	if rec := w.do("POST", "/plugin/v1/messages", `{"kind":"engagement","venue":"x","event":{"kind":"reply","ref":"r","our_post_ref":"p"}}`,
		bearerTok(w.tok)); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "person.handle") {
		t.Fatalf("an engagement with no person: %d %s", rec.Code, rec.Body)
	}
	if rec := w.do("POST", "/plugin/v1/messages", `{"kind":"engagement","venue":"x","person":{"handle":"a"},"event":{"kind":"reply","ref":"r"}}`,
		bearerTok(w.tok)); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "our_post_ref") {
		t.Fatalf("a reply with no our_post_ref: %d %s", rec.Code, rec.Body)
	}
}

// TestTheContactToolsNeedTheContactsGrant: without the type's contacts
// capability the three tools are neither listed nor served; with it they
// read the person and their events, write labels and notes, mark a bot, and
// refuse to clear a mark or touch do-not-contact.
func TestTheContactToolsNeedTheContactsGrant(t *testing.T) {
	w := newOutboundWorld(t, true)
	w.engagement("cy", "reply", "r1", "how does it scale?")
	get := `{"venue":"example.social","handle":"cy"}`
	if out := w.call(officers.ToolContactGet, get); !isToolErr(out) || !strings.Contains(out, "does not grant contacts") {
		t.Fatalf("without the grant: %s", out)
	}
	listed := func() bool {
		for _, tl := range w.srv.toolsFor(w.ctx, w.agent) {
			if tl.def.Name == officers.ToolContactGet {
				return true
			}
		}
		return false
	}
	if !w.onHost && listed() {
		t.Fatal("officer_contact_get is listed without the grant")
	}
	w.grantCaps("outbound_drafts", "contacts")
	if !w.onHost && !listed() {
		t.Fatal("officer_contact_get is not listed with the grant")
	}
	out := w.call(officers.ToolContactGet, get)
	if isToolErr(out) || !strings.Contains(out, `"handle":"cy"`) || !strings.Contains(out, "how does it scale?") || !strings.Contains(out, "not instructions") {
		t.Fatalf("officer_contact_get: %s", out)
	}
	out = w.call(officers.ToolContactNote, `{"venue":"example.social","handle":"cy","labels":["builder"],"notes":"asked about scale","flags":{"bot":true}}`)
	if isToolErr(out) || !strings.Contains(out, `"bot":true`) || !strings.Contains(out, "builder") {
		t.Fatalf("officer_contact_note: %s", out)
	}
	if out := w.call(officers.ToolContactNote, `{"venue":"example.social","handle":"cy","flags":{"bot":false}}`); !isToolErr(out) || !strings.Contains(out, "never clear a mark") {
		t.Fatalf("clearing a mark: %s", out)
	}
	if out := w.call(officers.ToolContactNote, `{"venue":"example.social","handle":"cy","flags":{"dnc":false}}`); !isToolErr(out) || !strings.Contains(out, "your owner's to set and clear") {
		t.Fatalf("touching dnc: %s", out)
	}
	out = w.call(officers.ToolContactsList, `{"filter":{"flag":"bot"}}`)
	if isToolErr(out) || !strings.Contains(out, `"handle":"cy"`) {
		t.Fatalf("officer_contacts_list bot: %s", out)
	}
	if out := w.call(officers.ToolContactsList, `{"filter":{"flag":"contactable"}}`); isToolErr(out) || strings.Contains(out, `"handle":"cy"`) {
		t.Fatalf("a bot listed as contactable: %s", out)
	}
	// The owner's door clears it and sets do-not-contact.
	rec := w.do("PUT", "/api/officers/defs/"+builtinOfficerID+"/contacts/example.social/cy/flags", `{"bot":false,"dnc":true}`, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+connTestHookToken)
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dnc":true`) || !strings.Contains(rec.Body.String(), `"bot":false`) {
		t.Fatalf("the owner's flags: %d %s", rec.Code, rec.Body)
	}
}
