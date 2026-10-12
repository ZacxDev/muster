package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Minter turns a member's OpenRouter MANAGEMENT key into an inference key their
// agents can use, and revokes it again. See the package doc for why.
type Minter interface {
	// Mint creates an inference key under the management key's account and
	// returns it with the handle Revoke takes.
	Mint(ctx context.Context, managementKey, name string) (key, ref string, err error)
	// Revoke deletes a key Mint created.
	Revoke(ctx context.Context, managementKey, ref string) error
}

// DefaultOpenRouterBase is OpenRouter's API root.
const DefaultOpenRouterBase = "https://openrouter.ai/api/v1"

// OpenRouterMinter is the [Minter] over OpenRouter's key-management API
// (POST /keys, DELETE /keys/{hash}).
//
// ⚠ THE RESPONSE SHAPE IS READ FROM OpenRouter's DOCUMENTATION, NOT FROM A LIVE
// CALL: no management key was available when this was written. It accepts the
// documented `{"key": "...", "data": {"hash": "..."}}`, and refuses (rather than
// guessing) any response that carries no `sk-or-` key, so a changed shape is a
// clear "could not mint" on the credentials page, never a stored garbage key.
type OpenRouterMinter struct {
	// Base is the API root; empty means [DefaultOpenRouterBase]. Tests point it
	// at an httptest server.
	Base   string
	Client *http.Client
}

func (m OpenRouterMinter) base() string {
	if m.Base == "" {
		return DefaultOpenRouterBase
	}
	return strings.TrimRight(m.Base, "/")
}

func (m OpenRouterMinter) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// errMint is the one failure a page shows. 🔴 It never carries the response
// body: an API echoing a key back must not reach a log or a page.
func errMint(status int) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return invalid("OpenRouter refused that key (is it a MANAGEMENT key? An ordinary API key cannot create keys)")
	}
	return fmt.Errorf("OpenRouter could not create a key for your agents (HTTP %d); try again", status)
}

// Mint implements [Minter].
func (m OpenRouterMinter) Mint(ctx context.Context, managementKey, name string) (string, string, error) {
	body, _ := json.Marshal(map[string]any{"name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base()+"/keys", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+managementKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client().Do(req)
	if err != nil {
		// The transport error names the URL, never the header.
		return "", "", fmt.Errorf("OpenRouter could not be reached: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", errMint(resp.StatusCode)
	}
	var out struct {
		Key  string `json:"key"`
		Data struct {
			Hash string `json:"hash"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || !strings.HasPrefix(out.Key, OpenRouterKeyPrefix) || out.Data.Hash == "" {
		return "", "", errors.New("OpenRouter answered, but not with a new key; nothing was saved")
	}
	return out.Key, out.Data.Hash, nil
}

// Revoke implements [Minter].
func (m OpenRouterMinter) Revoke(ctx context.Context, managementKey, ref string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, m.base()+"/keys/"+url.PathEscape(ref), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+managementKey)
	resp, err := m.client().Do(req)
	if err != nil {
		return fmt.Errorf("OpenRouter could not be reached: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("OpenRouter refused to delete the minted key (HTTP %d)", resp.StatusCode)
	}
	return nil
}
