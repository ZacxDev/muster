package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/devices"
)

// A PAIRED PHONE ANSWERS AN ASK, SIGNED (device_actions.go), over the real
// stores, a real gotrue session and the browser listener: the challenge, the
// step-up key's signature, the spend, and the web's own answer core.

// actionRig is a device rig with the attention store, and the owner's phone
// paired with a step-up key the test holds.
type actionRig struct {
	*deviceRig
	attn *attention.PGStore
	key  *ecdsa.PrivateKey
	tok  string
	dev  int64
}

func newActionRig(t *testing.T) *actionRig {
	t.Helper()
	a := &actionRig{}
	a.deviceRig = newDeviceRig(t, func(ext *Extensions, pool *pgxpool.Pool) {
		a.attn = attention.NewPG(pool)
		ext.Attention = a.attn
	})
	t.Cleanup(func() {
		_, _ = a.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE owner_id = $1`, a.uid)
	})
	a.key, a.tok, a.dev = a.pairKeyed(t, "keyed phone")
	return a
}

// pairKeyed pairs a phone with a fresh biometric StrongBox key.
func (a *actionRig) pairKeyed(t *testing.T, name string) (*ecdsa.PrivateKey, string, int64) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	at, rt := a.login()
	w := do(a.edge, "POST", accountDevicesPairPath, nil, withCookies(at, rt))
	var code struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(w.Body.Bytes(), &code) != nil {
		t.Fatalf("code: %d %s", w.Code, w.Body.String())
	}
	body, _ := json.Marshal(map[string]string{"code": code.Code, "name": name, "app_version": "0.2.0",
		"stepup_pubkey": base64.StdEncoding.EncodeToString(der), "stepup_key_type": devices.KeyBiometric, "stepup_key_level": devices.LevelStrongBox})
	w = a.call(a.edge, "POST", devicePairPath, "", string(body))
	var out struct {
		Token  string `json:"token"`
		Device struct {
			ID int64 `json:"id"`
		} `json:"device"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("pair: %d %s", w.Code, w.Body.String())
	}
	return k, out.Token, out.Device.ID
}

// askFor files an open ask owned by owner.
func (a *actionRig) askFor(t *testing.T, owner int64, q string) int64 {
	t.Helper()
	ask, err := a.attn.Create(asUser(owner), attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, Question: q, Options: []string{"yes", "no"}})
	if err != nil {
		t.Fatal(err)
	}
	return ask.ID
}

func (a *actionRig) status(t *testing.T, owner, id int64) attention.Ask {
	t.Helper()
	ask, err := a.attn.Get(asUser(owner), id)
	if err != nil {
		t.Fatal(err)
	}
	return ask
}

