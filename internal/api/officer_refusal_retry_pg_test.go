package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// A CADENCE REFUSAL'S retry_at (plugin_drafts.go parseRetryAt, draftEndFor,
// the officer page's Re-file): through the real plugin routes, the real
// stores and the officer's page. Present, it is stored on the draft.refused
// item and is Re-file's default; absent, Re-file is what it was; malformed,
// it is ignored and the refusal is still recorded.

// refusalArticle is refusal item's card on the officer page ("" for none).
func refusalArticle(page string, item int64) string {
	i := strings.Index(page, `id="refusal-`+strconv.FormatInt(item, 10)+`"`)
	if i < 0 {
		return ""
	}
	rest := page[i:]
	if j := strings.Index(rest, "</article>"); j > 0 {
		rest = rest[:j]
	}
	return rest
}

// checkRaw is the plugin's check of draft id with body exactly raw.
func (w *outboundWorld) checkRaw(id int64, raw string) *httptest.ResponseRecorder {
	return w.do("POST", "/plugin/v1/drafts/"+strconv.FormatInt(id, 10)+"/check", raw, bearerTok(w.tok))
}

// fileX files a post on X: its draft id.
func (w *outboundWorld) fileX(body string) int64 {
	w.t.Helper()
	id, out := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"x","target":{"kind":"post"},"body":"` + body + `"}`)
	if id == 0 {
		w.t.Fatalf("file: %s", out)
	}
	return id
}

// TestACadenceRefusalsRetryAtIsTheRefileDefault: the plugin refuses a post
// for cadence with retry_at in a -05:00 offset at :25 seconds past a minute.
// The draft.refused item stores it in UTC; the officer page's Re-file
// selects "When the plugin says it can go" and fills the time in, rounded UP
// to the next minute (never earlier than the plugin said); submitting that
// default re-files it at exactly that minute. A failed SEND's refusal
// (result ok:false) carries it the same way.
func TestACadenceRefusalsRetryAtIsTheRefileDefault(t *testing.T) {
	w := newOutboundWorld(t, true)
	retry := time.Now().UTC().Add(3*time.Hour + 17*time.Minute).Truncate(time.Minute).Add(25 * time.Second)
	wantStored := retry.Format(time.RFC3339)
	wantField := retry.Truncate(time.Minute).Add(time.Minute).Format("2006-01-02T15:04")
	sent := retry.In(time.FixedZone("", -5*3600)).Format(time.RFC3339)

	id := w.fileX("A cadence post.")
	if rec := w.checkRaw(id, `{"sendable":false,"detail":"cadence: the X slot is used today","retry_at":"`+sent+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("the check refusal with retry_at: %d %s", rec.Code, rec.Body.String())
	}
	item, payload, ok := w.refusedItemOf(id)
	if !ok || payload["retry_at"] != wantStored {
		t.Fatalf("the draft.refused item's retry_at: %v (found %v), want %q", payload["retry_at"], ok, wantStored)
	}
	card := refusalArticle(needsYou(w.officerPage()), item)
	if !strings.Contains(card, `<option value="at" selected data-refile-retry>`) || !strings.Contains(card, `name="at" type="datetime-local" value="`+wantField+`"`) {
		t.Fatalf("Re-file does not default to retry_at (%s):\n%s", wantField, card)
	}
	if rec := w.refusalPost(item, "refile", url.Values{"when": {"at"}, "at": {wantField}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("re-file at the default: %d %.300s", rec.Code, rec.Body.String())
	}
	if it := w.itemFull(item); it.notBefore == nil || it.notBefore.UTC().Format("2006-01-02T15:04") != wantField || it.status != officers.StatusOpen {
		t.Fatalf("re-filed at the default: %+v, want not_before %s", it, wantField)
	}

	sending := w.claimedDraft("x:retry-send", "A post the plugin could not send.")
	rec := w.result(sending, `{"ok":false,"detail":"cadence: the X slot is used today","retry_at":"`+sent+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the failed send's result: %d %s", rec.Code, rec.Body.String())
	}
	if _, p, ok := w.refusedItemOf(sending); !ok || p["retry_at"] != wantStored {
		t.Fatalf("the failed send's draft.refused retry_at: %v (found %v), want %q", p["retry_at"], ok, wantStored)
	}
}

// TestARefusalWithoutOrWithABadRetryAtKeepsTheOldRefile: no retry_at, one
// that is not RFC 3339, one that is a number, and an RFC 3339 time already
// past: each refusal is recorded (200, a draft.refused item); the malformed
// ones store nothing; and none changes Re-file — no retry choice, "In an
// hour" first, the time field empty.
func TestARefusalWithoutOrWithABadRetryAtKeepsTheOldRefile(t *testing.T) {
	w := newOutboundWorld(t, true)
	past := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	for _, c := range []struct {
		name, extra string
		stored      any
	}{
		{"absent", ``, nil},
		{"not RFC 3339", `,"retry_at":"tomorrow morning"`, nil},
		{"a number", `,"retry_at":1767225600`, nil},
		{"null", `,"retry_at":null`, nil},
		{"already past", `,"retry_at":"` + past + `"`, past},
	} {
		id := w.fileX("A post, " + strings.ReplaceAll(c.name, " ", "-") + ".")
		rec := w.checkRaw(id, `{"sendable":false,"detail":"cadence: later"`+c.extra+`}`)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: the refusal answered %d %s; want it recorded", c.name, rec.Code, rec.Body.String())
			continue
		}
		item, payload, ok := w.refusedItemOf(id)
		if !ok {
			t.Errorf("%s: no draft.refused item", c.name)
			continue
		}
		if got := payload["retry_at"]; got != c.stored {
			raw, _ := json.Marshal(payload)
			t.Errorf("%s: payload retry_at %v, want %v (%s)", c.name, got, c.stored, raw)
		}
		card := refusalArticle(needsYou(w.officerPage()), item)
		if card == "" || strings.Contains(card, "data-refile-retry") || strings.Contains(card, " selected") ||
			!strings.Contains(card, `name="when" class="`) || !strings.Contains(card, `name="at" type="datetime-local" value=""`) {
			t.Errorf("%s: Re-file changed:\n%s", c.name, card)
		}
		if i, j := strings.Index(card, `<option value="1h">`), strings.Index(card, `<option value="6h">`); i < 0 || j < i {
			t.Errorf("%s: \"In an hour\" is not the first choice", c.name)
		}
	}
}
