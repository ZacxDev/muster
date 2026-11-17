package api

import (
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

// WHO IS TOLD ABOUT ATTACHMENTS (round 1 Y1). `ccd ask-attach` ships only in
// the claude-code image, so only a claude-code agent is told to run it; any
// other kind is told attachments are not available to it. And while uploads
// are off, no attachment text is offered at all: not in the paragraph, not in
// attention_ask's schema.

// attachTextServer is a server with the attention tools on (a store that is
// never called) and uploads on or off.
func attachTextServer(t *testing.T, uploadsOn bool) *Server {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	ext := Extensions{Notes: stubNotesStore{}, Agents: stubAgentsStore{}, Attention: attention.NewPG(nil)}
	if uploadsOn {
		sg, err := uploads.NewSigner("attach-text-signing-key-0123456789abcdef")
		if err != nil {
			t.Fatal(err)
		}
		ext.Uploads = &uploads.Service{Store: &uploadstest.Store{}, Objects: &uploadstest.Objects{}, Signer: sg,
			PublicURL: "https://muster.example.test", MaxBytes: 1 << 20, LinkTTL: time.Hour}
	}
	s.UseExtensions(ext)
	return s
}

// askSchemaOf is attention_ask's parameter properties among defs, as JSON.
func askSchemaOf(t *testing.T, defs []agents.ToolDef) string {
	t.Helper()
	for _, d := range defs {
		if d.Name == attentionAskTool {
			b, _ := json.Marshal(d.Parameters)
			return string(b)
		}
	}
	t.Fatal("no attention_ask among the tools")
	return ""
}

func TestOnlyAClaudeCodeAgentIsToldToRunCcdAskAttach(t *testing.T) {
	s := attachTextServer(t, true)
	claude := agents.Agent{ID: 1, Name: "cc", Kind: agents.KindClaudeCode}
	gateway := agents.Agent{ID: 2, Name: "gw", Kind: "gateway"}
	if got := s.attentionInstructionFor(claude); !strings.Contains(got, "run `ccd ask-attach <path>`") {
		t.Errorf("a claude-code agent is not told how to attach:\n%s", got)
	}
	got := s.attentionInstructionFor(gateway)
	if strings.Contains(got, "ccd") || !strings.Contains(got, "Attachments are not available to you") {
		t.Errorf("a gateway agent's paragraph:\n%s", got)
	}
	if !strings.Contains(got, attentionInstruction) {
		t.Errorf("a gateway agent lost the attention paragraph itself:\n%s", got)
	}
	// The refusal of paths that reach muster says the same, per kind.
	if got := s.askPathsRefusal(callerOfAgent(claude)); !strings.Contains(got, "run `ccd ask-attach <path>`") {
		t.Errorf("a claude-code agent's paths refusal: %s", got)
	}
	if got := s.askPathsRefusal(callerOfAgent(gateway)); strings.Contains(got, "ccd") || !strings.Contains(got, "Attachments are not available to you") {
		t.Errorf("a gateway agent's paths refusal: %s", got)
	}
	// With uploads off, the refusal is that, for any kind.
	if got := attachTextServer(t, false).askPathsRefusal(callerOfAgent(claude)); strings.Contains(got, "ccd") || !strings.Contains(got, "file attachments are off") {
		t.Errorf("a claude-code agent's paths refusal with uploads off: %s", got)
	}
	// The schema every agent is offered names no ccd verb.
	if schema := askSchemaOf(t, s.attentionToolDefs()); strings.Contains(schema, "ccd") || !strings.Contains(schema, `"attachment_ids"`) {
		t.Errorf("attention_ask's schema with uploads on: %s", schema)
	}
}

func TestWhileUploadsAreOffNoAttachmentTextIsOffered(t *testing.T) {
	s := attachTextServer(t, false)
	for _, a := range []agents.Agent{{ID: 1, Name: "cc", Kind: agents.KindClaudeCode}, {ID: 2, Name: "gw", Kind: "gateway"}} {
		got := s.attentionInstructionFor(a)
		for _, w := range []string{"attach", "ccd"} {
			if strings.Contains(strings.ToLower(got), w) {
				t.Errorf("%s with uploads off is told %q:\n%s", a.Kind, w, got)
			}
		}
		if !strings.Contains(got, attentionInstruction) {
			t.Errorf("%s lost the attention paragraph:\n%s", a.Kind, got)
		}
	}
	schema := askSchemaOf(t, s.attentionToolDefs())
	if strings.Contains(schema, "attachment") || strings.Contains(schema, "ccd") {
		t.Errorf("attention_ask's schema with uploads off offers attachments: %s", schema)
	}
	if !strings.Contains(schema, `"context"`) {
		t.Errorf("attention_ask's schema lost context: %s", schema)
	}
	// The mirrored sessions' paragraph: no attachment line either way of
	// runtime while uploads are off; Claude Code's has it while they are on.
	for _, oc := range []bool{false, true} {
		if got := s.mirrorAttentionInstructionFor(oc); strings.Contains(got, "attach") {
			t.Errorf("mirrored (opencode %v) with uploads off: %s", oc, got)
		}
	}
	if got := attachTextServer(t, true).mirrorAttentionInstructionFor(false); !strings.Contains(got, "pass attachments:") {
		t.Errorf("mirrored Claude Code with uploads on: %s", got)
	}

}