// challenge asks for one: its nonce and message.
func (a *actionRig) challenge(t *testing.T, tok, action string, id int64, text *string) (string, string) {
	t.Helper()
	body := map[string]string{"action": action, "target": strconv.FormatInt(id, 10)}
	if text != nil {
		sum := sha256.Sum256([]byte(*text))
		body["text_sha256"] = hex.EncodeToString(sum[:])
	}
	b, _ := json.Marshal(body)
	w := a.call(a.edge, "POST", deviceChallengePath, tok, string(b))
	var out struct {
		Nonce   string `json:"nonce"`
		Message string `json:"message"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Nonce == "" {
		t.Fatalf("challenge %s %d: %d %s", action, id, w.Code, w.Body.String())
	}
	return out.Nonce, out.Message
}

func sign(t *testing.T, k *ecdsa.PrivateKey, msg string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(msg))
	sig, err := ecdsa.SignASN1(rand.Reader, k, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

// act POSTs a signed action; its status and refusal code.
func (a *actionRig) act(t *testing.T, tok, verb string, id int64, body map[string]string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	w := a.call(a.edge, "POST", deviceActionsPrefix+"/attention/asks/"+strconv.FormatInt(id, 10)+"/"+verb, tok, string(b))
	var out struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Code
}

func strp(s string) *string { return &s }

// TestAPhoneAnswersAnAskWithASignedAction: the whole happy path, and the
// replay. The challenge's message is the documented one; the signed answer is
// recorded by the web's own core (answered, by the operator, the answer
// verbatim) with a device.action audit row naming the text's hash; the SAME
// nonce and signature sent again is refused challenge_used and moves nothing.
func TestAPhoneAnswersAnAskWithASignedAction(t *testing.T) {
	a := newActionRig(t)
	id := a.askFor(t, a.uid, "Ship it?")
	other := a.askFor(t, a.uid, "And the docs?")
	answer := "yes — after the smoke tests"
	nonce, msg := a.challenge(t, a.tok, actionAskAnswer, id, &answer)
	sum := sha256.Sum256([]byte(answer))
	if want := strings.Join([]string{"muster-device-action-v1", nonce, "ask_answer", strconv.FormatInt(id, 10), hex.EncodeToString(sum[:])}, "\n"); msg != want {
		t.Fatalf("the message to sign:\n%q\nwant\n%q", msg, want)
	}
	body := map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": answer}
	if c, code := a.act(t, a.tok, "answer", id, body); c != http.StatusOK {
		t.Fatalf("the signed answer: %d %s", c, code)
	}
	if got := a.status(t, a.uid, id); got.Status != attention.Answered || got.Answer != answer || got.AnsweredBy != attention.ByOperator {
		t.Fatalf("the ask after the signed answer: %+v", got)
	}
	var n int
	if err := a.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log WHERE user_id = $1 AND action = 'device.action'
		AND object = $2 AND detail->>'target' = $3 AND detail->>'text_sha256' = $4`, a.uid, devices.Object(a.dev), strconv.FormatInt(id, 10), hex.EncodeToString(sum[:])).Scan(&n); err != nil || n != 1 {
		t.Fatalf("device.action audit rows: %d (%v)", n, err)
	}
	// The replay, at another ask too: refused, nothing moves.
	if c, code := a.act(t, a.tok, "answer", id, body); c != http.StatusUnauthorized || code != actionCodeUsed {
		t.Fatalf("the replay: %d %s, want 401 challenge_used", c, code)
	}
	if c, code := a.act(t, a.tok, "answer", other, body); c != http.StatusUnauthorized || code != actionCodeUsed {
		t.Fatalf("the replay on another ask: %d %s", c, code)
	}
	if got := a.status(t, a.uid, other); got.Status != attention.Open {
		t.Fatalf("the replay moved another ask: %+v", got)
	}
}

