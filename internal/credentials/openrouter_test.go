package credentials

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheMinterSpeaksOpenRouterKeyManagement pins the wire: POST /keys with the
// management key as Bearer, the minted key read from the documented shape; a
// 401 is a refusal that says "management key"; a response with no key stores
// nothing; DELETE /keys/{hash} revokes. No error carries a key or a body.
func TestTheMinterSpeaksOpenRouterKeyManagement(t *testing.T) {
	const mgmt = "sk-or-v1-FAKE-mgmt-wire-000000000000"
	var seen []string
	answer := `{"data":{"hash":"h-123","name":"muster user 41"},"key":"sk-or-v1-FAKE-minted-wire-1111111111"}`
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	m := OpenRouterMinter{Base: srv.URL}
	ctx := context.Background()

	key, ref, err := m.Mint(ctx, mgmt, "muster user 41")
	if err != nil || key != "sk-or-v1-FAKE-minted-wire-1111111111" || ref != "h-123" {
		t.Fatalf("Mint = %q %q %v", key, ref, err)
	}
	if seen[0] != `POST /keys Bearer `+mgmt+` {"name":"muster user 41"}` {
		t.Fatalf("request = %q", seen[0])
	}
	if err := m.Revoke(ctx, mgmt, "h-123"); err != nil || !strings.HasPrefix(seen[1], "DELETE /keys/h-123 Bearer "+mgmt) {
		t.Fatalf("Revoke: %v, request %q", err, seen[1])
	}

	status, answer = http.StatusUnauthorized, `{"error":"bad key `+mgmt+`"}`
	_, _, err = m.Mint(ctx, mgmt, "x")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "MANAGEMENT key") || strings.Contains(err.Error(), mgmt) {
		t.Fatalf("401: %v", err)
	}
	status, answer = http.StatusOK, `{"data":{"hash":"h"},"key":"not-a-key"}`
	if _, _, err := m.Mint(ctx, mgmt, "x"); err == nil || strings.Contains(err.Error(), "not-a-key") {
		t.Fatalf("a response with no sk-or- key: %v", err)
	}
	status, answer = http.StatusInternalServerError, `oops `+mgmt
	if _, _, err := m.Mint(ctx, mgmt, "x"); err == nil || strings.Contains(err.Error(), mgmt) {
		t.Fatalf("500: %v", err)
	}
}
