package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
)

// defaultAgentCLIPath is where the Dockerfile puts the agent-side muster.
// Overridden per-Server in tests; there is no env knob, because the only thing
// that may legitimately vary this is the image layout, and that is set in the
// same repo as this constant.
//
// ⚠ THE "SAME REPO" HALF OF THAT SENTENCE WAS AN ASPIRATION WHEN IT WAS WRITTEN
// AND IS A FACT NOW. There was no Dockerfile in this repository at all for the
// whole of the carve, so "the image layout is set in the same repo" named a file
// that did not exist and a path nothing produced. `/Dockerfile`'s final stage
// now copies the CLI here, built from this same source tree.
//
// 🔴 SAME SOURCE, NOT SAME VERSION — AND THIS COMMENT CLAIMED OTHERWISE. It used
// to say the CLI was built "from the same VERSION as the server", which made the
// served binary and the serving server "the same build by construction". The
// source is shared; the stamp is not. Dockerfile links the server with
// `-X …/internal/api.BuildVersion=${VERSION}` and links this CLI with
// `-ldflags="-s -w"` — no `-X` — so the served binary reports
// `main.buildVersion`'s default, `dev`, whatever ${VERSION} the image was built
// with.
const defaultAgentCLIPath = "/agent-cli/muster"

// handleAgentCLIDownload serves GET /agent/muster: the statically-linked
// muster binary a dispatched agent fetches at startup, so it can work its
// task with `muster agent task …` rather than hand-rolled curl.
//
// 🔴 Gated by requireAgentToken, the SAME middleware as /agent/task* — not the
// plain hook token. Two reasons, and the second is the load-bearing one:
//   - only a real provisioned agent has any business pulling this, and
//   - requireHookToken is ENFORCE-WHEN-SET, so an empty MUSTER_HOOK_TOKEN
//     leaves a hook-token gate wide open, on the LAN NodePort included. The
//     routes this binary exists to call are on requireAgentToken, which resolves
//     a real agent every time; gating the binary more weakly than its callers is
//     the asymmetry to avoid.
//
// It is a READ of a file baked into the image at build time. Nothing user-
// supplied reaches the path: it is a constant, so there is no traversal surface
// (no path parameter, no query, no header is consulted).
//
// A missing file answers 503 with a reason rather than a 200 with an empty body.
// That distinction matters more than it looks: the consumer is a `curl -sf` in a
// pod's startup script, and an empty 200 would be written to disk, marked
// executable, and fail later as "not found"-shaped noise a long way from here.
// 503 makes `curl -sf` fail loudly at the fetch.
func (s *Server) handleAgentCLIDownload(w http.ResponseWriter, r *http.Request) {
	path := s.agentCLIPath
	if path == "" {
		path = defaultAgentCLIPath
	}
	f, err := os.Open(path)
	if err != nil {
		// Distinguish "this build has no CLI" (dev/`go run`, the expected case)
		// from a real I/O fault, so the log says which.
		if errors.Is(err, os.ErrNotExist) {
			s.logger.Printf("agent cli: %s is not in this image; the route is unavailable", path)
		} else {
			s.logger.Printf("agent cli: open %s: %v", path, err)
		}
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the agent CLI is not available in this build",
		})
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		s.logger.Printf("agent cli: stat %s: %v", path, err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the agent CLI is not available in this build",
		})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	// 🔴 THIS HEADER DESCRIBES THE SERVER, NOT THE SERVED BYTES. BuildVersion is
	// THIS process's version, stamped by Dockerfile's server build; the binary in
	// the body is built from the same source tree but with no `-X`, so running it
	// and asking `--version` answers `dev` and will not match this header. It is
	// still worth sending — the two come from one tree and one image, so it
	// identifies the build the bytes came FROM — but it is not a report of what
	// the downloaded binary will say about itself. The comment here previously
	// read "the version the bytes were built at", which is the claim this
	// paragraph corrects.
	w.Header().Set("X-Muster-Version", BuildVersion)
	w.WriteHeader(http.StatusOK)

	// Copy errors are logged, not written: the header block is already sent, so
	// there is no way left to signal failure in-band. A truncated body then fails
	// the `curl -sf` on Content-Length mismatch, which is the correct outcome.
	if _, err := io.Copy(w, f); err != nil {
		s.logger.Printf("agent cli: streaming %s: %v", path, err)
	}
}
