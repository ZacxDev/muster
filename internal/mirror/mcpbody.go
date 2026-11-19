package mirror

import (
	"fmt"

	"github.com/ZacxDev/muster/internal/pages"
)

// THE MCP REQUEST BOUND — ONE RULE, EVERY HOP. Two things read an MCP request
// body on the way from an agent to a tool: muster's own MCP server
// (internal/api's serveMCP, POST /mcp and POST /mirror/mcp) and, for a
// mirrored session, its host's local MCP proxy (internal/mirrorhost's
// MCPProxy, in muster-mirror), which forwards the body to muster over plain
// HTTP. Both read it under MaxMCPRequestBytes, and muster then holds each
// tool to MCPRequestLimit.
//
// 🔴 WHY THE BOUND IS DERIVED FROM THE PAGE LIMIT. page_create and
// page_update document a page of up to pages.MaxVersionBytes (512 KiB) with
// its images inline as data: URIs. Both hops used to cap every request at a
// flat 64 KiB, so a page with a couple of screenshots was refused HTTP 413 by
// the host proxy (a ~440 KB page of 10 inline WebP screenshots, at af368efa)
// and unread by muster: the documented limit could not be reached at all.
// The body is read before the tool's name is known, so the read bound is the
// LARGEST input any tool documents — the blocks as a request may carry them
// (pages.MaxBlocksInputBytes, which says why that is twice the stored size)
// plus MCPToolInputBytes for everything else in the request (the JSON-RPC
// envelope, _meta, the title, page_id). A page past 512 KiB but inside that
// bound reaches the page tool and gets ITS refusal, not a transport one.
//
// Every other tool keeps the old tight bound: muster refuses a tools/call
// over MCPToolInputBytes for any tool but the two page writers, and any
// other method over it, AFTER the read (MCPRequestLimit) — so a large body
// costs at most one MaxMCPRequestBytes read and goes no further.
const (
	// MCPToolInputBytes bounds one MCP request for every method and tool but
	// page_create and page_update (the bound all of them had before).
	MCPToolInputBytes = 64 << 10
	// MaxMCPRequestBytes bounds one MCP request at every hop: 1088 KiB.
	MaxMCPRequestBytes = pages.MaxBlocksInputBytes + MCPToolInputBytes

	// MCPPageCreateTool and MCPPageUpdateTool are the two tools that take a
	// page's blocks (internal/api's page tools name themselves by these).
	MCPPageCreateTool = "page_create"
	MCPPageUpdateTool = "page_update"
)

// MCPRequestLimit is the most one MCP request of method (and, for a
// tools/call, tool) may carry.
func MCPRequestLimit(method, tool string) int {
	if method == "tools/call" && (tool == MCPPageCreateTool || tool == MCPPageUpdateTool) {
		return MaxMCPRequestBytes
	}
	return MCPToolInputBytes
}

// 🔴 A NEW HOST PROXY IN FRONT OF AN OLD muster (#833). A muster older than
// MCPBoundRelease read every MCP request under a flat 64 KiB and answered any
// read failure, the too-large one included, with exactly LegacyMCPUnreadCode /
// LegacyMCPUnreadMessage (HTTP 200, id null): a current muster-mirror forwards
// a page of 400 KB, and the old server's answer reads as a malformed request
// that names no hop and no limit (measured: 65,536 bytes read, 66,000 not, on
// a server built from a pre-bound commit behind a current muster-mirror). The
// host proxy recognises that exact reply to a request over LegacyMCPReadBytes
// and replaces it with MCPUnreadByOldMuster. muster no longer writes that
// message (internal/api's serveMCP names itself and the cause). A muster from
// MCPBoundRelease up to that change still writes it when a read fails for
// another reason (a connection cut mid-body), which is why the replacement
// says "probably".
const (
	// MCPBoundRelease is the first release whose muster reads a request
	// under MaxMCPRequestBytes (#733).
	MCPBoundRelease = "v0.239.0"
	// LegacyMCPReadBytes is what such a muster read of a request: a fixed
	// fact about old builds, its own constant so a later change to
	// MCPToolInputBytes cannot move the match or the words.
	LegacyMCPReadBytes     = 64 << 10
	LegacyMCPUnreadCode    = -32700
	LegacyMCPUnreadMessage = "could not read the request"
)

// MCPUnreadByOldMuster is the host proxy's answer when muster at server gave
// the legacy unread reply to a request of n bytes.
func MCPUnreadByOldMuster(server string, n int64) string {
	return fmt.Sprintf("muster at %s could not read this MCP request of %d bytes: it reads at most %d KiB per request, "+
		"so it is probably older than %s (from %s on, %s and %s take up to %d KiB); upgrade that muster, or keep the request under %d KiB",
		server, n, LegacyMCPReadBytes>>10, MCPBoundRelease, MCPBoundRelease, MCPPageCreateTool, MCPPageUpdateTool, MaxMCPRequestBytes>>10, LegacyMCPReadBytes>>10)
}

// MCPTooLarge is the refusal of a request over MaxMCPRequestBytes, read by
// who ("muster" or "muster-mirror on this host"): which side refused, and
// what the bounds are, so a mixed rollout says where it stopped.
func MCPTooLarge(who string) string {
	return fmt.Sprintf("%s refused this MCP request: it is larger than %d KiB, the most one request may carry "+
		"(%s and %s take a page whose blocks are at most %d KiB as stored; every other tool takes at most %d KiB)",
		who, MaxMCPRequestBytes>>10, MCPPageCreateTool, MCPPageUpdateTool, pages.MaxVersionBytes>>10, MCPToolInputBytes>>10)
}
