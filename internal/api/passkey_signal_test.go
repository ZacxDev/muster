package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"golang.org/x/net/html"
)

// elementsByID is every element in page whose id is id.
func elementsByID(t *testing.T, page, id string) []*html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	return nodesWith(doc, "id", id)
}

// attrVal is n's value of key, "" when it has none.
func attrVal(n *html.Node, key string) string {
	v, _ := nodeAttr(n, key)
	return v
}

// pageSignal is the Signal API hand-off on page: present, and its data. The
// ids are nil when the page withholds the set (JSON null).
type pageSignalData struct {
	RPID   string    `json:"rpId"`
	UserID string    `json:"userId"`
	IDs    *[]string `json:"allAcceptedCredentialIds"`
	Name   string    `json:"name"`
}

func pageSignal(t *testing.T, page string) (pageSignalData, bool) {
	t.Helper()
	els := elementsByID(t, page, "passkey-signal")
	switch len(els) {
	case 0:
		return pageSignalData{}, false
	case 1:
	default:
		t.Fatalf("%d #passkey-signal elements", len(els))
	}
	if _, hidden := nodeAttr(els[0], "hidden"); !hidden {
		t.Fatal("#passkey-signal is not hidden")
	}
	var d pageSignalData
	if err := json.Unmarshal([]byte(attrVal(els[0], "data-signal")), &d); err != nil {
		t.Fatalf("data-signal is not JSON: %q", attrVal(els[0], "data-signal"))
	}
	if !strings.Contains(page, passkeySignalJS) {
		t.Fatal("the signal element is drawn without its script")
	}
	return d, true
}

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func sorted(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// recordedCredIDs is the cred_id recorded on each of sub's passkeys, by
// GoTrue passkey id ("" when none is).
func (w *invWorld) recordedCredIDs(sub string) map[string]string {
	gu, _ := w.fake.User(sub)
	rec, _ := gu.AppMetadata[passkeyMetaKey].(map[string]any)
	out := map[string]string{}
	for _, p := range w.fake.Passkeys(sub) {
		m, _ := rec[p.ID].(map[string]any)
		id, _ := m[passkeyCredIDKey].(string)
		out[p.ID] = id
	}
	return out
}

// TestTheSignInPageOffersAutofillAndThePhoneButton: with passkeys on, the
// email field asks for passkey autofill (autocomplete "username webauthn"),
// and a second button offers a phone's passkey; with them off, neither.
func TestTheSignInPageOffersAutofillAndThePhoneButton(t *testing.T) {
	on := newPasskeyWorld(t)
	page := do(on.edge, "GET", loginPath, nil).Body.String()
	email := elementsByID(t, page, "muster-email")
	if len(email) != 1 || attrVal(email[0], "autocomplete") != "username webauthn" {
		t.Fatalf("the email field with passkeys on: %v", email)
	}
	phone := elementsByID(t, page, "passkey-signin-phone")
	if len(phone) != 1 || phone[0].Data != "button" || attrVal(phone[0], "type") != "button" || nodeText(phone[0]) != "Use a passkey from my phone" {
		t.Fatalf("the phone button: %v", phone)
	}
	if b := elementsByID(t, page, "passkey-signin"); len(b) != 1 || nodeText(b[0]) != "Sign in with a passkey" {
		t.Fatalf("the passkey button: %v", b)
	}
	off := newInviteWorld(t)
	page = do(off.edge, "GET", loginPath, nil).Body.String()
	email = elementsByID(t, page, "muster-email")
	if len(email) != 1 || attrVal(email[0], "autocomplete") != "username" {
		t.Fatalf("the email field with passkeys off: %v", email)
	}
	if len(elementsByID(t, page, "passkey-signin-phone")) != 0 || len(elementsByID(t, page, "passkey-signin")) != 0 {
		t.Fatal("passkey buttons with passkeys off")
	}
}

// TestTheSignInPageWordsANoPasskeyOutcomeNeutrally: a ceremony that ended
// with no passkey no longer says "cancelled" (nobody may have cancelled
// anything), and the copy is pinned whole.
func TestTheSignInPageWordsANoPasskeyOutcomeNeutrally(t *testing.T) {
	const notUsed = "No passkey was used. You can try again, use a passkey from your phone, or sign in with your password. " +
		"If a password manager opened instead, choose its option to use your browser, a phone or a security key."
	const failedHere = "This device could not use a passkey. Try again, or sign in with your password."
	if PasskeyNotUsedNote != notUsed || PasskeyDeviceFailedNote != failedHere {
		t.Fatalf("the copy:\n%q\n%q", PasskeyNotUsedNote, PasskeyDeviceFailedNote)
	}
	w := newPasskeyWorld(t)
	page := do(w.edge, "GET", loginPath, nil).Body.String()
	for _, want := range []string{"notUsed = " + jsStr(notUsed), "failed = " + jsStr(failedHere)} {
		if !strings.Contains(page, want) {
			t.Fatalf("the sign-in script does not carry %s", want)
		}
	}
	if strings.Contains(page, "Passkey sign-in was cancelled.") {
		t.Fatal("the sign-in page still says the sign-in was cancelled")
	}
}

// TestTheAccountPageAlwaysLinksPasskeysInGoTrueMode: the Passkeys link is on
// /account with passkeys ON, OFF and with GoTrue's /settings FAILING — it was
// hidden in the last two, leaving no way to the page that says why.
func TestTheAccountPageAlwaysLinksPasskeysInGoTrueMode(t *testing.T) {
	// One world at a time: each resets the shared database (and with it the
	// admin the previous one signed in).
	for _, name := range []string{"on", "off", "settings failing"} {
		var w *invWorld
		switch name {
		case "on":
			w = newPasskeyWorld(t)
		case "off":
			w = newInviteWorld(t)
		default:
			w = newPasskeyWorld(t)
			w.s.gt.pk.mu.Lock()
			w.s.gt.pk.at = time.Time{} // forget the cached passkeys_enabled
			w.s.gt.pk.mu.Unlock()
			w.fake.FailSettings(1000)
		}
		page := do(w.edge, "GET", "/account", nil, w.asAdmin())
		l := elementsByID(t, page.Body.String(), "account-passkeys")
		if page.Code != 200 || len(l) != 1 || l[0].Data != "a" || attrVal(l[0], "href") != accountPasskeysPath || nodeText(l[0]) != "Passkeys" {
			t.Fatalf("passkeys %s: %d, the link %v", name, page.Code, l)
		}
		// CONTROL: muster believes passkeys are off where they are, or failing.
		if served := w.s.passkeysServed(context.Background()); served != (name == "on") {
			t.Fatalf("CONTROL: passkeys %s, but passkeysServed = %v", name, served)
		}
	}
}

// TestARegistrationRecordsTheCredentialID: adding a passkey records its
// WebAuthn credential id (base64url of the raw id) beside its backup flags —
// GoTrue's list never returns it, and the Signal API needs it.
func TestARegistrationRecordsTheCredentialID(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, at, rt := w.joinMember("pk-credid@invite.test")
	rec := w.addPasskey("raw-id-α", gotruetest.FlagUPUVBEBS, withCookies(at, rt))
	if rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	id, _ := jsonBody(t, rec)["id"].(string)
	gu, _ := w.fake.User(sub)
	all, _ := gu.AppMetadata[passkeyMetaKey].(map[string]any)
	got, _ := all[id].(map[string]any)
	want := map[string]any{"synced": true, "can_sync": true, passkeyCredIDKey: b64url("raw-id-α")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the record: %v, want %v", got, want)
	}
}

// TestThePasskeysPageSignalsTheCompleteSet: /account/passkeys hands the
// Signal API the account's RP ID, its user handle (base64url of the GoTrue
// subject's text, as GoTrue registers it), every accepted credential id and
// its name — and after a removal, which redirects there, the set without the
// removed one.
func TestThePasskeysPageSignalsTheCompleteSet(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, at, rt := w.joinMember("pk-signal@invite.test")
	cookies := withCookies(at, rt)
	var ids []string
	for _, c := range []string{"phone-key", "laptop-key"} {
		rec := w.addPasskey(c, gotruetest.FlagUPUV, cookies)
		if rec.Code != 200 {
			t.Fatalf("add %s: %d", c, rec.Code)
		}
		id, _ := jsonBody(t, rec)["id"].(string)
		ids = append(ids, id)
	}
	d, ok := pageSignal(t, do(w.edge, "GET", accountPasskeysPath, nil, cookies).Body.String())
	if !ok || d.RPID != "localhost" || d.UserID != b64url(sub) || d.Name != "pk-signal@invite.test" || d.IDs == nil ||
		!reflect.DeepEqual(sorted(*d.IDs), sorted([]string{b64url("phone-key"), b64url("laptop-key")})) {
		t.Fatalf("the signal: %+v (ok %v)", d, ok)
	}
	if d.UserID == b64url("pk-signal@invite.test") || d.UserID == sub {
		t.Fatal("the user handle is not the base64url of the subject")
	}
	r := do(w.edge, "POST", accountPasskeysPath+"/"+ids[0]+"/delete", url.Values{}, cookies)
	if r.Code != http.StatusSeeOther || r.Header().Get("Location") != accountPasskeysPath {
		t.Fatalf("remove: %d %q", r.Code, r.Header().Get("Location"))
	}
	d, ok = pageSignal(t, do(w.edge, "GET", accountPasskeysPath, nil, cookies).Body.String())
	if !ok || d.IDs == nil || !reflect.DeepEqual(*d.IDs, []string{b64url("laptop-key")}) {
		t.Fatalf("the signal after removing one: %+v", d)
	}
}

// TestTheSignalIsWithheldWhileAnyCredentialIdIsUnknown: with one passkey whose
// id muster never recorded (registered before it recorded ids), the page
// sends NO accepted set — a partial one would tell the password manager that
// passkey is gone — but still sends the user details. The control: the same
// account with only the recorded one sends the set.
func TestTheSignalIsWithheldWhileAnyCredentialIdIsUnknown(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, at, rt := w.joinMember("pk-partial@invite.test")
	cookies := withCookies(at, rt)
	if rec := w.addPasskey("recorded", gotruetest.FlagUPUV, cookies); rec.Code != 200 {
		t.Fatalf("add: %d", rec.Code)
	}
	d, ok := pageSignal(t, do(w.edge, "GET", accountPasskeysPath, nil, cookies).Body.String())
	if !ok || d.IDs == nil || !reflect.DeepEqual(*d.IDs, []string{b64url("recorded")}) {
		t.Fatalf("CONTROL: the complete set is sent: %+v", d)
	}
	w.fake.AddPasskey(sub, "unrecorded")
	d, ok = pageSignal(t, do(w.edge, "GET", accountPasskeysPath, nil, cookies).Body.String())
	if !ok || d.IDs != nil || d.UserID != b64url(sub) || d.Name != "pk-partial@invite.test" || d.RPID != "localhost" {
		t.Fatalf("with an unrecorded passkey: %+v (ok %v)", d, ok)
	}
}

// TestAnUnknownCredentialIsFlaggedOnlyWhenProvablyUnknown: GoTrue refuses
// every failed passkey sign-in alike; muster says credential_unknown — the
// page's cue for signalUnknownCredential — only when the user handle names no
// user, a user with no passkeys, or one whose every passkey has a recorded id
// and none is this one. A user with an unrecorded passkey, or a handle that is
// not a GoTrue user id, is a plain refusal.
func TestAnUnknownCredentialIsFlaggedOnlyWhenProvablyUnknown(t *testing.T) {
	w := newPasskeyWorld(t)
	_, emptySub, _, _ := w.joinMember("pk-unk-empty@invite.test")
	_, recSub, at, rt := w.joinMember("pk-unk-rec@invite.test")
	if rec := w.addPasskey("rec-1", gotruetest.FlagUPUV, withCookies(at, rt)); rec.Code != 200 {
		t.Fatalf("add: %d", rec.Code)
	}
	_, unrecSub, _, _ := w.joinMember("pk-unk-unrec@invite.test")
	w.fake.AddPasskey(unrecSub, "unrec-1")
	signIn := func(credID, handle string) *http.Response {
		o := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{})
		ch, _ := jsonBody(t, o)["challenge_id"].(string)
		return postJSON(w.edge, passkeySignInVerifyPath, map[string]any{
			"challenge_id": ch, "credential": gotruetest.AssertionFor(credID, handle, gotruetest.FlagUPUV)}).Result()
	}
	for _, c := range []struct {
		name, cred, handle string
		unknown            bool
	}{
		{"a user with no passkeys", "gone-1", emptySub, true},
		{"no such user", "gone-2", "0f0f0f0f-0f0f-4f0f-af0f-0f0f0f0f0f0f", true},
		{"every passkey recorded, none this one", "gone-3", recSub, true},
		{"an unrecorded passkey", "gone-4", unrecSub, false},
		{"a handle that is no GoTrue user id", "gone-5", "user-handle", false},
	} {
		res := signIn(c.cred, c.handle)
		var body map[string]any
		_ = json.NewDecoder(res.Body).Decode(&body)
		wantErr := PasskeyRejectedRefusal
		if c.unknown {
			wantErr = PasskeyUnknownRefusal
		}
		_, flagged := body["credential_unknown"]
		if res.StatusCode != http.StatusUnauthorized || body["error"] != wantErr || flagged != c.unknown ||
			(c.unknown && body["credential_unknown"] != true) || len(res.Cookies()) != 0 {
			t.Errorf("%s: %d %v", c.name, res.StatusCode, body)
		}
	}
	// Only GoTrue's VERIFICATION failure is asked about. The same provably
	// unknown credential refused for another reason — here a challenge GoTrue
	// does not know — is that refusal, not "unknown".
	res := postJSON(w.edge, passkeySignInVerifyPath, map[string]any{
		"challenge_id": "no-such-challenge", "credential": gotruetest.AssertionFor("gone-6", emptySub, gotruetest.FlagUPUV)}).Result()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if _, flagged := body["credential_unknown"]; flagged || body["error"] == PasskeyUnknownRefusal || res.StatusCode == http.StatusOK {
		t.Errorf("a refusal that is not a verification failure: %d %v", res.StatusCode, body)
	}
	// When GoTrue cannot answer, nothing is shown: an admin passkey list that
	// fails (500), or app_metadata (the recorded ids) that cannot be read, is
	// a plain refusal — never "unknown". recSub would be "unknown" if both
	// answered (every passkey recorded, none "gone-7").
	for _, c := range []struct {
		name string
		fail func()
	}{
		{"the admin passkey list fails", func() { w.fake.FailAdminPasskeys(1) }},
		{"the recorded ids cannot be read", func() { w.fake.FailAdminUserGet(1) }},
	} {
		c.fail()
		res := signIn("gone-7", recSub)
		var body map[string]any
		_ = json.NewDecoder(res.Body).Decode(&body)
		if _, flagged := body["credential_unknown"]; flagged || res.StatusCode != http.StatusUnauthorized || body["error"] != PasskeyRejectedRefusal {
			t.Errorf("%s: %d %v; want a plain refusal", c.name, res.StatusCode, body)
		}
	}
	// CONTROL: with both answering, the same sign-in IS "unknown".
	if res := signIn("gone-7", recSub); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("CONTROL: %d", res.StatusCode)
	} else {
		var body map[string]any
		_ = json.NewDecoder(res.Body).Decode(&body)
		if body["credential_unknown"] != true {
			t.Fatalf("CONTROL: the same sign-in with GoTrue answering: %v", body)
		}
	}
	// CONTROL: the recorded passkey itself still signs in.
	if res := signIn("rec-1", recSub); res.StatusCode != http.StatusOK {
		t.Fatalf("CONTROL: the recorded passkey: %d", res.StatusCode)
	}
}

