// Package github holds the single connected GitHub account: the OAuth access
// token (encrypted at rest with AES-256-GCM), the login, and granted scopes.
// The token is injected into dispatched agent pods (GITHUB_TOKEN, HTTPS clone)
// and used to list repos for the Repos tab and the dispatch modal.
package github

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"
)

// Connection is the stored GitHub account. Token is plaintext in memory; it is
// only ever persisted encrypted.
type Connection struct {
	Login       string    `json:"login"`
	Scopes      string    `json:"scopes"`
	Token       string    `json:"-"`
	ConnectedAt time.Time `json:"connectedAt"`
}

// Store is the GitHub-connection persistence behaviour. Implementations encrypt
// the token at rest and return it decrypted from Get.
type Store interface {
	Save(ctx context.Context, login, scopes, token string) error
	Get(ctx context.Context) (Connection, bool, error)
	Clear(ctx context.Context) error
}

// encrypt seals plaintext with AES-256-GCM, returning nonce||ciphertext||tag.
func encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("github: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("github: new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("github: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt opens a nonce||ciphertext||tag blob produced by encrypt.
func decrypt(key, blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("github: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("github: new gcm: %w", err)
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("github: ciphertext too short")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}
