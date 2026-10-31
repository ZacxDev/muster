package agentgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestAResolveFailureFromInsideATurnReadsAsUnsent pins the mark that lets a
// deliverer of an unsolicited turn (internal/plugins) run the TOOL-capable turn —
// which resolves the agent itself — and still tell "nothing was contacted" from
// "may have been pasted". Both resolve failures (an empty hooks token, a driver
// error) must read as unsent through Chat AND ChatWithTools, and keep their text.
// The control: a failure AFTER resolution, from the same kind of gateway, does not.
func TestAResolveFailureFromInsideATurnReadsAsUnsent(t *testing.T) {
	noToken := fixtureAgent()
	noToken.HooksToken = ""
	for _, c := range []struct {
		name   string
		driver *fixedResolver
		a      agents.Agent
	}{
		{"empty hooks token", &fixedResolver{ep: provision.Endpoint{Host: "127.0.0.1", Port: 1}}, noToken},
		{"driver error", &fixedResolver{err: provision.ErrNoEndpoint}, fixtureAgent()},
	} {
		for _, turn := range []string{"Chat", "ChatWithTools"} {
			t.Run(c.name+"/"+turn, func(t *testing.T) {
				g, err := New(Config{Driver: c.driver, Runtime: HooksSHA256(), Model: testSentinel})
				if err != nil {
					t.Fatal(err)
				}
				if turn == "Chat" {
					_, err = g.Chat(context.Background(), c.a, "s", "hi", nil)
				} else {
					_, err = g.ChatWithTools(context.Background(), c.a, "s", "", "hi", nil, nil, nil)
				}
				if got := Unsent(err); got != UnsentUnresolved {
					t.Fatalf("Unsent(%v) = %q; want %q — a resolve failure contacted nothing", err, got, UnsentUnresolved)
				}
				if !strings.Contains(err.Error(), c.a.Name) || strings.Contains(err.Error(), UnsentUnresolved) {
					t.Fatalf("the mark changed the error's text: %v", err)
				}
			})
		}
	}
	// Control: a failure after resolution (the runtime answered 500) is NOT unsent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	g, err := New(Config{Driver: &fixedResolver{ep: endpointOf(t, srv.URL)}, Runtime: HooksSHA256(), Model: testSentinel, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.ChatWithTools(context.Background(), fixtureAgent(), "s", "", "hi", nil, nil, nil)
	if err == nil || Unsent(err) != "" {
		t.Fatalf("a 500 after resolution: err=%v Unsent=%q; want an error that is NOT unsent", err, Unsent(err))
	}
}

// TestLanesFullReadsOnlyCcdsTypedCapacityRefusal: LanesFull is true for ccd's
// typed 503 `capacity` (a turn whose explicit session found no free slot) and
// for nothing else — not an untyped 503, not a typed 503 not_ready, not a 409
// busy. And the capacity refusal is NOT one of Unsent's reasons: adding it there
// would change every unsolicited deliverer's handling (unsent.go says why).
func TestLanesFullReadsOnlyCcdsTypedCapacityRefusal(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"typed capacity", http.StatusServiceUnavailable, `{"error":{"type":"capacity","message":"every Claude Code session ccd may run at once is mid-turn"}}`, true},
		{"untyped 503", http.StatusServiceUnavailable, `upstream unavailable`, false},
		{"typed not_ready", http.StatusServiceUnavailable, `{"error":{"type":"not_ready","message":"starting"}}`, false},
		{"capacity type on a 409", http.StatusConflict, `{"error":{"type":"capacity","message":"x"}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			g, err := New(Config{Driver: &fixedResolver{ep: endpointOf(t, srv.URL)}, Runtime: HooksSHA256(), Model: testSentinel, Client: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = g.Chat(context.Background(), fixtureAgent(), "s", "hi", nil)
			if err == nil {
				t.Fatal("the turn succeeded")
			}
			if got := LanesFull(err); got != c.want {
				t.Fatalf("LanesFull(%v) = %v, want %v", err, got, c.want)
			}
			if c.want && Unsent(err) != "" {
				t.Fatalf("Unsent(%v) = %q, want \"\" (capacity is deliberately not an Unsent reason)", err, Unsent(err))
			}
		})
	}
	if LanesFull(nil) {
		t.Fatal("LanesFull(nil)")
	}
}
