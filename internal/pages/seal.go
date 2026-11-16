package pages

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// LinkKeySize is the length of the master key WithLinkKey takes: the connector
// store's (MUSTER_CONNECTOR_ENCRYPTION_KEY), which the link sealer derives its
// own key from.
const LinkKeySize = 32

// ErrNoLinkKey is a reusable link asked of a store that keeps none: this
// server has no encryption key, so a link's address cannot be kept.
var ErrNoLinkKey = errors.New("pages: this server keeps no reusable links (MUSTER_CONNECTOR_ENCRYPTION_KEY is unset)")

// errLinkDecrypt is a sealed token that does not open: another key, or a
// ciphertext moved to another row.
var errLinkDecrypt = errors.New("pages: a reusable link's token could not be decrypted with the configured key")

// linkHKDFInfo is the derivation label, and the domain separator from the
// other stores derived from the same master key (internal/credentials uses
// "muster-user-credentials-v1"; the connector store the master key itself), so
// no ciphertext of theirs can open here or the other way round.
const linkHKDFInfo = "muster-page-links-v1"

// linkSealer is a reusable link's token at rest: AES-256-GCM over a key
// derived (HKDF-SHA256) from the connector key, a random nonce prepended — the
// construction internal/credentials and internal/connectors use.
//
// 🔴 THE CIPHERTEXT IS BOUND TO ITS ROW: the owner, the page and the token's
// own hash are its associated data. A sealed token copied onto another row (a
// bad UPDATE, another owner's page) does not open, so it can never be shown
// as that row's address; and a token whose hash is not the row's lookup key
// cannot be handed out as if it were. TestASealedTokenMovedToAnotherRowDoesNotOpen.
type linkSealer struct{ aead cipher.AEAD }

func newLinkSealer(master []byte) (*linkSealer, error) {
	if len(master) != LinkKeySize {
		return nil, fmt.Errorf("pages: link encryption key must be %d bytes, got %d", LinkKeySize, len(master))
	}
	key, err := hkdf.Key(sha256.New, master, nil, linkHKDFInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &linkSealer{aead: aead}, nil
}

func linkAAD(owner, page int64, hash string) []byte {
	out := []byte(linkHKDFInfo)
	out = binary.BigEndian.AppendUint64(out, uint64(owner))
	out = binary.BigEndian.AppendUint64(out, uint64(page))
	out = binary.BigEndian.AppendUint32(out, uint32(len(hash)))
	return append(out, hash...)
}

func (s *linkSealer) seal(owner, page int64, token string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("pages: nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, []byte(token), linkAAD(owner, page, TokenHash(token))), nil
}

// open returns the token sealed in blob for the row (owner, page, hash), and
// checks that it IS that row's token (its hash is the row's).
func (s *linkSealer) open(owner, page int64, hash string, blob []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(blob) < n+s.aead.Overhead() {
		return "", errLinkDecrypt
	}
	pt, err := s.aead.Open(nil, blob[:n], blob[n:], linkAAD(owner, page, hash))
	if err != nil {
		return "", errLinkDecrypt
	}
	tok := string(pt)
	if !TokenShapeOK(tok) || TokenHash(tok) != hash {
		return "", errLinkDecrypt
	}
	return tok, nil
}
