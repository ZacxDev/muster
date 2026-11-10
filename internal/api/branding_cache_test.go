package api

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/branding"
)

// scriptedBranding is a branding.Store whose Load answers from a script, one
// entry per call (the last entry repeats), and counts the calls.
type scriptedBranding struct {
	mu     sync.Mutex
	script []scriptedLoad
	loads  int
}

type scriptedLoad struct {
	b   branding.Branding
	err error
}

func (s *scriptedBranding) Load(context.Context) (branding.Branding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := min(s.loads, len(s.script)-1)
	s.loads++
	return s.script[i].b, s.script[i].err
}

func (s *scriptedBranding) Apply(context.Context, branding.Change) (branding.Branding, error) {
	return branding.Branding{}, errors.New("scriptedBranding: Apply is not used here")
}

func (s *scriptedBranding) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

// cacheServer is a server reading branding from script, on a clock the test
// moves (*now).
func cacheServer(t *testing.T, script ...scriptedLoad) (*Server, *scriptedBranding, *time.Time) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	store := &scriptedBranding{script: script}
	s.UseExtensions(Extensions{Branding: store})
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, store, &now
}

// TestTheBrandingRowIsCachedForThirtySeconds: within 30 s of a read the row is
// served from the cache (no second Load), and at 30 s it is read again. The
// rows are distinct, so a render that reloaded would show the second one. The
// TTL is a literal here, not brandCacheTTL, so changing it fails this test.
func TestTheBrandingRowIsCachedForThirtySeconds(t *testing.T) {
	first := branding.Branding{Name: "First", ETag: "1111111111111111"}
	second := branding.Branding{Name: "Second", ETag: "2222222222222222"}
	s, store, now := cacheServer(t, scriptedLoad{b: first}, scriptedLoad{b: second})
	ctx := context.Background()

	if got := s.instanceBranding(ctx); got.Name != "First" || store.loadCount() != 1 {
		t.Fatalf("first render: %q after %d loads, want First after 1", got.Name, store.loadCount())
	}
	for _, d := range []time.Duration{0, time.Second, 29*time.Second + 999*time.Millisecond} {
		*now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).Add(d)
		if got := s.instanceBranding(ctx); got.Name != "First" || store.loadCount() != 1 {
			t.Fatalf("+%v: %q after %d loads, want First from the cache (1 load)", d, got.Name, store.loadCount())
		}
	}
	*now = time.Date(2026, 11, 10, 12, 0, 30, 0, time.UTC)
	if got := s.instanceBranding(ctx); got.Name != "Second" || store.loadCount() != 2 {
		t.Fatalf("+30s: %q after %d loads, want Second after 2 (the cache expired)", got.Name, store.loadCount())
	}
}

// TestAFailedBrandingReadKeepsTheLastRow: when a read fails after one that
// succeeded, the page still draws the last row read (not the built-in mark),
// and the failure is itself cached for the TTL — a database that is down
// costs one read per 30 s, not one per page. Once the store answers again,
// the next read after that TTL shows the new row.
func TestAFailedBrandingReadKeepsTheLastRow(t *testing.T) {
	last := branding.Branding{Name: "Last good", ETag: "3333333333333333"}
	next := branding.Branding{Name: "Recovered", ETag: "4444444444444444"}
	s, store, now := cacheServer(t,
		scriptedLoad{b: last},
		scriptedLoad{err: errors.New("database is down")},
		scriptedLoad{b: next},
	)
	ctx := context.Background()
	start := *now

	if got := s.instanceBranding(ctx); got.Name != "Last good" {
		t.Fatalf("first render: %q, want Last good", got.Name)
	}
	*now = start.Add(31 * time.Second) // expired: this read fails
	if got := s.instanceBranding(ctx); got.Name != "Last good" || got.ETag != last.ETag || store.loadCount() != 2 {
		t.Fatalf("failed read: %+v after %d loads, want the last row read after 2", got, store.loadCount())
	}
	*now = start.Add(60 * time.Second) // 29 s after the failure: still cached
	if got := s.instanceBranding(ctx); got.Name != "Last good" || store.loadCount() != 2 {
		t.Fatalf("29s after the failure: %q after %d loads, want Last good with no new load", got.Name, store.loadCount())
	}
	*now = start.Add(61 * time.Second) // 30 s after the failure: read again
	if got := s.instanceBranding(ctx); got.Name != "Recovered" || store.loadCount() != 3 {
		t.Fatalf("after the TTL: %q after %d loads, want Recovered after 3", got.Name, store.loadCount())
	}
}

// TestAFailedFirstBrandingReadIsTheBuiltInMark: with no row ever read, a
// failed read is "nothing set".
func TestAFailedFirstBrandingReadIsTheBuiltInMark(t *testing.T) {
	s, _, _ := cacheServer(t, scriptedLoad{err: errors.New("database is down")})
	if got := s.instanceBranding(context.Background()); got.Name != "" || got.ETag != "" || got.Logo != nil {
		t.Fatalf("got %+v, want the zero Branding", got)
	}
}
