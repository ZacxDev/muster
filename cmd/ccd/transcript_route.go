package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// GET /v1/transcript: THE PRIMARY CLI'S OWN TRANSCRIPT, INCREMENTALLY.
//
// muster-server's officer transcript forwarder (internal/officers/transcript)
// polls it to forward what an officer instance is doing to HQ while it holds
// an item. It is the same structured record every other reader of a session
// uses — the CLI's transcript JSONL, decoded by internal/cctranscript
// (ReadLines / Backfill: complete lines only, a partial last line left for the
// next poll) — never the screen.
//
// Query:
//
//   - after: the offset to read from (a Page.Next this route returned). -1 is
//     the TAIL: the last `backfill` bytes (0: none — start live at the end).
//   - backfill: the tail's window in bytes, at most transcriptBackfillMax.
//   - session: the session the caller's offset belongs to. When the primary is
//     now in another session (a new conversation), or the offset is past the
//     file's end (the file was replaced), the read starts at 0 of the current
//     one: every line of a conversation that began after the caller's last
//     read is new to it.
//
// The answer is a cctranscript.Page. Bearer-authenticated like /v1/responses:
// it serves tool output, so it is exactly as private as a turn's reply.
//
// The PRIMARY's CLI, found by its own pid and ITS sessions file
// (cliSessionOf), never by the last hook's session: an officer's turns (its
// first turn and every re-prompt) carry no lane header, so they run in the
// primary. A lane's transcript is not served.

const (
	// transcriptReadMax bounds one forward read.
	transcriptReadMax = 1 << 20
	// transcriptBackfillMax bounds a tail's window.
	transcriptBackfillMax = 1 << 20
	// transcriptBackfillEvents bounds a tail's events.
	transcriptBackfillEvents = 2000
)

func (s *server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.Bearer) {
		writeFailure(w, &failure{Status: http.StatusUnauthorized, Type: failUnauthorized, Message: "missing or wrong bearer"})
		return
	}
	q := r.URL.Query()
	after := int64(0)
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < -1 {
			writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "after must be an offset, or -1 for the tail"})
			return
		}
		after = n
	}
	backfill := 0
	if v := q.Get("backfill"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "backfill must be a byte count"})
			return
		}
		backfill = min(n, transcriptBackfillMax)
	}
	cs, path, ok := cliSessionOf(s.cfg.ConfigDir, procRoot, s.cliPIDOf(r.Context()))
	if !ok || path == "" {
		writeFailure(w, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: "no running CLI session with a transcript"})
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeFailure(w, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: "the session's transcript cannot be opened"})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeFailure(w, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: "the session's transcript cannot be read"})
		return
	}
	size := st.Size()
	if sess := q.Get("session"); (sess != "" && sess != cs.SessionID) || after > size {
		after = 0
	}
	page := cctranscript.Page{Session: cs.SessionID, Lines: []cctranscript.Line{}}
	switch {
	case after < 0 && backfill == 0:
		page.Next, err = lastLineEnd(f, size)
	case after < 0:
		var lines []cctranscript.Line
		lines, page.Next, err = cctranscript.Backfill(f, size, backfill, transcriptBackfillEvents)
		page.Lines = append(page.Lines, lines...)
	default:
		var lines []cctranscript.Line
		lines, page.Next, err = cctranscript.ReadLines(f, after, size, transcriptReadMax)
		page.Lines = append(page.Lines, lines...)
	}
	if err != nil {
		writeFailure(w, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: "the session's transcript cannot be read"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// lastLineEnd is the offset just past the last complete line of r (size
// bytes): where a live tail starts, so a record still being written is read
// whole on the next poll rather than skipped from its middle.
func lastLineEnd(r io.ReaderAt, size int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for end := size; end > 0; {
		start := max(end-int64(len(buf)), 0)
		got, err := r.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:got], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}
