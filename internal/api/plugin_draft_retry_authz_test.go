package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// retryOnlyStore is a plugins.Store whose Draft read fails with readErr and
// that records whether a retry reached it (every other method panics).
type retryOnlyStore struct {
	plugins.Store
	readErr error
	retried bool
}

func (f *retryOnlyStore) Draft(context.Context, int64) (plugins.Draft, error) {
	return plugins.Draft{ID: 1, OwnerID: 1}, f.readErr
}

func (f *retryOnlyStore) RetryDraft(context.Context, int64, plugins.DraftDecisionSource) (plugins.Draft, error) {
	f.retried = true
	return plugins.Draft{ID: 2, OwnerID: 1, Attempt: 2}, nil
}

// TestAManualRetryFailsClosedWhenTheDraftCannotBeRead: retryAs's decider
// check must not be skipped by a read error — the retry is refused before
// the store (500, or 404 for none). CONTROL: a readable draft reaches it.
func TestAManualRetryFailsClosedWhenTheDraftCannotBeRead(t *testing.T) {
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	for _, c := range []struct {
		err  error
		code int
	}{{errors.New("connection reset"), http.StatusInternalServerError}, {plugins.ErrNotFound, http.StatusNotFound}} {
		st := &retryOnlyStore{readErr: c.err}
		s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
		s.ext.Plugins = st
		_, refused := s.retryAs(ctx, 1, plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser})
		if st.retried || refused == nil || refused.code != c.code {
			t.Errorf("read error %v: retried %v, refusal %+v; want %d before the store", c.err, st.retried, refused, c.code)
		}
	}
	st := &retryOnlyStore{}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.ext.Plugins = st
	if _, refused := s.retryAs(ctx, 1, plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}); refused != nil || !st.retried {
		t.Fatalf("control: a readable draft: refused %+v, retried %v; want it retried", refused, st.retried)
	}
}
