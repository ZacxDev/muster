package mirror

import (
	"slices"
	"strings"
	"testing"
)

// TestAHelloOffersOpenCodeOnlyBesideTheSend: the hub keeps opencode from a
// hello only when the same hello advertises the gated send (Msg.OpenCodeSend):
// the spawn posts the first prompt through that API. claude is kept either
// way; a read-only host's hello offers nothing.
func TestAHelloOffersOpenCodeOnlyBesideTheSend(t *testing.T) {
	both := []string{SpawnRuntimeClaude, SpawnRuntimeOpenCode, "sh", SpawnRuntimeClaude}
	for _, c := range []struct {
		interactive, send bool
		want              []string
	}{
		{true, true, []string{"claude", "opencode"}},
		{true, false, []string{"claude"}},
		{false, true, nil},
	} {
		got := spawnRuntimesOf(Msg{Interactive: c.interactive, OpenCodeSend: c.send, AgentSpawn: both})
		if !slices.Equal(got, c.want) {
			t.Fatalf("interactive %v send %v: %q; want %q", c.interactive, c.send, got, c.want)
		}
	}
}

// TestTheSpawnModelIsReadAsOpenCodeReadsIt: the wire's openrouter/<vendor>/<model>
// splits at its FIRST '/' (Model.Ref.parse): provider openrouter, id
// <vendor>/<model>. "a/b/c" is provider "a" and refused; an id outside the
// shape or past its bound is refused; the server's id becomes the wire's.
func TestTheSpawnModelIsReadAsOpenCodeReadsIt(t *testing.T) {
	for _, c := range []struct {
		wire, provider, id string
		ok                 bool
	}{
		{"openrouter/qwen/qwen3-coder", "openrouter", "qwen/qwen3-coder", true},
		{"openrouter/moonshotai/kimi-k2-0905", "openrouter", "moonshotai/kimi-k2-0905", true},
		{"openrouter/meta-llama/llama-3.3-70b-instruct:free", "openrouter", "meta-llama/llama-3.3-70b-instruct:free", true},
		{"a/b/c", "a", "b/c", false},
		{"openrouter/qwen", "openrouter", "qwen", false},
		{"openrouter/qwen/a/b", "openrouter", "qwen/a/b", false},
		{"openrouter/Qwen/x", "openrouter", "Qwen/x", false},
		{"openrouter/-x/y", "openrouter", "-x/y", false},
		{"openrouter/x/y;z", "openrouter", "x/y;z", false},
		{"openrouter/x/" + strings.Repeat("y", MaxSpawnModel), "openrouter", "x/" + strings.Repeat("y", MaxSpawnModel), false},
		{"", "", "", false},
	} {
		p, id, ok := ParseSpawnModel(c.wire)
		if p != c.provider || id != c.id || ok != c.ok {
			t.Fatalf("%q: %q %q %v; want %q %q %v", c.wire, p, id, ok, c.provider, c.id, c.ok)
		}
	}
	if w := SpawnModelWire("z-ai/glm-4.6"); w != "openrouter/z-ai/glm-4.6" {
		t.Fatalf("the wire form: %q", w)
	}
}
