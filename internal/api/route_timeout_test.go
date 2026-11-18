package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestRouteTimeoutIsTheShortestDeadline: loadConfig checks the send queue's
// idle wait against RouteTimeout alone (cmd/muster-server/config.go), which is
// right only while every other route deadline is longer.
func TestRouteTimeoutIsTheShortestDeadline(t *testing.T) {
	for name, d := range map[string]time.Duration{"PRRouteTimeout": PRRouteTimeout, "ConversationRouteTimeout": ConversationRouteTimeout} {
		if d <= RouteTimeout {
			t.Errorf("%s (%s) is not longer than RouteTimeout (%s): loadConfig's idle-wait check no longer covers it", name, d, RouteTimeout)
		}
	}
}

// TestRouteTimeoutPicksByOriginAndMark: the deadline is the ORIGIN's (a
// pull-request review) or the MESSAGE's mark (human_conversation), never the
// plugin's name; a task, whatever else, keeps RouteTimeout.
func TestRouteTimeoutPicksByOriginAndMark(t *testing.T) {
	var s Server
	human := plugins.Message{ID: 1, HumanConversation: true}
	for _, c := range []struct {
		name   string
		origin plugins.RouteOrigin
		item   routedItem
		want   time.Duration
	}{
		{"the chief's route of an unmarked message", plugins.RouteOrigin{}, routedItem{msg: plugins.Message{ID: 1}}, 30 * time.Minute},
		{"the chief's route of a human_conversation message", plugins.RouteOrigin{}, routedItem{msg: human}, 4 * time.Hour},
		{"the owner's route of a human_conversation message", plugins.RouteOrigin{Via: plugins.RouteViaMCP}, routedItem{msg: human}, 4 * time.Hour},
		{"a route by thread of an unmarked message", plugins.RouteOrigin{Via: plugins.RouteViaThread}, routedItem{msg: plugins.Message{ID: 1}}, 30 * time.Minute},
		{"a pull-request review", plugins.RouteOrigin{Via: plugins.RouteViaPR}, routedItem{msg: plugins.Message{ID: 1}}, 4 * time.Hour},
		{"a task", plugins.RouteOrigin{}, routedItem{msg: human, task: notes.Note{ID: 9}}, 30 * time.Minute},
	} {
		if got := s.routeTimeout(c.origin, c.item); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
