package devices

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// SIGNED ACTIONS (docs/design/android.md, "A gated action, step by step";
// migration 0058): a paired phone asks for a one-time CHALLENGE bound to an
// action, its target and the sha256 of the exact text it carries; the phone's
// step-up key (Android Keystore, a fresh biometric per use) signs the
// challenge's message; the action route spends the challenge and verifies the
// signature before it acts.
//
// 🔴 THE MESSAGE IS REBUILT FROM THE STORED ROW, never taken from the request:
// what the key signed is checked against what the challenge was bound to, so a
// signature over one answer cannot carry another, and a challenge asked for
// one ask cannot act on another.
//
// 🔴 SPENT BEFORE IT IS JUDGED. The row is marked used by one conditional
// UPDATE (used_at IS NULL, unexpired, this device), in the same transaction as
// the verification — and committed whatever the verification says, so a bad
// signature burns its nonce as surely as a good one: every challenge is tried
// at most once.

const (
	// NoncePrefix starts every challenge nonce.
	NoncePrefix = "mdc_"
	// ChallengeTTL is how long a challenge can be spent.
	ChallengeTTL = 2 * time.Minute
	// MessageVersion is the first line of every signed message.
	MessageVersion = "muster-device-action-v1"
)

var (
	// ErrNoStepUpKey: the device paired without a step-up key (read-only).
	ErrNoStepUpKey = errors.New("devices: this device has no step-up key, so it can take no action")
	// ErrChallengeInvalid: no such challenge for this device, or one bound to
	// another action, target or text.
	ErrChallengeInvalid = errors.New("devices: no such challenge for this action")
	// ErrChallengeUsed: the challenge was already spent (a replay, or a retry
	// of a failed attempt).
	ErrChallengeUsed = errors.New("devices: this challenge was already used")
	// ErrChallengeExpired: the challenge lapsed (ChallengeTTL).
	ErrChallengeExpired = errors.New("devices: this challenge has expired")
	// ErrBadSignature: the signature does not verify against the device's
	// step-up key over the challenge's message.
	ErrBadSignature = errors.New("devices: the signature does not verify")
)

// Challenge is what a signed action is bound to.
type Challenge struct {
	DeviceID int64
	// Action is the action's wire id (docs/design/android.md, "Action ids").
	Action string
	// Target is the action's target (an ask's id).
	Target string
	// TextSHA256 is the sha256 of the exact text the action carries; nil for
	// an action that carries none.
	TextSHA256 []byte
}

// TextHash is the sha256 of text, as a challenge binds it.
func TextHash(text string) []byte {
	sum := sha256.Sum256([]byte(text))
	return sum[:]
}

// Message is the exact text a device signs for nonce and c: MessageVersion,
// the nonce, the action, the target and the hex sha256 of the text ("" for
// none), one per line, joined by "\n", no trailing newline, UTF-8. The
// signature is ECDSA P-256 over its SHA-256 (Java's "SHA256withECDSA"), DER
// encoded.
func Message(nonce string, c Challenge) string {
	return strings.Join([]string{MessageVersion, nonce, c.Action, c.Target, hex.EncodeToString(c.TextSHA256)}, "\n")
}

// DecodeSignature reads a base64 signature (standard or URL alphabet, padded
// or not).
func DecodeSignature(s string) ([]byte, error) {
	raw := strings.TrimRight(strings.TrimSpace(s), "=")
	if raw == "" || len(raw) > 200 {
		return nil, ErrBadSignature
	}
	b, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		b, err = base64.RawURLEncoding.DecodeString(raw)
	}
	if err != nil {
		return nil, ErrBadSignature
	}
	return b, nil
}

// verify reports whether sig is der's key's signature over msg.
func verify(der []byte, msg string, sig []byte) bool {
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return false
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(msg))
	return ecdsa.VerifyASN1(ec, sum[:], sig)
}

