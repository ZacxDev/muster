// Command muster-toolctx-calibrate measures how well the rendered view's
// tool-context ESTIMATE (cctranscript.EstimateTokens) matches the MEASURED
// growth, over a directory of Claude Code transcripts, and fits the
// calibration constant the estimate uses (cctranscript.ResultBytesPerToken).
//
// 🔴 IT PRINTS AGGREGATES ONLY — counts, medians, percentiles. No transcript
// text, path, tool name or id is ever printed, and nothing is written. Nothing
// in CI runs it (it has no tests; `go test ./...` only compiles it), and its
// input is whatever directory it is pointed at:
//
//	go run ./cmd/muster-toolctx-calibrate [-dir ~/.claude/projects]
//
// THE SAMPLES are clean single-call gaps — exactly what the ledger labels
// CostMeasured: one call, its result, and the next turn, with nothing else in
// between. For each: the measured growth (next turn's context − the issuing
// turn's), the issuing turn's output_tokens, and the call's and result's sizes
// (Event.Bytes). Transcripts are split by a hash of their path into a FIT half
// and a HELD-OUT half: the constant is fitted on the first, and the error is
// reported on the second, for the old estimate and the new one.
//
// THE MODEL is a split, not one factor: growth = the turn's own output (which
// re-enters the context in full) + the result's tokens. The result's share is
// fitted as bytes per token: the median of resultBytes / (growth − output) over
// the fit half's samples whose result is big enough to measure (≥ minBytes).
package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// minBytes is the smallest result the fit reads a rate from: below it, the
// result's few tokens drown in the turn's own variation.
const minBytes = 400

type sample struct {
	delta, output, in, out int64
	held                   bool
}

func main() {
	home, _ := os.UserHomeDir()
	dir := flag.String("dir", filepath.Join(home, ".claude", "projects"), "the transcripts' directory (read, never written)")
	flag.Parse()
	samples, files, err := collect(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "muster-toolctx-calibrate:", err)
		os.Exit(1)
	}
	report(samples, files)
}

// collect walks dir's main-session transcripts (one level of project
// directories; a subagent's transcripts, deeper, are skipped) and returns every
// clean single-call sample.
func collect(dir string) ([]sample, int, error) {
	var out []sample
	files := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= 1 {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		s, err := fileSamples(p)
		if err != nil {
			return nil // unreadable: skipped, not fatal
		}
		files++
		sum := sha256.Sum256([]byte(p))
		for i := range s {
			s[i].held = sum[0]&1 == 1
		}
		out = append(out, s...)
		return nil
	})
	return out, files, err
}

func fileSamples(p string) ([]sample, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var (
		led             cctranscript.ToolContext
		lastOut         int64
		in, res, output = map[string]int64{}, map[string]int64{}, map[string]int64{}
		out             []sample
	)
	for off := int64(0); off < st.Size(); {
		lines, next, err := cctranscript.ReadLines(f, off, st.Size(), 8<<20)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if next == off {
			break
		}
		off = next
		for _, ev := range cctranscript.Flatten(lines) {
			if ev.Usage != nil {
				lastOut = ev.Usage.Output
			}
			switch ev.Kind {
			case cctranscript.KindToolCall:
				in[ev.ToolID], output[ev.ToolID] = int64(ev.Bytes), lastOut
			case cctranscript.KindToolResult:
				res[ev.ToolID] = int64(ev.Bytes)
			}
			for _, c := range led.Observe(ev) {
				if c.Kind == cctranscript.CostMeasured && output[c.ToolID] > 0 {
					out = append(out, sample{delta: c.Tokens, output: output[c.ToolID], in: in[c.ToolID], out: res[c.ToolID]})
				}
				delete(in, c.ToolID)
				delete(res, c.ToolID)
				delete(output, c.ToolID)
			}
		}
	}
	return out, nil
}

func report(all []sample, files int) {
	var fit, held []sample
	for _, s := range all {
		if s.held {
			held = append(held, s)
		} else {
			fit = append(fit, s)
		}
	}
	fmt.Printf("transcripts: %d · clean single-call samples: %d (fit %d, held out %d)\n", files, len(all), len(fit), len(held))

	// The split's premise: the turn's output re-enters the context in full.
	var rest, below []float64
	for _, s := range fit {
		r := float64(s.delta - s.output)
		rest = append(rest, r)
		if r < 0 {
			below = append(below, r)
		}
	}
	fmt.Printf("fit: growth − output: median %.0f, p10 %.0f · negative in %.1f%% of samples\n",
		pct(rest, 50), pct(rest, 10), 100*float64(len(below))/float64(len(rest)))

	var rates []float64
	for _, s := range fit {
		if r := s.delta - s.output; r > 0 && s.out >= minBytes {
			rates = append(rates, float64(s.out)/float64(r))
		}
	}
	k := pct(rates, 50)
	fmt.Printf("fit: result bytes per token (results ≥ %d B, n=%d): median %.2f, p25 %.2f, p75 %.2f\n",
		minBytes, len(rates), k, pct(rates, 25), pct(rates, 75))
	fmt.Printf("in use: ResultBytesPerToken = %v\n", cctranscript.ResultBytesPerToken)

	old := func(s sample) int64 { return (s.in + s.out + 3) / 4 }
	cal := func(s sample) int64 { return cctranscript.EstimateTokens(s.output, s.in, s.out) }
	// no-output: the same rate with the turn's output unknown (a runtime that
	// records no usage) — what such a runtime's estimate can reach.
	noOut := func(s sample) int64 { return cctranscript.EstimateTokens(0, s.in, s.out) }
	for _, set := range []struct {
		name string
		s    []sample
	}{{"fit", fit}, {"held out", held}} {
		for _, e := range []struct {
			name string
			f    func(sample) int64
		}{{"old (in+out)/4", old}, {"calibrated", cal}, {"no-output", noOut}} {
			var rel, ratio []float64
			for _, s := range set.s {
				est := float64(e.f(s))
				m := float64(s.delta)
				rel = append(rel, math.Abs(est-m)/m)
				ratio = append(ratio, m/math.Max(est, 1))
			}
			fmt.Printf("%-9s %-15s |est−measured|/measured: median %.2f, p90 %.2f · measured/est: median %.2f\n",
				set.name, e.name, pct(rel, 50), pct(rel, 90), pct(ratio, 50))
		}
	}
}

// pct is the p-th percentile of v (nearest rank), 0 for none.
func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}
