package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/push"
)

// ---------------------------------------------------------------------------
// A PAIRED PHONE'S SIGNED ACTIONS, Phase 2 — asks only (docs/design/android.md,
// "A gated action, step by step", "Which actions are gated", "Action ids";
// the contract an app builds against: "The signed ask actions, as built").
//
//   POST /api/device/challenge            {action, target, text_sha256?}
//     → {nonce, expires_at, message}: a one-time challenge bound to the
//       action, its target and the sha256 of the exact text (internal/devices
//       challenge.go). The app signs `message` with its step-up key.
//   POST /api/device/actions/attention/asks/{id}/answer  {nonce, signature, answer}
//   POST /api/device/actions/attention/asks/{id}/cancel  {nonce, signature}
//
// 🔴 ONE CORE, TWO DOORS. A verified answer is answerAskFrom — the SAME call
// the Attention view's form and a session's attention_answer make, under the
// device owner's own principal (row-level security: another owner's ask is
// not found) — and a dismissal is cancelAsk. So the notification cleared, the
// waiters woken, the delivery queued: all identical to the web.
//
// 🔴 EVERY ACTION IS SIGNED (operator decision: one rule, refusals included),
// and the challenge is spent before the action is judged, so a retry needs a
// new challenge and a replay is refused.
// ---------------------------------------------------------------------------

// The action wire ids this server serves, each naming exactly one route — the
// app's table (docs/design/android.md, "Action ids"), exact strings.
const (
	actionAskAnswer  = push.ActionAskAnswer
	actionAskDismiss = push.ActionAskDismiss
)

const (
	deviceChallengePath  = deviceAPIPrefix + "/challenge"
	deviceActionsPrefix  = deviceAPIPrefix + "/actions"
	deviceAskAnswerRoute = "POST " + deviceActionsPrefix + "/attention/asks/{id}/answer"
	deviceAskCancelRoute = "POST " + deviceActionsPrefix + "/attention/asks/{id}/cancel"
)

// deviceActionRoutes is THE LEDGER of signed action routes: wire id → the one
// route it names. Payload v2 offers only these ids (attentionActions), the mux
// registers exactly these routes, and TestEveryDeviceActionIsARouteAndARow
// holds both to the design doc's table.
var deviceActionRoutes = map[string]string{
	actionAskAnswer:  deviceAskAnswerRoute,
	actionAskDismiss: deviceAskCancelRoute,
}

// The machine-readable refusal codes of the signed routes (the `code` of the
// JSON error body), for the app to branch on.
const (
	actionCodeBadRequest    = "bad_request"
	actionCodeNoKey         = "no_stepup_key"
	actionCodeKeyNotAllowed = "key_not_allowed"
	actionCodeInvalid       = "challenge_invalid"
	actionCodeUsed          = "challenge_used"
	actionCodeExpired       = "challenge_expired"
	actionCodeBadSignature  = "bad_signature"
	actionCodeNotFound      = "not_found"
	actionCodeNotOpen       = "not_open"
	actionCodeInvalidAnswer = "invalid_answer"
	actionCodeFailed        = "failed"
)

func (s *Server) registerDeviceActionRoutes(mux Mux) {
	mux.HandleFunc("POST "+deviceChallengePath, s.requireDeviceToken(s.handleDeviceChallenge))
	mux.HandleFunc(deviceAskAnswerRoute, s.requireDeviceToken(s.handleDeviceAskAnswer))
	mux.HandleFunc(deviceAskCancelRoute, s.requireDeviceToken(s.handleDeviceAskCancel))
}

