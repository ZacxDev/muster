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
// How a file is read (usageScan.file): complete lines only (a trailing line
// still being written is left for the next read), decoded with a LEAN struct
// (usageRecord: never the content); a line that does not decode, or is over
// maxTranscriptLine, is skipped.
//
// 🔴 ONE LEDGER FOR THE WHOLE SCAN, NOT ONE PER FILE. The same API call and
// the same prompt are written into MORE THAN ONE file: a forked subagent's
// transcript, a branched or resumed session copy the records they continue
// from. So every call and every prompt is counted once across all the files
// read, and is OWNED — its session and its hour — by its first record in
// scan order. The files are read in SORTED relative-path order, so the owner
// is the same on every scan: HQ replaces a bucket on every push, and an owner
// that moved between scans would move counts between sessions.
//
//   - API calls and tokens: assistant records, subagent (sidechain) records
//     INCLUDED — a subagent's calls are this instance's spend — each call
//     counted once by callLedger, with its largest value of each token field
//     over every copy.
//   - Model: per bucket, the model (message.model, which every record of a
//     call that names one repeats) with the most output tokens over the bucket's calls; a tie
//     goes to the smaller model id, so the answer does not depend on file or
//     record order. No call naming a model: none (bucketModel).
//   - Turns: distinct non-empty promptIds of user records that are not a
//     subagent's (isSidechain: the parent's tool call, not a turn) and not
//     injected context (isMeta, isCompactSummary).
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

// usageFile is one transcript to read: its path relative to projects/ (the
// sort key), its path on disk, and its session.
type usageFile struct {
	rel, path, session string
}

// hourlyUsage counts every transcript under projects modified at or after
// since into buckets of hour >= since (the route's doc).
func hourlyUsage(projects string, since time.Time) []cctranscript.UsageBucket {
	var files []usageFile
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
		rel = filepath.ToSlash(rel)
		parts := strings.Split(rel, "/")
		if len(parts) < 2 {
			return nil // directly under projects/: no project, no session
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(since) {
			return nil
		}
		files = append(files, usageFile{rel: rel, path: path, session: strings.TrimSuffix(parts[1], ".jsonl")})
		return nil
	})
	// The owner of a record found in several files is the first file in THIS
	// order (the route's doc): plain string order of the relative path, not the
	// walk's, which visits <session>/ before <session>.jsonl.
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	sc := usageScan{since: since, acc: map[bucketKey]*cctranscript.UsageBucket{}, callOwner: map[string]bucketKey{},
		callModel: map[string]string{}, prompts: map[string]bool{}}
	for _, f := range files {
		if r, err := os.Open(f.path); err == nil {
			sc.file(r, f.session)
			r.Close()
		}
	}
	return sc.buckets()
}

// usageRecord is the LEAN decode of one transcript line: only what the
// hourly count reads. The content — most of a transcript's bytes — is never
// decoded.
type usageRecord struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	PromptID         string `json:"promptId"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	IsAPIError       bool   `json:"isApiErrorMessage"`
	Message          struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage *podagent.Usage `json:"usage"`
	} `json:"message"`
}

// usageScan is one GET /v1/usage's count, across every file it reads.
type usageScan struct {
	since time.Time
	acc   map[bucketKey]*cctranscript.UsageBucket
	// calls is THE ledger for the whole scan; callOwner is each call's
	// (session, hour), from its first record in scan order.
	calls     callLedger
	callOwner map[string]bucketKey
	// callModel is each call's model, as its records name it.
	callModel map[string]string
	// prompts is every promptId already counted, in any session.
	prompts map[string]bool
}

// bucket is the bucket k, or nil when its hour is before the window.
func (sc *usageScan) bucket(k bucketKey) *cctranscript.UsageBucket {
	if k.hour.Before(sc.since) {
		return nil
	}
	b := sc.acc[k]
	if b == nil {
		b = &cctranscript.UsageBucket{Session: k.session, Hour: k.hour}
		sc.acc[k] = b
	}
	return b
}

// file reads one transcript of session into the scan.
func (sc *usageScan) file(r io.Reader, session string) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if err != nil {
			return // io.EOF (the unterminated tail is ignored) or a read error
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
		own := bucketKey{session, at.UTC().Truncate(time.Hour)}
		switch rec.Type {
		case "assistant":
			key, first, counted := sc.calls.add(rec.Message.ID, rec.Message.Model, rec.IsAPIError, rec.Message.Usage)
			if counted && first {
				sc.callOwner[key] = own
			}
			if counted && rec.Message.Model != "" {
				sc.callModel[key] = rec.Message.Model
			}
		case "user":
			if rec.IsSidechain || rec.IsMeta || rec.IsCompactSummary || rec.PromptID == "" || sc.prompts[rec.PromptID] {
				continue
			}
			sc.prompts[rec.PromptID] = true
			if b := sc.bucket(own); b != nil {
				b.Turns++
			}
		}
	}
}

// buckets adds every call to its owner's bucket and answers the non-empty
// buckets, sorted by hour then session.
func (sc *usageScan) buckets() []cctranscript.UsageBucket {
	models := map[bucketKey]map[string]int64{}
	for _, key := range sc.calls.keys() {
		own := sc.callOwner[key]
		b := sc.bucket(own)
		if b == nil {
			continue
		}
		u := sc.calls.call(key)
		if m := sc.callModel[key]; m != "" {
			if models[own] == nil {
				models[own] = map[string]int64{}
			}
			models[own][m] += u.OutputTokens
		}
		b.APICalls++
		b.InputTokens += u.InputTokens
		b.OutputTokens += u.OutputTokens
		b.CacheReadTokens += u.CacheReadInputTokens
		b.CacheCreationTokens += u.CacheCreationInputTokens
	}
	out := make([]cctranscript.UsageBucket, 0, len(sc.acc))
	for k, b := range sc.acc {
		if b.APICalls == 0 && b.Turns == 0 {
			continue
		}
		b.Model = bucketModel(models[k])
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

// bucketModel is THE rule for a bucket's model: of the output tokens per
// model, the model with the most; a tie goes to the smaller model id. None
// for no model.
func bucketModel(out map[string]int64) string {
	best, bestOut := "", int64(-1)
	for m, n := range out {
		if n > bestOut || (n == bestOut && m < best) {
			best, bestOut = m, n
		}
	}
	return best
}
