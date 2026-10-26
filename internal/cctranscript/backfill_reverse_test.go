package cctranscript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// BACKFILL READS BACKWARDS AND STOPS (tail.go): it must answer EXACTLY what
// the forward decode-and-trim it replaced answered — the same lines, events,
// prompt places and resume offset — on every shape a live transcript takes:
// lines with no events, several events per line, a line that straddles the
// event cap, undecodable lines, empty lines, a partial last line, a window
// that starts mid-line, and windows and caps either side of the file.

// forwardBackfill is Backfill as it was: the window's start found the same
// way, then backfillForward.
func forwardBackfill(r io.ReaderAt, size int64, window, maxEvents int) ([]Line, int64, error) {
	start := size - int64(window)
	if start < 0 {
		start = 0
	}
	if start > 0 {
		prev := make([]byte, 1)
		if _, err := r.ReadAt(prev, start-1); err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, err
		}
		if prev[0] != '\n' {
			nl, err := nextNewline(r, start, size)
			if err != nil {
				return nil, 0, err
			}
			if nl < 0 {
				return nil, start, nil
			}
			start = nl + 1
		}
	}
	return backfillForward(r, start, size, window, maxEvents)
}

// randomTranscript is a transcript of n rows of the shapes above.
func randomTranscript(rng *rand.Rand, n int) []byte {
	var b bytes.Buffer
	row := func(v any) { j, _ := json.Marshal(v); b.Write(j); b.WriteByte('\n') }
	for i := 0; i < n; i++ {
		switch rng.Intn(7) {
		case 0:
			row(map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"content": fmt.Sprintf("prompt %d %s", i, strings.Repeat("x", rng.Intn(300)))}})
		case 1:
			var parts []any
			for k := rng.Intn(4) + 1; k > 0; k-- {
				parts = append(parts, map[string]any{"type": "text", "text": fmt.Sprintf("reply %d.%d", i, k)})
			}
			row(map[string]any{"type": "assistant", "message": map[string]any{"content": parts}})
		case 2:
			row(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_use", "id": fmt.Sprint("t", i), "name": "Bash", "input": map[string]any{"command": "ls"}}}}})
		case 3:
			row(map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprint("t", i), "content": strings.Repeat("out ", rng.Intn(200)), "is_error": false}}}})
		case 4:
			row(map[string]any{"type": "attachment", "attachment": map[string]any{"type": "x"}}) // draws nothing
		case 5:
			b.WriteString("{not json\n")
		default:
			b.WriteString("\n")
		}
	}
	if rng.Intn(2) == 0 {
		b.WriteString(`{"type":"user","message":{"content":"still being wri`) // partial last line
	}
	return b.Bytes()
}

func TestBackfillBackwardsAnswersWhatForwardsDid(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	cases, trimmed, partial := 0, 0, 0
	for i := 0; i < 3000; i++ {
		data := randomTranscript(rng, rng.Intn(80))
		size := int64(len(data))
		window := rng.Intn(len(data)+50) + 1
		maxEvents := rng.Intn(40) + 1
		r := bytes.NewReader(data)
		got, gotOff, err := Backfill(r, size, window, maxEvents)
		want, wantOff, werr := forwardBackfill(r, size, window, maxEvents)
		if (err == nil) != (werr == nil) || gotOff != wantOff || !reflect.DeepEqual(nonNil(got), nonNil(want)) {
			t.Fatalf("case %d (window %d, max %d, %d bytes):\n backwards %d lines off %d err %v\n  forwards %d lines off %d err %v",
				i, window, maxEvents, size, len(got), gotOff, err, len(want), wantOff, werr)
		}
		cases++
		n := 0
		for _, l := range want {
			n += len(l.Events)
		}
		if n == maxEvents {
			trimmed++
		}
		if !bytes.HasSuffix(data, []byte("\n")) && len(data) > 0 {
			partial++
		}
	}
	// CONTROL: the cap was reached, and partial lines were present, in many
	// cases — not a sweep of empty windows.
	if trimmed < 300 || partial < 300 {
		t.Fatalf("only %d capped and %d partial cases of %d", trimmed, partial, cases)
	}
}

func nonNil(l []Line) []Line {
	if l == nil {
		return []Line{}
	}
	return l
}

// BenchmarkBackfill is a 4 MiB window of a long transcript: what a view's open
// decodes.
func BenchmarkBackfill(b *testing.B) {
	data := randomTranscript(rand.New(rand.NewSource(1)), 60000)
	r := bytes.NewReader(data)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := Backfill(r, int64(len(data)), 4<<20, 300); err != nil {
			b.Fatal(err)
		}
	}
}
