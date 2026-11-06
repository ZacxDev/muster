package api

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
)

// The attention tests' shared fixtures.

// attentionUser is user 1's signed-in session.
func attentionUser() context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleMember, Via: authz.ViaSession})
}

// itemsByWindow is items by the window of each one's card.
func itemsByWindow(items []attentionItem) map[string]attentionItem {
	out := map[string]attentionItem{}
	for _, it := range items {
		out[it.card.ref.Window] = it
	}
	return out
}

// attentionServer is the real mux with the attention routes and m.
func attentionServer(t *testing.T, m Mirror) (*Server, *httptest.Server) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal(),
		Mirror: m, Attention: emptyAttention{}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// emptyAttention is an attention store with nothing in it.
type emptyAttention struct{ attention.Store }

func (emptyAttention) Open(context.Context) ([]attention.Ask, error) { return nil, nil }
func (emptyAttention) Answered(context.Context, time.Time, int) ([]attention.Ask, error) {
	return nil, nil
}