// CreateChallenge mints a challenge for c under ctx's principal (the device's
// owner), returning its nonce ONCE and when it expires. ErrNoStepUpKey for a
// device without a key; ErrKeyNotAllowed for a biometric-or-PIN key while the
// server refuses them (allowPIN false).
func (s *PGStore) CreateChallenge(ctx context.Context, c Challenge, allowPIN bool) (string, time.Time, error) {
	p, err := user(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	nonce, err := newSecret(NoncePrefix)
	if err != nil {
		return "", time.Time{}, err
	}
	var expires time.Time
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := stepUpKeyTx(ctx, tx, c.DeviceID, allowPIN); err != nil {
			return err
		}
		// Lapsed challenges of this device go with each new one.
		if _, err := tx.Exec(ctx, `DELETE FROM device_challenges WHERE device_id = $1 AND expires_at < now() - interval '1 hour'`, c.DeviceID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO device_challenges (owner_id, device_id, nonce_sha256, action, target, text_sha256, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7)) RETURNING expires_at`,
			p.UserID, c.DeviceID, hash(nonce), c.Action, c.Target, nilIfEmpty(c.TextSHA256), ChallengeTTL.Seconds()).Scan(&expires)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return nonce, expires, nil
}

func nilIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// stepUpKeyTx is device id's step-up key and type, refused when it has none or
// its type is one the server does not accept now.
func stepUpKeyTx(ctx context.Context, tx pgx.Tx, id int64, allowPIN bool) ([]byte, string, error) {
	var der []byte
	var typ *string
	err := tx.QueryRow(ctx, `SELECT stepup_pubkey, stepup_key_type FROM devices WHERE id = $1`, id).Scan(&der, &typ)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrBadToken
	}
	if err != nil {
		return nil, "", err
	}
	if der == nil || typ == nil {
		return nil, "", ErrNoStepUpKey
	}
	if *typ == KeyBiometricOrCredential && !allowPIN {
		return nil, "", fmt.Errorf("%w: this server does not accept biometric-or-PIN keys", ErrKeyNotAllowed)
	}
	return der, *typ, nil
}

// SpendChallenge spends nonce for c under ctx's principal and verifies sig
// over its message with the device's step-up key; nil means the action may
// proceed. On success it appends the audit row (device.action) naming the
// device, the action, the target and the text's hash, in the same
// transaction. See the file header for why a refusal still spends the nonce.
func (s *PGStore) SpendChallenge(ctx context.Context, nonce string, c Challenge, sig []byte, allowPIN bool) error {
	p, err := user(ctx)
	if err != nil {
		return err
	}
	if !shapeOK(NoncePrefix, nonce) {
		return ErrChallengeInvalid
	}
	var verdict error
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			id                  int64
			action, target      string
			text                []byte
			used, expired, mine bool
		)
		err := tx.QueryRow(ctx, `SELECT id, action, target, text_sha256, used_at IS NOT NULL, expires_at <= now(), device_id = $2
			FROM device_challenges WHERE nonce_sha256 = $1 FOR UPDATE`, hash(nonce), c.DeviceID).
			Scan(&id, &action, &target, &text, &used, &expired, &mine)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			verdict = ErrChallengeInvalid
			return nil
		case err != nil:
			return err
		case !mine:
			verdict = ErrChallengeInvalid
			return nil
		case used:
			verdict = ErrChallengeUsed
			return nil
		}
		// Spent now, whatever follows.
		if _, err := tx.Exec(ctx, `UPDATE device_challenges SET used_at = now() WHERE id = $1 AND used_at IS NULL`, id); err != nil {
			return err
		}
		switch {
		case expired:
			verdict = ErrChallengeExpired
			return nil
		case action != c.Action || target != c.Target || subtle.ConstantTimeCompare(text, c.TextSHA256) != 1:
			verdict = ErrChallengeInvalid
			return nil
		}
		der, typ, err := stepUpKeyTx(ctx, tx, c.DeviceID, allowPIN)
		if err != nil {
			if errors.Is(err, ErrNoStepUpKey) || errors.Is(err, ErrKeyNotAllowed) || errors.Is(err, ErrBadToken) {
				verdict = err
				return nil
			}
			return err
		}
		if !verify(der, Message(nonce, Challenge{DeviceID: c.DeviceID, Action: action, Target: target, TextSHA256: text}), sig) {
			verdict = ErrBadSignature
			return nil
		}
		return users.AuditTx(ctx, tx, users.Actor{UserID: p.UserID, Via: authz.ViaDevice}, "device.action", Object(c.DeviceID),
			map[string]any{"action": action, "target": target, "text_sha256": hex.EncodeToString(text), "stepup_key_type": typ})
	})
	if err != nil {
		return err
	}
	return verdict
}
