package cctranscript

// Page is one incremental read of a session's transcript, as ccd serves it on
// GET /v1/transcript (cmd/ccd/transcript_route.go) and muster-server's officer
// transcript forwarder reads it (internal/officers/transcript).
//
//   - Session is the CLI session id whose transcript this is. A caller that
//     sent another session's offset was served this one from its start.
//   - Next is the offset to read from next: always a line boundary
//     (ReadLines), never past a partial last line.
//   - Lines are the complete lines read, each with the events it produced, its
//     end offset and its own time.
type Page struct {
	Session string `json:"session"`
	Next    int64  `json:"next"`
	Lines   []Line `json:"lines"`
}