// TestASignInRecordsTheIdOfTheOnlyUnrecordedPasskey: a passkey registered
// before muster recorded ids gets its id at its next sign-in when it is the
// account's ONLY unrecorded one (GoTrue's acceptance proves the credential is
// that passkey); with two unrecorded, which one signed in cannot be told, and
// nothing is recorded.
func TestASignInRecordsTheIdOfTheOnlyUnrecordedPasskey(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, at, rt := w.joinMember("pk-backfill@invite.test")
	if rec := w.addPasskey("already", gotruetest.FlagUPUV, withCookies(at, rt)); rec.Code != 200 {
		t.Fatalf("add: %d", rec.Code)
	}
	old := w.fake.AddPasskey(sub, "old-key")
	if rec := w.passkeySignIn("old-key", gotruetest.FlagUPUV); rec.Code != 200 {
		t.Fatalf("sign in: %d %s", rec.Code, rec.Body)
	}
	if got := w.recordedCredIDs(sub)[old]; got != b64url("old-key") {
		t.Fatalf("the only unrecorded passkey after its sign-in: %q (all %v)", got, w.recordedCredIDs(sub))
	}
	// A RECORDED passkey signing in is never recorded again, onto another: with
	// A recorded and a legacy B unrecorded, a sign-in with A leaves B
	// unrecorded (the set stays withheld) — B never gets A's id, which would
	// make the accepted set [A, A] and drop B from the password manager.
	_, sub3, at3, rt3 := w.joinMember("pk-backfill3@invite.test")
	if rec := w.addPasskey("a-key", gotruetest.FlagUPUV, withCookies(at3, rt3)); rec.Code != 200 {
		t.Fatalf("add: %d", rec.Code)
	}
	legacy := w.fake.AddPasskey(sub3, "b-key")
	if rec := w.passkeySignIn("a-key", gotruetest.FlagUPUV); rec.Code != 200 {
		t.Fatalf("sign in with the recorded one: %d %s", rec.Code, rec.Body)
	}
	if got := w.recordedCredIDs(sub3)[legacy]; got != "" {
		t.Fatalf("the legacy passkey after a sign-in with the recorded one got id %q (all %v)", got, w.recordedCredIDs(sub3))
	}
	if d, ok := pageSignal(t, do(w.edge, "GET", accountPasskeysPath, nil, withCookies(at3, rt3)).Body.String()); !ok || d.IDs != nil {
		t.Fatalf("the signal with one unrecorded passkey: %+v (ok %v); want the set withheld", d, ok)
	}
	_, sub2, _, _ := w.joinMember("pk-backfill2@invite.test")
	w.fake.AddPasskey(sub2, "two-a")
	w.fake.AddPasskey(sub2, "two-b")
	if rec := w.passkeySignIn("two-a", gotruetest.FlagUPUV); rec.Code != 200 {
		t.Fatalf("sign in: %d", rec.Code)
	}
	for id, got := range w.recordedCredIDs(sub2) {
		if got != "" {
			t.Fatalf("with two unrecorded passkeys, %s got id %q", id, got)
		}
	}
}

