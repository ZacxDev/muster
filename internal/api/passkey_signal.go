package api

// --- the WebAuthn Signal API: keeping password managers in step ----------------
//
// A passkey removed here stays in the person's password manager (or phone)
// until something tells it otherwise, and keeps being offered at sign-in,
// where GoTrue refuses it. WebAuthn Level 3's Signal API is how a site tells
// it: PublicKeyCredential.signalAllAcceptedCredentials (the full set this
// account accepts — anything else for this user is hidden or removed),
// signalUnknownCredential (this one credential is not accepted here) and
// signalCurrentUserDetails (the account's name). Every call is FEATURE-DETECTED
// in the page and its failure ignored: a browser without the API just keeps
// offering the old passkey, as before.
//
// What the calls need, and where muster gets it:
//
//   - userId is the user handle the passkey was registered with. GoTrue
//     v2.197.0 registers user.id as []byte(user.ID.String()) (User.WebAuthnID),
//     so it is the base64url of the GoTrue subject's TEXT — known to muster
//     for any signed-in person.
//   - The credential ids are NOT in GoTrue's passkey list (PasskeyListItem has
//     id, friendly_name, created_at, last_used_at; WebAuthnCredential's
//     credential_id is json:"-"). So muster RECORDS each one itself, next to
//     the backup flags it already records (passkeyMetaKey, "cred_id"): at
//     registration from the attestation, and — for a passkey registered before
//     muster recorded ids — at sign-in, when it can be attributed without
//     guessing (backfillCredentialID).
//   - rpId is GoTrue's GOTRUE_WEBAUTHN_RP_ID, which muster has no setting
//     for: it is read off the options GoTrue hands out (rpIDCache).
//
// 🔴 signalAllAcceptedCredentials IS SENT ONLY WITH THE COMPLETE SET. A list
// missing one of the account's passkeys tells the password manager that
// passkey is gone — it would hide a way in the person still has. So the set is
// sent only when EVERY passkey GoTrue lists has a recorded id
// (TestTheSignalIsWithheldWhileAnyCredentialIdIsUnknown); otherwise only the
// user details are.
//
// 🔴 signalUnknownCredential IS SENT ONLY WHEN MUSTER CAN SHOW THE CREDENTIAL
// IS NOT THIS ACCOUNT'S. GoTrue answers every failed passkey sign-in with the
// same 400 webauthn_verification_failed — an unknown credential, a bad
// signature, a user handle that names nobody — so its answer alone says
// nothing. credentialUnknown asks GoTrue's admin API about the user the
// assertion's user handle names, and says "unknown" only for: no such user, a
// user with no passkeys, or a user whose every passkey has a recorded id and
// none is this one (TestAnUnknownCredentialIsFlaggedOnlyWhenProvablyUnknown).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue"
)

// passkeyCredIDKey is the key, in a passkey's record under passkeyMetaKey, of
// its WebAuthn credential id (gotrue.CredentialID).
const passkeyCredIDKey = "cred_id"

// PasskeyUnknownRefusal answers a passkey sign-in GoTrue refused when muster
// can show the credential is not one this account has (credentialUnknown).
const PasskeyUnknownRefusal = "That passkey is no longer registered here. Sign in with your password, then add a new passkey under Account → Passkeys."

// rpIDCache is the WebAuthn RP ID GoTrue's options last named.
type rpIDCache struct {
	mu sync.Mutex
	id string
}

// noteRPID remembers the RP ID in GoTrue's options: rpId in sign-in options,
// rp.id in registration options. Options naming none change nothing.
func (g *gotrueState) noteRPID(options json.RawMessage) {
	var o struct {
		RPID string `json:"rpId"`
		RP   struct {
			ID string `json:"id"`
		} `json:"rp"`
	}
	if json.Unmarshal(options, &o) != nil {
		return
	}
	id := o.RPID
	if id == "" {
		id = o.RP.ID
	}
	if id == "" {
		return
	}
	g.rp.mu.Lock()
	g.rp.id = id
	g.rp.mu.Unlock()
}

