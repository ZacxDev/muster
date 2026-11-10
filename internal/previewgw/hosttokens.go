package previewgw

// hosttokens.go — the mpt_ host tokens: minted on the CLI, stored as their
// sha256 in STATE_DIR, read by the running gateway on every tunnel accept and
// on its 30 s recheck. There is no admin socket: the CLI and the gateway
// share the file, and a flock on a sibling lock file orders them (the CLI
// takes it exclusive to write, the gateway shared to read). A write goes to a
// temporary file renamed over the old one, so a reader never sees half a
// file even without the lock.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

// File names in STATE_DIR.
const (
	hostTokensFile = "host-tokens.json"
	hostTokensLock = "host-tokens.lock"
)

// tokenSecretBytes is the random part of a host token: 256 bits.
const tokenSecretBytes = 32

// hostTokenLen is the length of a whole token: the prefix and the unpadded
// base64url of tokenSecretBytes.
var hostTokenLen = len(wire.TokenPrefix) + base64.RawURLEncoding.EncodedLen(tokenSecretBytes)

// HostToken is one stored host token. The secret itself is never stored.
type HostToken struct {
	ID string `json:"id"` // "ht_" + 8 hex; what ls prints and revoke takes
	// OwnerID is muster's owner id, from GET /plugin/v1/self when the token
	// was created: what the gateway keys the host's tunnel and previews on.
	OwnerID int64 `json:"owner_id"`
	// OwnerLogin is that owner's login as /self named it: a LABEL for ls,
	// never compared with anything.
	OwnerLogin string `json:"owner_login"`
	Host       string `json:"host"`
	Hash       string `json:"sha256"` // hex sha256 of the whole token
	Created    string `json:"created"`
}

// ErrNoHostToken is Revoke's error for an id the file does not hold.
var ErrNoHostToken = errors.New("no host token with that id")

// HostTokens is the token file in one STATE_DIR.
type HostTokens struct{ Dir string }

func (s HostTokens) path() string { return filepath.Join(s.Dir, hostTokensFile) }

// HashToken is a token's stored form.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// TokenShapeOK reports whether s is shaped like a host token: the prefix and
// exactly the base64url characters of 32 bytes. A string that is not is
// refused without reading the file.
func TokenShapeOK(s string) bool {
	if len(s) != hostTokenLen || !strings.HasPrefix(s, wire.TokenPrefix) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s[len(wire.TokenPrefix):])
	return err == nil
}

// List reads every stored token under a shared lock. A missing file is an
// empty list.
func (s HostTokens) List() ([]HostToken, error) {
	unlock, err := lockFile(filepath.Join(s.Dir, hostTokensLock), false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.readLocked()
}

func (s HostTokens) readLocked() ([]HostToken, error) {
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("host tokens: %w", err)
	}
	var toks []HostToken
	if err := json.Unmarshal(b, &toks); err != nil {
		return nil, fmt.Errorf("host tokens: %s is not valid: %w", hostTokensFile, err)
	}
	return toks, nil
}

func (s HostTokens) writeLocked(toks []HostToken) error {
	b, err := json.MarshalIndent(toks, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, hostTokensFile+".tmp*")
	if err != nil {
		return fmt.Errorf("host tokens: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("host tokens: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("host tokens: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("host tokens: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("host tokens: %w", err)
	}
	return os.Rename(tmp.Name(), s.path())
}

// update runs fn on the stored list under the exclusive lock and writes back
// what it returns.
func (s HostTokens) update(fn func([]HostToken) ([]HostToken, error)) error {
	unlock, err := lockFile(filepath.Join(s.Dir, hostTokensLock), true)
	if err != nil {
		return err
	}
	defer unlock()
	toks, err := s.readLocked()
	if err != nil {
		return err
	}
	toks, err = fn(toks)
	if err != nil {
		return err
	}
	return s.writeLocked(toks)
}

// Create mints a token for (ownerID, host), stores its hash, and returns the
// token (shown once) and its record. ownerLogin is stored as a label.
func (s HostTokens) Create(ownerID int64, ownerLogin, host string, now time.Time) (string, HostToken, error) {
	if err := mirror.ValidHost(host); err != nil {
		return "", HostToken{}, err
	}
	if ownerID <= 0 {
		return "", HostToken{}, errors.New("host tokens: an owner id is required")
	}
	secret := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", HostToken{}, err
	}
	idb := make([]byte, 4)
	if _, err := rand.Read(idb); err != nil {
		return "", HostToken{}, err
	}
	tok := wire.TokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	rec := HostToken{ID: "ht_" + hex.EncodeToString(idb), OwnerID: ownerID, OwnerLogin: ownerLogin,
		Host: host, Hash: HashToken(tok), Created: now.UTC().Format(time.RFC3339)}
	err := s.update(func(toks []HostToken) ([]HostToken, error) {
		for _, t := range toks {
			if t.ID == rec.ID {
				return nil, errors.New("host tokens: id collision; run it again")
			}
		}
		return append(toks, rec), nil
	})
	if err != nil {
		return "", HostToken{}, err
	}
	return tok, rec, nil
}

// Revoke deletes the token with id. A gateway ends its tunnel at its next
// recheck (at most 30 s).
func (s HostTokens) Revoke(id string) (HostToken, error) {
	var gone HostToken
	err := s.update(func(toks []HostToken) ([]HostToken, error) {
		out := toks[:0]
		for _, t := range toks {
			if t.ID == id {
				gone = t
				continue
			}
			out = append(out, t)
		}
		if gone.ID == "" {
			return nil, ErrNoHostToken
		}
		return out, nil
	})
	return gone, err
}

// tokenIndex is the stored tokens by hash.
type tokenIndex map[string]HostToken

func indexTokens(toks []HostToken) tokenIndex {
	ix := make(tokenIndex, len(toks))
	for _, t := range toks {
		ix[t.Hash] = t
	}
	return ix
}

// sortedTokens orders tokens for ls: by owner label, then host, then id.
func sortedTokens(toks []HostToken) []HostToken {
	out := append([]HostToken(nil), toks...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.OwnerLogin != b.OwnerLogin {
			return a.OwnerLogin < b.OwnerLogin
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.ID < b.ID
	})
	return out
}

// SortedForList is List's answer in ls order.
func (s HostTokens) SortedForList() ([]HostToken, error) {
	toks, err := s.List()
	if err != nil {
		return nil, err
	}
	return sortedTokens(toks), nil
}
