package main

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/podagent"
)

// GET /v1/usage: THIS POD'S CLAUDE USAGE, PER SESSION AND UTC HOUR, counted
// from the CLI's own transcripts (cctranscript.UsagePage).
//
// muster-server's officer usage collector (internal/officers/usage) reads it
// and pushes it to the officer's HQ. Bearer-authenticated like
// /v1/transcript. The answer carries counts only: no transcript text is ever
// in it, nor in a log line.
//
// Query: since=<unix seconds>, required. It is TRUNCATED DOWN to the UTC hour
// and clamped to at most usageMaxLookback ago (on an hour boundary too), so
// the oldest bucket is always a whole hour: HQ replaces a bucket on every
// push, and a partial oldest hour would overwrite a complete one.
//
// What is read: every *.jsonl under <CLAUDE_CONFIG_DIR>/projects/, at any
// depth, last modified at or after since (an older file cannot hold a newer
// record). A file's SESSION is the first path element under its project
// directory, `.jsonl` stripped: <project>/<session>.jsonl is the session's own
// transcript, and <project>/<session>/subagents/<x>.jsonl a subagent's,
// counted on the session that ran it.
//
// What is counted, per file (usageOfFile): complete lines only (a trailing
// line still being written is left for the next read), decoded with a LEAN
// struct (usageRecord: never the content); a line that does not decode, or is
// over maxTranscriptLine, is skipped.
//
//   - API calls and tokens: assistant records, subagent (sidechain) records
//     INCLUDED — a subagent's calls are this instance's spend — each call
//     counted once by callLedger, at the hour of its first record, with its
//     largest value of each token field.
//   - Turns: distinct non-empty promptIds of NON-sidechain user records, at
//     the hour first seen. A subagent's prompt is the parent's tool call, not
//     a turn.
//
// A record's hour is its own timestamp's; a record with none is skipped.
// Every record of a file is read, those before since included, so a call or
// a prompt first seen before since stays in the hour it began (outside the
// answer) rather than reappearing in a later one.

// usageMaxLookback bounds how far back GET /v1/usage reads.
const usageMaxLookback = 48 * time.Hour

func (s *server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.Bearer) {
		writeFailure(w, &failure{Status: http.StatusUnauthorized, Type: failUnauthorized, Message: "missing or wrong bearer"})
		return
	}
	n, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil || n < 0 {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "since must be a time in unix seconds"})
		return
	}
	since := usageWindowStart(time.Unix(n, 0), s.clock())
	page := cctranscript.UsagePage{Since: since, Buckets: hourlyUsage(filepath.Join(s.cfg.ConfigDir, "projects"), since)}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// usageWindowStart is since truncated DOWN to the UTC hour, and no earlier
// than the first whole hour within usageMaxLookback of now.
func usageWindowStart(since, now time.Time) time.Time {
	start := since.UTC().Truncate(time.Hour)
	floor := now.UTC().Add(-usageMaxLookback)
	if t := floor.Truncate(time.Hour); t.Before(floor) {
		floor = t.Add(time.Hour)
	}
	if start.Before(floor) {
		return floor
	}
	return start
}

// bucketKey is one bucket's identity.
type bucketKey struct {
	session string
	hour    time.Time
}

// hourlyUsage counts every transcript under projects modified at or after
// since into buckets of hour >= since (the route's doc).
func hourlyUsage(projects string, since time.Time) []cctranscript.UsageBucket {
	acc := map[bucketKey]*cctranscript.UsageBucket{}
	prompts := map[string]bool{} // session + "\x00" + promptId already counted
	_ = filepath.WalkDir(projects, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != projects {
				return fs.SkipDir // an unreadable directory is skipped, not fatal
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		rel, err := filepath.Rel(projects, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 2 {
			return nil // directly under projects/: no project, no session
		}
		session := strings.TrimSuffix(parts[1], ".jsonl")
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(since) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		usageOfFile(f, session, since, acc, prompts)
		return nil
	})
	out := make([]cctranscript.UsageBucket, 0, len(acc))
	for _, b := range acc {
		if b.APICalls == 0 && b.Turns == 0 {
			continue
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Hour.Equal(out[j].Hour) {
			return out[i].Hour.Before(out[j].Hour)
		}
		return out[i].Session < out[j].Session
	})
	return out
}

// usageRecord is the LEAN decode of one transcript line: only what the
// hourly count reads. The content — most of a transcript's bytes — is never
// decoded.
type usageRecord struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	PromptID    string `json:"promptId"`
	IsSidechain bool   `json:"isSidechain"`
	IsAPIError  bool   `json:"isApiErrorMessage"`
	Message     struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage *podagent.Usage `json:"usage"`
	} `json:"message"`
}

// usageOfFile adds one transcript file of session to acc (buckets of hour >=
// since only). prompts is the scan's prompt set, shared across the session's
// files.
func usageOfFile(r io.Reader, session string, since time.Time, acc map[bucketKey]*cctranscript.UsageBucket, prompts map[string]bool) {
	bucket := func(hour time.Time) *cctranscript.UsageBucket {
		if hour.Before(since) {
			return nil
		}
		k := bucketKey{session, hour}
		b := acc[k]
		if b == nil {
			b = &cctranscript.UsageBucket{Session: session, Hour: hour}
			acc[k] = b
		}
		return b
	}
	var calls callLedger
	callHour := map[string]time.Time{}
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if err != nil {
			break // io.EOF (the unterminated tail is ignored) or a read error
		}
		if len(line) == 0 {
			continue
		}
		var rec usageRecord
		if json.Unmarshal(line, &rec) != nil || rec.Timestamp == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
		if err != nil {
			continue
		}
		hour := at.UTC().Truncate(time.Hour)
		switch rec.Type {
		case "assistant":
			key, first, counted := calls.add(rec.Message.ID, rec.Message.Model, rec.IsAPIError, rec.Message.Usage)
			if counted && first {
				callHour[key] = hour
			}
		case "user":
			if rec.IsSidechain || rec.PromptID == "" {
				continue
			}
			k := session + "\x00" + rec.PromptID
			if prompts[k] {
				continue
			}
			prompts[k] = true
			if b := bucket(hour); b != nil {
				b.Turns++
			}
		}
	}
	for _, key := range calls.keys() {
		b := bucket(callHour[key])
		if b == nil {
			continue
		}
		u := calls.call(key)
		b.APICalls++
		b.InputTokens += u.InputTokens
		b.OutputTokens += u.OutputTokens
		b.CacheReadTokens += u.CacheReadInputTokens
		b.CacheCreationTokens += u.CacheCreationInputTokens
	}
}
