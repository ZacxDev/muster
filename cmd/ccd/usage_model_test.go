package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// modelRig is one session whose hours mix models, every output distinct
// where it must not tie:
//
//	10:00  claude-alpha 2 calls, output 20 + 30 = 50; claude-beta 1 call, 70
//	       -> claude-beta (most OUTPUT, not most calls)
//	11:00  claude-beta 40 first, claude-alpha 40 second -> claude-alpha (a tie:
//	       the smaller model id, not the first seen)
//	12:00  claude-alpha 45 first, claude-beta 45 second -> claude-alpha (the
//	       same tie the other way round)
//	13:00  claude-alpha 15 first, claude-beta 16 second -> claude-beta (the
//	       larger output, not the first seen)
//	14:00  one call with no model field -> no model key at all
func modelRig(t *testing.T) *server {
	t.Helper()
	config := t.TempDir()
	noModel := func(at, id string, in, out int64) string {
		b, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": at, "message": map[string]any{"id": id,
			"usage": map[string]any{"input_tokens": in, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": out}}})
		return string(b) + "\n"
	}
	writeUsageFile(t, filepath.Join(config, "projects", "-m", "sess-m.jsonl"), ""+
		aLine("2000-01-03T10:01:00.000Z", "msg_m1", "claude-alpha", 1, 101, 11, 20, nil)+
		aLine("2000-01-03T10:02:00.000Z", "msg_m2", "claude-alpha", 2, 102, 12, 30, nil)+
		aLine("2000-01-03T10:03:00.000Z", "msg_m3", "claude-beta", 3, 103, 13, 70, nil)+
		aLine("2000-01-03T11:01:00.000Z", "msg_m4", "claude-beta", 4, 104, 14, 40, nil)+
		aLine("2000-01-03T11:02:00.000Z", "msg_m5", "claude-alpha", 5, 105, 15, 40, nil)+
		aLine("2000-01-03T12:01:00.000Z", "msg_m6", "claude-alpha", 6, 106, 16, 45, nil)+
		aLine("2000-01-03T12:02:00.000Z", "msg_m7", "claude-beta", 7, 107, 17, 45, nil)+
		aLine("2000-01-03T13:01:00.000Z", "msg_m8", "claude-alpha", 8, 108, 18, 15, nil)+
		aLine("2000-01-03T13:02:00.000Z", "msg_m9", "claude-beta", 9, 109, 19, 16, nil)+
		noModel("2000-01-03T14:01:00.000Z", "msg_m10", 10, 17))
	s := newServer(serverConfig{ConfigDir: config, Bearer: usageBearer}, nil, newAuthTracker())
	s.clock = func() time.Time { return time.Date(2000, 1, 3, 14, 20, 0, 0, time.UTC) }
	return s
}

// TestUsageRouteNamesEachSessionHoursModel pins the answer over modelRig: a
// bucket's model is the one with the most output tokens in it, a tie going
// to the smaller model id whichever was seen first, and a bucket whose calls
// name no model carries no model key.
func TestUsageRouteNamesEachSessionHoursModel(t *testing.T) {
	s := modelRig(t)
	_, _, raw := getUsage(t, s, "?since="+unixOf("2000-01-03T10:00:00Z"), true)
	want := `{"since":"2000-01-03T10:00:00Z","buckets":[` +
		`{"session":"sess-m","hour":"2000-01-03T10:00:00Z","turns":0,"api_calls":3,"input_tokens":6,"output_tokens":120,"cache_read_tokens":306,"cache_creation_tokens":36,"model":"claude-beta"},` +
		`{"session":"sess-m","hour":"2000-01-03T11:00:00Z","turns":0,"api_calls":2,"input_tokens":9,"output_tokens":80,"cache_read_tokens":209,"cache_creation_tokens":29,"model":"claude-alpha"},` +
		`{"session":"sess-m","hour":"2000-01-03T12:00:00Z","turns":0,"api_calls":2,"input_tokens":13,"output_tokens":90,"cache_read_tokens":213,"cache_creation_tokens":33,"model":"claude-alpha"},` +
		`{"session":"sess-m","hour":"2000-01-03T13:00:00Z","turns":0,"api_calls":2,"input_tokens":17,"output_tokens":31,"cache_read_tokens":217,"cache_creation_tokens":37,"model":"claude-beta"},` +
		`{"session":"sess-m","hour":"2000-01-03T14:00:00Z","turns":0,"api_calls":1,"input_tokens":10,"output_tokens":17,"cache_read_tokens":0,"cache_creation_tokens":0}]}` + "\n"
	if raw != want {
		t.Fatalf("answer\n got %s\nwant %s", raw, want)
	}
}

// TestUsageRouteStaysReadableByAnOlderReader: an older muster-server decodes
// the answer into a bucket with no model field (agentgateway's plain decoder)
// and gets every count — an INVARIANT guard (encoding/json ignores an unknown
// field), kept so a future strict decoder there is a decision, not an
// accident.
func TestUsageRouteStaysReadableByAnOlderReader(t *testing.T) {
	s := modelRig(t)
	_, _, raw := getUsage(t, s, "?since="+unixOf("2000-01-03T10:00:00Z"), true)
	var old struct {
		Buckets []struct {
			Session      string    `json:"session"`
			Hour         time.Time `json:"hour"`
			APICalls     int64     `json:"api_calls"`
			OutputTokens int64     `json:"output_tokens"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Buckets) != 5 || old.Buckets[0].APICalls != 3 || old.Buckets[0].OutputTokens != 120 {
		t.Fatalf("an older reader decoded %+v", old.Buckets)
	}
}
