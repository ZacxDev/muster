package connectors

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// KeySize is the encryption key's length: AES-256.
const KeySize = 32

// ErrDecrypt is returned when a stored secret does not open under the configured
// key — the wrong key, or a ciphertext that was moved to another row.
//
// 🔴 IT FAILS CLOSED: no fallback key, no plaintext column, no "try without
// additional data". The proxy answers 502 and forwards nothing.
var ErrDecrypt = errors.New("connectors: the stored secret could not be decrypted with the configured key")

// sealer encrypts connector secrets with AES-256-GCM.
//
// 🔴 THE CONNECTOR NAME IS ADDITIONAL AUTHENTICATED DATA. A ciphertext sealed
// for `github` does not open as `deploy-bot`, so a row-level swap in the database
// (one connector's ciphertext copied onto another's row, to send a secret to the
// other's hosts) fails authentication instead of quietly succeeding.
type sealer struct{ aead cipher.AEAD }

func newSealer(key []byte) (sealer, error) {
	if len(key) != KeySize {
		return sealer{}, fmt.Errorf("connectors: encryption key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return sealer{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return sealer{}, err
	}
	return sealer{aead: aead}, nil
}

func aad(name string) []byte { return []byte("muster-connector-v1\x00" + name) }

// seal returns nonce||ciphertext||tag.
func (s sealer) seal(name, secret string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("connectors: nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, []byte(secret), aad(name)), nil
}

func (s sealer) open(name string, blob []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(blob) < n+s.aead.Overhead() {
		return "", ErrDecrypt
	}
	pt, err := s.aead.Open(nil, blob[:n], blob[n:], aad(name))
	if err != nil {
		return "", ErrDecrypt
	}
	return string(pt), nil
}