// rpID is GoTrue's RP ID: the one its options last named, else — once per
// process, the first time a page needs it before any options passed through —
// read off a sign-in options call. "" when that fails.
func (g *gotrueState) rpID(ctx context.Context) string {
	g.rp.mu.Lock()
	id := g.rp.id
	g.rp.mu.Unlock()
	if id != "" {
		return id
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	o, err := g.Client.PasskeySignInOptions(ctx, "muster-server")
	if err != nil {
		return ""
	}
	g.noteRPID(o.Options)
	g.rp.mu.Lock()
	defer g.rp.mu.Unlock()
	return g.rp.id
}

// passkeySignal is what the page hands the Signal API (passkeySignalJS).
// AllAcceptedCredentialIDs is nil — and signalAllAcceptedCredentials is not
// called — unless it is the complete set.
type passkeySignal struct {
	RPID                     string   `json:"rpId"`
	UserID                   string   `json:"userId"`
	AllAcceptedCredentialIDs []string `json:"allAcceptedCredentialIds"`
	Name                     string   `json:"name,omitempty"`
}

// userHandleOf is the WebAuthn user handle GoTrue registers for subject sub,
// base64url-encoded as the Signal API takes it.
func userHandleOf(sub string) string { return base64.RawURLEncoding.EncodeToString([]byte(sub)) }

// recordedCredID is the credential id recorded for the passkey whose record is
// f, or "".
func recordedCredID(f map[string]any) string {
	id, _ := f[passkeyCredIDKey].(string)
	return id
}

// signalFor builds the signal for subject sub (whose email is name) and its
// passkeys list, as GoTrue listed them, with flags their records. ok is false
// when there is nothing to send (no RP ID).
func (s *Server) signalFor(ctx context.Context, sub, name string, list []gotrue.Passkey, flags map[string]map[string]any) (passkeySignal, bool) {
	rp := s.gt.rpID(ctx)
	if rp == "" || sub == "" {
		return passkeySignal{}, false
	}
	sig := passkeySignal{RPID: rp, UserID: userHandleOf(sub), Name: name}
	if flags == nil { // the records could not be read: the set is unknown
		return sig, true
	}
	ids := []string{}
	for _, p := range list {
		id := recordedCredID(flags[p.ID])
		if id == "" {
			return sig, true
		}
		ids = append(ids, id)
	}
	sig.AllAcceptedCredentialIDs = ids
	return sig, true
}

// signalNode is the element and script that hand sig to the Signal API.
func signalNode(sig passkeySignal) string {
	b, err := json.Marshal(sig)
	if err != nil {
		return ""
	}
	return `<div id="passkey-signal" hidden data-signal="` + html.EscapeString(string(b)) + `"></div>
<script>` + passkeySignalJS + `</script>`
}

// isUUIDText reports whether s is a UUID in its canonical text form — what
// GoTrue's user handles are.
func isUUIDText(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
		default:
			return false
		}
	}
	return true
}

// credentialUnknown reports whether the credential credID, whose assertion
// named user handle handle, is provably NOT a passkey of the account GoTrue
// would have checked it against: the user does not exist, has no passkeys, or
// has only passkeys whose recorded ids all differ. Anything unreadable or
// unrecorded is "not provably unknown" (false).
func (s *Server) credentialUnknown(ctx context.Context, ip, credID string, handle []byte) bool {
	sub := string(handle)
	if credID == "" || !isUUIDText(sub) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	list, err := s.gt.Client.AdminPasskeys(ctx, ip, sub)
	if err != nil {
		var ae *gotrue.APIError
		return errors.As(err, &ae) && ae.Status == http.StatusNotFound && ae.Code == "user_not_found"
	}
	if len(list) == 0 {
		return true
	}
	flags := s.passkeyFlags(ctx, sub)
	if flags == nil {
		return false
	}
	for _, p := range list {
		id := recordedCredID(flags[p.ID])
		if id == "" || id == credID {
			return false
		}
	}
	return true
}

// backfillCredentialID records credID — the credential sub just signed in
// with — on the one passkey it can only be: when sub's passkeys include
// exactly ONE with no recorded id and credID is recorded on none, GoTrue's
// accepting the assertion proves credID is that passkey's. With two or more
// unrecorded it could be either, and nothing is written. Best effort, logged.
func (s *Server) backfillCredentialID(ctx context.Context, ip, sub, credID string) {
	if sub == "" || credID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	flags := s.passkeyFlags(ctx, sub)
	if flags == nil {
		return
	}
	for _, f := range flags {
		if recordedCredID(f) == credID {
			return
		}
	}
	list, err := s.gt.Client.AdminPasskeys(ctx, ip, sub)
	if err != nil {
		s.logger.Printf("passkeys: could not list passkeys to record a credential id: %v", err)
		return
	}
	only := ""
	for _, p := range list {
		if recordedCredID(flags[p.ID]) != "" {
			continue
		}
		if only != "" {
			return
		}
		only = p.ID
	}
	if only == "" {
		return
	}
	rec := map[string]any{}
	for k, v := range flags[only] {
		rec[k] = v
	}
	rec[passkeyCredIDKey] = credID
	s.setPasskeyFlag(ctx, sub, only, rec)
}