// TestAResetThatRemovedPasskeysSignalsWhatRemains: the page a ticked reset
// lands on hands the Signal API the (now empty) set, so the password manager
// stops offering the removed passkeys. The set is GoTrue's, never the query's:
// a hand-typed ?passkeys=3 for an account that still has a passkey signals
// that passkey, and a reset that kept them signals nothing.
func TestAResetThatRemovedPasskeysSignalsWhatRemains(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-reset-signal@invite.test")
	w.fake.AddPasskey(sub, "lost-1")
	w.fake.AddPasskey(sub, "lost-2")
	r := do(w.edge, "POST", ResetPath, w.resetForm(sub, "a-new-long-password", true))
	if r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/account?reset=1&passkeys=2" {
		t.Fatalf("reset: %d %q", r.Code, r.Header().Get("Location"))
	}
	ck := setCookies(r)
	cookies := withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)
	d, ok := pageSignal(t, do(w.edge, "GET", "/account?reset=1&passkeys=2", nil, cookies).Body.String())
	if !ok || d.IDs == nil || len(*d.IDs) != 0 || d.UserID != b64url(sub) || d.RPID != "localhost" {
		t.Fatalf("the signal after a removing reset: %+v (ok %v)", d, ok)
	}
	if rec := w.addPasskey("new-key", gotruetest.FlagUPUV, cookies); rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	d, ok = pageSignal(t, do(w.edge, "GET", "/account?reset=1&passkeys=3", nil, cookies).Body.String())
	if !ok || d.IDs == nil || !reflect.DeepEqual(*d.IDs, []string{b64url("new-key")}) {
		t.Fatalf("a hand-typed count signals what GoTrue lists: %+v", d)
	}
	for _, v := range []string{"kept", "none", "unknown", "0"} {
		if _, ok := pageSignal(t, do(w.edge, "GET", "/account?reset=1&passkeys="+v, nil, cookies).Body.String()); ok {
			t.Fatalf("passkeys=%s drew a signal", v)
		}
	}
}
