package clankmailplugin

// sign.go — keys, signed objects, and the §6.4 request-auth builder.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// ReadSeedFile reads a ClankMail key file: ONE line holding the base64url
// (RFC 4648 §5, no padding) of a 32-byte Ed25519 seed, optionally followed by a
// newline — the key-file format other ClankMail clients use. The decoding is canonical (SPEC §2):
// the decoded bytes must re-encode to exactly the text read, so padding,
// whitespace or a second line are refused, not ignored.
func ReadSeedFile(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read the key file")
	}
	text := strings.TrimSuffix(string(b), "\n")
	seed, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(seed) != ed25519.SeedSize || base64.RawURLEncoding.EncodeToString(seed) != text {
		return nil, errors.New("not a ClankMail key file (one line of base64url, 32 bytes)")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// WriteSeedFile creates path (mode 0600; it must not exist) holding a new
// random seed in ReadSeedFile's format, and fsyncs it and its directory.
func WriteSeedFile(path string) (ed25519.PrivateKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.WriteString(base64.RawURLEncoding.EncodeToString(seed) + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// KeyIDOf is the k_ id of priv's public key.
func KeyIDOf(priv ed25519.PrivateKey) string {
	return clankmail.KeyID(priv.Public().(ed25519.PublicKey))
}

// SignObject builds the payload {v:1, t, from, ts, …fields}, signs those exact
// bytes, and runs the result through the full §4 pipeline before returning it,
// so a field this code got wrong is refused here rather than by the hub. fields
// must not set v, t, from or ts. String values must be valid UTF-8: JSON would
// otherwise replace the bad bytes and sign something other than what was given.
func SignObject(priv ed25519.PrivateKey, t string, tsMS int64, fields map[string]any) (token, id string, err error) {
	payload := map[string]any{"v": 1, "t": t, "from": KeyIDOf(priv), "ts": tsMS}
	for k, v := range fields {
		if _, clash := payload[k]; clash {
			return "", "", fmt.Errorf("field %q is set by SignObject", k)
		}
		if s, ok := v.(string); ok && !utf8.ValidString(s) {
			return "", "", fmt.Errorf("field %q is not valid UTF-8", k)
		}
		payload[k] = v
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return "", "", err
	}
	raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	token = clankmail.Sign(priv, raw)
	obj, err := clankmail.Verify(token)
	if err != nil {
		return "", "", fmt.Errorf("could not build a valid %s: %w", t, err)
	}
	return token, obj.ID, nil
}

// AuthorizationHeader is the §6.4 `auth` for ONE request, as the value of its
// Authorization header: `ClankMail <token>`, the token signing
//
//	{"v":1,"t":"auth","from":<k_>,"ts":<ms>,"hub":<HUB_NAME>,"method":<METHOD>,
//	 "path":<request-target>,"body":<hex sha256 of the body bytes>,"n":<nonce>}
//
// requestURI is the request-target EXACTLY as it goes on the wire (path plus
// `?query`, percent-encoding included): the hub compares it byte for byte with
// its own request line. Callers pass url.URL.RequestURI() of the URL the
// http.Client sends, which is what net/http writes. body is the exact body
// bytes (nil or empty for none: the hash of empty). n is 12 random bytes in hex,
// so two identical requests in one millisecond still differ.
//
// ⚠ CLOCKS. The hub accepts an auth whose ts is within ±60 s of its own clock
// (and a submission within ±300 s). The host running the plugin must keep its
// clock synchronised (NTP); a skew past 60 s turns every inbox poll and allow
// call into 401 E_AUTH, which the loops report on /health and retry.
func AuthorizationHeader(priv ed25519.PrivateKey, hubName, method, requestURI string, body []byte, nowMS int64) (string, error) {
	sum := sha256.Sum256(body)
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	tok, _, err := SignObject(priv, "auth", nowMS, map[string]any{
		"hub": hubName, "method": method, "path": requestURI,
		"body": hex.EncodeToString(sum[:]), "n": hex.EncodeToString(nonce),
	})
	if err != nil {
		return "", err
	}
	return "ClankMail " + tok, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