// TestASignedActionIsRefusedForEachWayItIsWrong: each refusal leaves the ask
// open and, where the challenge was this device's, spends it. Each case has
// its own fresh challenge and differs from the happy path in ONE thing.
func TestASignedActionIsRefusedForEachWayItIsWrong(t *testing.T) {
	a := newActionRig(t)
	yes := "yes"
	intruder, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	t.Run("a signature by another key", func(t *testing.T) {
		id := a.askFor(t, a.uid, "Bad signature?")
		nonce, msg := a.challenge(t, a.tok, actionAskAnswer, id, &yes)
		if c, code := a.act(t, a.tok, "answer", id, map[string]string{"nonce": nonce, "signature": sign(t, intruder, msg), "answer": yes}); c != http.StatusUnauthorized || code != actionCodeBadSignature {
			t.Fatalf("%d %s, want 401 bad_signature", c, code)
		}
		// The nonce is burnt: the right signature on it now is a replay.
		if c, code := a.act(t, a.tok, "answer", id, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": yes}); code != actionCodeUsed {
			t.Fatalf("the good signature after a bad one: %d %s, want challenge_used", c, code)
		}
		if a.status(t, a.uid, id).Status != attention.Open {
			t.Fatal("the ask moved")
		}
	})
	t.Run("another answer than the one signed for", func(t *testing.T) {
		id := a.askFor(t, a.uid, "Text swap?")
		nonce, msg := a.challenge(t, a.tok, actionAskAnswer, id, &yes)
		if c, code := a.act(t, a.tok, "answer", id, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": "no"}); c != http.StatusUnauthorized || code != actionCodeInvalid {
			t.Fatalf("%d %s, want 401 challenge_invalid", c, code)
		}
		if a.status(t, a.uid, id).Status != attention.Open {
			t.Fatal("the ask moved")
		}
	})
	t.Run("another ask than the one signed for", func(t *testing.T) {
		id, id2 := a.askFor(t, a.uid, "Target A?"), a.askFor(t, a.uid, "Target B?")
		nonce, msg := a.challenge(t, a.tok, actionAskAnswer, id, &yes)
		if c, code := a.act(t, a.tok, "answer", id2, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": yes}); c != http.StatusUnauthorized || code != actionCodeInvalid {
			t.Fatalf("%d %s, want 401 challenge_invalid", c, code)
		}
		if a.status(t, a.uid, id2).Status != attention.Open || a.status(t, a.uid, id).Status != attention.Open {
			t.Fatal("an ask moved")
		}
	})
	t.Run("an expired challenge", func(t *testing.T) {
		id := a.askFor(t, a.uid, "Expired?")
		nonce, msg := a.challenge(t, a.tok, actionAskAnswer, id, &yes)
		if _, err := a.pool.Exec(authz.System(context.Background()), `UPDATE device_challenges SET expires_at = now() - interval '1 second' WHERE device_id = $1 AND used_at IS NULL`, a.dev); err != nil {
			t.Fatal(err)
		}
		if c, code := a.act(t, a.tok, "answer", id, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": yes}); c != http.StatusUnauthorized || code != actionCodeExpired {
			t.Fatalf("%d %s, want 401 challenge_expired", c, code)
		}
		if a.status(t, a.uid, id).Status != attention.Open {
			t.Fatal("the ask moved")
		}
	})
	t.Run("a settled ask", func(t *testing.T) {
		id := a.askFor(t, a.uid, "Settled?")
		if _, err := a.attn.Cancel(asUser(a.uid), id); err != nil {
			t.Fatal(err)
		}
		nonce, msg := a.challenge(t, a.tok, actionAskAnswer, id, &yes)
		if c, code := a.act(t, a.tok, "answer", id, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": yes}); c != http.StatusConflict || code != actionCodeNotOpen {
			t.Fatalf("%d %s, want 409 not_open", c, code)
		}
		if got := a.status(t, a.uid, id); got.Status != attention.Cancelled || got.Answer != "" {
			t.Fatalf("the settled ask was written: %+v", got)
		}
	})
	t.Run("another owner's ask", func(t *testing.T) {
		var other int64
		if err := a.pool.QueryRow(authz.System(context.Background()), `INSERT INTO users (role, display_name) VALUES ('member', 'someone else') RETURNING id`).Scan(&other); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			sys := authz.System(context.Background())
			_, _ = a.pool.Exec(sys, `DELETE FROM attention_asks WHERE owner_id = $1`, other)
			_, _ = a.pool.Exec(sys, `DELETE FROM users WHERE id = $1`, other)
		})
		theirs := a.askFor(t, other, "Theirs?")
		nonce, msg := a.challenge(t, a.tok, actionAskAnswer, theirs, &yes)
		if c, code := a.act(t, a.tok, "answer", theirs, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": yes}); c != http.StatusNotFound || code != actionCodeNotFound {
			t.Fatalf("%d %s, want 404 not_found", c, code)
		}
		if a.status(t, other, theirs).Status != attention.Open {
			t.Fatal("another owner's ask moved")
		}
	})
	t.Run("another device's challenge", func(t *testing.T) {
		_, tok2, _ := a.pairKeyed(t, "second phone")
		id := a.askFor(t, a.uid, "Other device?")
		nonce, msg := a.challenge(t, tok2, actionAskAnswer, id, &yes)
		// The first phone presents the second phone's nonce, signed by its own key.
		if c, code := a.act(t, a.tok, "answer", id, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": yes}); c != http.StatusUnauthorized || code != actionCodeInvalid {
			t.Fatalf("%d %s, want 401 challenge_invalid", c, code)
		}
		if a.status(t, a.uid, id).Status != attention.Open {
			t.Fatal("the ask moved")
		}
	})
}

