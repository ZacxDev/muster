package mirror

import (
	"strings"
	"testing"
)

// TestTheMCPRequestBoundFitsTheDocumentedPage pins the bound as literals: a
// page's 512 KiB of blocks, twice over for a caller's encoder, plus the 64
// KiB every other request had. Only the two page writers' tools/call may use
// it; every other tool and method keeps 64 KiB.
func TestTheMCPRequestBoundFitsTheDocumentedPage(t *testing.T) {
	if MaxMCPRequestBytes != 1114112 || MCPToolInputBytes != 65536 {
		t.Fatalf("MaxMCPRequestBytes %d, MCPToolInputBytes %d", MaxMCPRequestBytes, MCPToolInputBytes)
	}
	for _, c := range []struct {
		method, tool string
		want         int
	}{
		{"tools/call", "page_create", 1114112},
		{"tools/call", "page_update", 1114112},
		{"tools/call", "page_get", 65536},
		{"tools/call", "task_comment", 65536},
		{"tools/call", "", 65536},
		{"tools/list", "page_create", 65536},
		{"initialize", "", 65536},
	} {
		if got := MCPRequestLimit(c.method, c.tool); got != c.want {
			t.Errorf("MCPRequestLimit(%q, %q) = %d, want %d", c.method, c.tool, got, c.want)
		}
	}
	msg := MCPTooLarge("muster-mirror on this host")
	for _, want := range []string{"muster-mirror on this host refused this MCP request", "1088 KiB", "page_create and page_update", "512 KiB", "64 KiB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("MCPTooLarge lacks %q: %s", want, msg)
		}
	}
}