// actionRefuse answers a refused signed request: {error, code}.
func (s *Server) actionRefuse(w http.ResponseWriter, status int, code, msg string) {
	s.writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

// challengeRefusal maps a devices error to the route's answer; false when err
// is not one of the challenge's refusals.
func (s *Server) challengeRefusal(w http.ResponseWriter, err error) bool {
	msg := strings.TrimPrefix(func() string {
		if err == nil {
			return ""
		}
		return err.Error()
	}(), "devices: ")
	switch {
	case errors.Is(err, devices.ErrNoStepUpKey):
		s.actionRefuse(w, http.StatusForbidden, actionCodeNoKey, msg)
	case errors.Is(err, devices.ErrKeyNotAllowed):
		s.actionRefuse(w, http.StatusForbidden, actionCodeKeyNotAllowed, msg)
	case errors.Is(err, devices.ErrChallengeInvalid):
		s.actionRefuse(w, http.StatusUnauthorized, actionCodeInvalid, msg)
	case errors.Is(err, devices.ErrChallengeUsed):
		s.actionRefuse(w, http.StatusUnauthorized, actionCodeUsed, msg)
	case errors.Is(err, devices.ErrChallengeExpired):
		s.actionRefuse(w, http.StatusUnauthorized, actionCodeExpired, msg)
	case errors.Is(err, devices.ErrBadSignature):
		s.actionRefuse(w, http.StatusUnauthorized, actionCodeBadSignature, msg)
	case errors.Is(err, devices.ErrBadToken):
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": DeviceTokenRefusal})
	default:
		return false
	}
	return true
}

// handleDeviceChallenge is POST /api/device/challenge.
func (s *Server) handleDeviceChallenge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action     string `json:"action"`
		Target     string `json:"target"`
		TextSHA256 string `json:"text_sha256"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxDeviceBody)).Decode(&body); err != nil {
		s.actionRefuse(w, http.StatusBadRequest, actionCodeBadRequest, `the body is {"action","target","text_sha256"}`)
		return
	}
	if _, ok := deviceActionRoutes[body.Action]; !ok {
		s.actionRefuse(w, http.StatusBadRequest, actionCodeBadRequest, "unknown action "+strconv.Quote(body.Action))
		return
	}
	if id, err := strconv.ParseInt(body.Target, 10, 64); err != nil || id <= 0 || strconv.FormatInt(id, 10) != body.Target {
		s.actionRefuse(w, http.StatusBadRequest, actionCodeBadRequest, "the target is an ask's id")
		return
	}
	var text []byte
	switch body.Action {
	case actionAskAnswer:
		b, err := hex.DecodeString(body.TextSHA256)
		if err != nil || len(b) != 32 || strings.ToLower(body.TextSHA256) != body.TextSHA256 {
			s.actionRefuse(w, http.StatusBadRequest, actionCodeBadRequest, "ask_answer needs text_sha256: the lowercase hex sha256 of the answer's UTF-8 bytes")
			return
		}
		text = b
	default:
		if body.TextSHA256 != "" {
			s.actionRefuse(w, http.StatusBadRequest, actionCodeBadRequest, body.Action+" carries no text")
			return
		}
	}
	d := deviceFromContext(r.Context())
	c := devices.Challenge{DeviceID: d.ID, Action: body.Action, Target: body.Target, TextSHA256: text}
	nonce, expires, err := s.ext.Devices.CreateChallenge(r.Context(), c, s.ext.AllowPINKeys)
	if err != nil {
		if s.challengeRefusal(w, err) {
			return
		}
		s.logger.Printf("devices: device %d challenge: %v", d.ID, err)
		s.actionRefuse(w, http.StatusInternalServerError, actionCodeFailed, "the challenge could not be created")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"nonce": nonce, "expires_at": expires.UTC().Format(time.RFC3339),
		"expires_in": int(devices.ChallengeTTL / time.Second), "message": devices.Message(nonce, c)})
}

// maxDeviceActionBody bounds a signed action's JSON body: an answer of
// attention.MaxAnswerRunes characters at the costliest JSON spelling of one
// (a 4-byte character escaped as a surrogate pair, 12 bytes), plus the nonce,
// the signature and the envelope — so a valid answer is never refused as a
// malformed body before the answer's own rule (invalid_answer) reads it.
const maxDeviceActionBody = attention.MaxAnswerRunes*12 + 4<<10

// deviceAskAction is a signed ask action's common half: the body, the ask id
// from the path, the challenge spent and the signature verified for action
// with text (nil for none). It reports false, the request answered, on any
// refusal; on success, the challenge it verified (for RecordAction).
func (s *Server) deviceAskAction(w http.ResponseWriter, r *http.Request, action string, body any, nonce, sig *string, text func() []byte) (int64, devices.Challenge, bool) {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxDeviceActionBody)).Decode(body); err != nil {
		s.actionRefuse(w, http.StatusBadRequest, actionCodeBadRequest, "the body is not the action's JSON")
		return 0, devices.Challenge{}, false
	}
	id, ok := askIDParam(r)
	if !ok {
		s.actionRefuse(w, http.StatusNotFound, actionCodeNotFound, "no such question")
		return 0, devices.Challenge{}, false
	}
	sigBytes, err := devices.DecodeSignature(*sig)
	if err != nil {
		s.actionRefuse(w, http.StatusUnauthorized, actionCodeBadSignature, "the signature is not base64")
		return 0, devices.Challenge{}, false
	}
	d := deviceFromContext(r.Context())
	c := devices.Challenge{DeviceID: d.ID, Action: action, Target: strconv.FormatInt(id, 10), TextSHA256: text()}
	if err := s.ext.Devices.SpendChallenge(r.Context(), *nonce, c, sigBytes, s.ext.AllowPINKeys); err != nil {
		if s.challengeRefusal(w, err) {
			if !errors.Is(err, devices.ErrBadToken) {
				s.logger.Printf("devices: device %d %s on ask %d refused: %v", d.ID, action, id, err)
			}
			return 0, devices.Challenge{}, false
		}
		s.logger.Printf("devices: device %d %s on ask %d: %v", d.ID, action, id, err)
		s.actionRefuse(w, http.StatusInternalServerError, actionCodeFailed, "the action could not be checked")
		return 0, devices.Challenge{}, false
	}
	return id, c, true
}

// askActionResult answers a performed (or refused) ask action, as the web
// route's errors map, and audits the verified action WITH that outcome
// (devices.RecordAction): the ask's new status, or the refusal's code.
func (s *Server) askActionResult(w http.ResponseWriter, r *http.Request, c devices.Challenge, a attention.Ask, err error) {
	outcome := string(a.Status)
	switch {
	case err == nil:
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": a.ID, "status": a.Status})
	case errors.Is(err, attention.ErrNotFound):
		outcome = actionCodeNotFound
		s.actionRefuse(w, http.StatusNotFound, actionCodeNotFound, attentionErrText(err, a))
	case errors.Is(err, attention.ErrNotOpen):
		outcome = actionCodeNotOpen
		s.actionRefuse(w, http.StatusConflict, actionCodeNotOpen, attentionErrText(err, a))
	case errors.Is(err, attention.ErrInvalid):
		outcome = actionCodeInvalidAnswer
		s.actionRefuse(w, http.StatusBadRequest, actionCodeInvalidAnswer, attentionErrText(err, a))
	default:
		outcome = actionCodeFailed
		s.logger.Printf("devices: ask action: %v", err)
		s.actionRefuse(w, http.StatusInternalServerError, actionCodeFailed, attentionErrText(err, a))
	}
	if aerr := s.ext.Devices.RecordAction(r.Context(), c, outcome); aerr != nil {
		s.logger.Printf("devices: device %d: audit %s on ask %s (%s): %v", c.DeviceID, c.Action, c.Target, outcome, aerr)
	}
}

// handleDeviceAskAnswer is POST /api/device/actions/attention/asks/{id}/answer.
// The signed text is the answer EXACTLY as sent: its sha256 is what the
// challenge binds, before the answer is trimmed like any other.
func (s *Server) handleDeviceAskAnswer(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		s.actionRefuse(w, http.StatusNotFound, actionCodeNotFound, "this server serves no questions")
		return
	}
	var body struct {
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"`
		Answer    string `json:"answer"`
	}
	id, c, ok := s.deviceAskAction(w, r, actionAskAnswer, &body, &body.Nonce, &body.Signature, func() []byte { return devices.TextHash(body.Answer) })
	if !ok {
		return
	}
	a, err := s.answerAskFrom(r.Context(), id, body.Answer, attention.AnswerSource{})
	s.askActionResult(w, r, c, a, err)
}

// handleDeviceAskCancel is POST /api/device/actions/attention/asks/{id}/cancel.
func (s *Server) handleDeviceAskCancel(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		s.actionRefuse(w, http.StatusNotFound, actionCodeNotFound, "this server serves no questions")
		return
	}
	var body struct {
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"`
	}
	id, c, ok := s.deviceAskAction(w, r, actionAskDismiss, &body, &body.Nonce, &body.Signature, func() []byte { return nil })
	if !ok {
		return
	}
	a, err := s.cancelAsk(r.Context(), id)
	s.askActionResult(w, r, c, a, err)
}