// TestAPhoneDismissesAnAskSigned: ask_dismiss is the web's cancel, signed; a
// challenge for it carries no text and one for ask_answer must.
func TestAPhoneDismissesAnAskSigned(t *testing.T) {
	a := newActionRig(t)
	id := a.askFor(t, a.uid, "Dismiss me?")
	for _, c := range []struct {
		body map[string]string
		want int
	}{
		{map[string]string{"action": actionAskAnswer, "target": strconv.FormatInt(id, 10)}, http.StatusBadRequest},
		{map[string]string{"action": actionAskDismiss, "target": strconv.FormatInt(id, 10), "text_sha256": strings.Repeat("ab", 32)}, http.StatusBadRequest},
		{map[string]string{"action": "answer", "target": strconv.FormatInt(id, 10)}, http.StatusBadRequest},
		{map[string]string{"action": actionAskDismiss, "target": "01"}, http.StatusBadRequest},
	} {
		b, _ := json.Marshal(c.body)
		if w := a.call(a.edge, "POST", deviceChallengePath, a.tok, string(b)); w.Code != c.want {
			t.Fatalf("challenge %v: %d %s, want %d", c.body, w.Code, w.Body.String(), c.want)
		}
	}
	nonce, msg := a.challenge(t, a.tok, actionAskDismiss, id, nil)
	if c, code := a.act(t, a.tok, "cancel", id, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg)}); c != http.StatusOK {
		t.Fatalf("the signed dismissal: %d %s", c, code)
	}
	if got := a.status(t, a.uid, id); got.Status != attention.Cancelled {
		t.Fatalf("the ask after the signed dismissal: %+v", got)
	}
}

// TestTheConfigSaysActionsWithOneSwitch: where the asks are served, the
// device config turns features.actions on — the one signal the app reads —
// and carries no second, parallel list of ids (round-0 audit: a top-level
// action_ids nothing read). RED at the first #314 head, which sent both.
func TestTheConfigSaysActionsWithOneSwitch(t *testing.T) {
	a := newActionRig(t)
	w := a.call(a.edge, "GET", deviceConfigPath, a.tok, "")
	var cfg map[string]json.RawMessage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cfg) != nil {
		t.Fatalf("config: %d %s", w.Code, w.Body.String())
	}
	if _, ok := cfg["action_ids"]; ok {
		t.Errorf("the config still carries action_ids: %s", w.Body.String())
	}
	var features map[string]bool
	if json.Unmarshal(cfg["features"], &features) != nil || !features["actions"] {
		t.Errorf("features.actions is not on where the asks are served: %s", cfg["features"])
	}
}

// TestAReadOnlyPhoneCannotAct: a phone paired without a step-up key gets no
// challenge (403 no_stepup_key), so it can sign nothing.
func TestAReadOnlyPhoneCannotAct(t *testing.T) {
	a := newActionRig(t)
	at, rt := a.login()
	tok, _ := a.pair(at, rt, "read-only phone")
	id := a.askFor(t, a.uid, "Read only?")
	b, _ := json.Marshal(map[string]string{"action": actionAskDismiss, "target": strconv.FormatInt(id, 10)})
	w := a.call(a.edge, "POST", deviceChallengePath, tok, string(b))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"`+actionCodeNoKey+`"`) {
		t.Fatalf("a read-only phone's challenge: %d %s", w.Code, w.Body.String())
	}
}
