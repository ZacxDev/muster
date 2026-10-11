package connectors

import (
	"bytes"
	"errors"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func TestSealRoundTripsAndHidesThePlaintext(t *testing.T) {
	s, err := newSealer(testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "ghp_FAKE-plaintext-0123456789"
	ct, err := s.seal("github", secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte(secret)) || bytes.Contains(ct, []byte("plaintext")) {
		t.Fatal("the ciphertext contains the plaintext")
	}
	got, err := s.open("github", ct)
	if err != nil || got != secret {
		t.Fatalf("open = %q, %v", got, err)
	}
	ct2, _ := s.seal("github", secret)
	if bytes.Equal(ct, ct2) {
		t.Fatal("two seals of one secret are identical: the nonce is not random")
	}
}

func TestTheWrongKeyFailsClosed(t *testing.T) {
	a, _ := newSealer(testKey(1))
	b, _ := newSealer(testKey(2))
	ct, _ := a.seal("github", "ghp_FAKE-0123456789")
	if got, err := b.open("github", ct); !errors.Is(err, ErrDecrypt) || got != "" {
		t.Fatalf("open with the wrong key = %q, %v; want ErrDecrypt and nothing", got, err)
	}
	if _, err := a.open("github", ct[:5]); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a truncated ciphertext = %v, want ErrDecrypt", err)
	}
}

// TestACiphertextIsBoundToItsConnectorName: a row-level swap (one connector's
// ciphertext copied onto another connector's row) must not open.
func TestACiphertextIsBoundToItsConnectorName(t *testing.T) {
	s, _ := newSealer(testKey(1))
	ct, _ := s.seal("github", "ghp_FAKE-0123456789")
	if _, err := s.open("deploy-bot", ct); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a ciphertext opened under another connector's name: %v", err)
	}
}

func TestTheKeyMustBe32Bytes(t *testing.T) {
	if _, err := newSealer(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte key was accepted")
	}
}
