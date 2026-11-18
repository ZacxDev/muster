package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// asAdmin is ctx (the MCP request's, carrying its image collector) under the
// fixture's admin principal: serveSurface's request carries none.
func asAdmin(ctx context.Context) context.Context {
	return authz.WithPrincipal(ctx, authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
}

// ihPNGBytes is a PNG signature then filler: what http.DetectContentType
// sniffs as image/png. A fixture, not a real image.
var ihPNGBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{4, 5, 6, 0xfd}, 500)...)

// ihMCPContent is a tools/call result's content blocks.
type ihMCPContent struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MIMEType string `json:"mimeType"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// TestAScreenshotComesBackAsAnImageOverMCP: over the MCP door, a JSON
// response whose png_base64 field holds an image is answered with the rest of
// the JSON as text and the image as an MCP image content block — not as
// megabytes of base64 text. Any other JSON response is the one text block it
// always was, and the in-process gateway runtime (no image collector) still
// gets the whole answer as text.
func TestAScreenshotComesBackAsAnImageOverMCP(t *testing.T) {
	f := newIHToolFixture(t, "approved")
	body := `{"session":"abc-1","png_base64":"` + base64.StdEncoding.EncodeToString(ihPNGBytes) + `"}`
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/plain" {
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(svc.Close)
	f.gs.repo = f.reconfigure(t, svc.URL).Destination()

	var defs []agents.ToolDef
	for _, d := range connectorToolDefs() {
		if d.Name == internalHTTPToolName {
			defs = append(defs, d)
		}
	}
	surface := mcpSurface{tools: func() []agents.ToolDef { return defs },
		call: func(ctx context.Context, name, args string) string {
			return f.s.toolInternalHTTP(asAdmin(ctx), f.a, args)
		}}
	call := func(path string) ihMCPContent {
		t.Helper()
		r := serveSurface(t, surface, mcpToolCall(internalHTTPToolName, map[string]any{"connector": "app-driver", "method": "GET", "path": path}))
		var c ihMCPContent
		if err := json.Unmarshal(r.Result, &c); err != nil {
			t.Fatalf("result: %v: %s", err, r.Result)
		}
		return c
	}

	got := call("/v1/sessions/abc-1/screenshot")
	if got.IsError || len(got.Content) != 2 || got.Content[0].Type != "text" || got.Content[1].Type != "image" {
		t.Fatalf("the screenshot call answered %d blocks %+v, want text then image", len(got.Content), got.Content)
	}
	if img, err := base64.StdEncoding.DecodeString(got.Content[1].Data); err != nil || !bytes.Equal(img, ihPNGBytes) ||
		got.Content[1].MIMEType != "image/png" {
		t.Fatalf("the image block is not the screenshot: %s (%v)", got.Content[1].MIMEType, err)
	}
	var res struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
		Images int    `json:"images"`
	}
	if err := json.Unmarshal([]byte(got.Content[0].Text), &res); err != nil || res.Status != 200 || res.Images != 1 {
		t.Fatalf("the text block: %v %s", err, got.Content[0].Text)
	}
	if strings.Contains(res.Body, "iVBORw0KGgo") || !strings.Contains(res.Body, `"session":"abc-1"`) ||
		!strings.Contains(res.Body, "[image 1 of this result: image/png, 2008 bytes, returned as an image]") {
		t.Fatalf("the text block's body: %s", res.Body)
	}

	// Any other JSON response: one text block, as before.
	if plain := call("/v1/plain"); len(plain.Content) != 1 || plain.Content[0].Type != "text" ||
		!strings.Contains(plain.Content[0].Text, `{\"ok\":true}`) {
		t.Fatalf("a plain JSON response answered %+v, want its one text block", plain.Content)
	}

	// The gateway runtime has no image collector: the whole answer is text.
	text := f.s.toolInternalHTTP(f.ctx, f.a, `{"connector":"app-driver","method":"GET","path":"/v1/sessions/abc-1/screenshot"}`)
	if !strings.Contains(text, "iVBORw0KGgo") || strings.Contains(text, `"images"`) {
		t.Fatalf("without an image collector the base64 must stay in the text: %.200s", text)
	}
}
