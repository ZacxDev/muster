package consumerusage

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

func hr(h int) time.Time { return time.Date(2026, 11, 10, h, 0, 0, 0, time.UTC) }

// TestAccountAtIsTheLastAccountTheInstanceRanOnInTheHour: with failovers
// acct-a -> acct-b at 10:20 and acct-b -> acct-c at exactly 12:00 (given
// newest first, as the failover store answers), and the instance on acct-c
// now: 09:00 ran on acct-a; 10:00 ended on acct-b (the move inside it); 11:00
// on acct-b (a move AT the hour's end belongs to the next hour); 12:00 on
// acct-c. An instance with no failover is on its current account every hour.
func TestAccountAtIsTheLastAccountTheInstanceRanOnInTheHour(t *testing.T) {
	fos := []Failover{
		{From: "acct-b", To: "acct-c", At: hr(12)},
		{From: "acct-a", To: "acct-b", At: hr(10).Add(20 * time.Minute)},
	}
	for _, c := range []struct {
		hour int
		want string
	}{{9, "acct-a"}, {10, "acct-b"}, {11, "acct-b"}, {12, "acct-c"}, {13, "acct-c"}} {
		if got := AccountAt("acct-c", fos, hr(c.hour)); got != c.want {
			t.Errorf("hour %02d:00: %q, want %q", c.hour, got, c.want)
		}
	}
	if got := AccountAt("acct-d", nil, hr(9)); got != "acct-d" {
		t.Errorf("no failover: %q, want the current account", got)
	}
}

// TestRowsAreTheContractRowsSortedAttributedAndValid: buckets out of order
// become rows sorted by hour then session, each on its hour's account, in
// the contract's field order; a session HQ would refuse, an hour off the hour
// and a bucket with no account are left out and counted.
func TestRowsAreTheContractRowsSortedAttributedAndValid(t *testing.T) {
	fos := []Failover{{From: "acct-a", To: "acct-b", At: hr(11).Add(5 * time.Minute)}}
	bs := []cctranscript.UsageBucket{
		{Session: "s-two", Hour: hr(12), Turns: 21, APICalls: 22, InputTokens: 23, OutputTokens: 24, CacheReadTokens: 25, CacheCreationTokens: 26},
		{Session: "s-one", Hour: hr(10), Turns: 11, APICalls: 12, InputTokens: 13, OutputTokens: 14, CacheReadTokens: 15, CacheCreationTokens: 16},
		{Session: "bad session!", Hour: hr(10), Turns: 1},
		{Session: "s-off", Hour: hr(10).Add(time.Minute), Turns: 2},
	}
	rows, invalid := Rows(bs, "acct-b", fos)
	b, _ := json.Marshal(rows)
	want := `[{"hour":"2026-11-10T10:00:00Z","account":"acct-a","session":"s-one","turns":11,"api_calls":12,"input_tokens":13,"output_tokens":14,"cache_read_tokens":15,"cache_creation_tokens":16},` +
		`{"hour":"2026-11-10T12:00:00Z","account":"acct-b","session":"s-two","turns":21,"api_calls":22,"input_tokens":23,"output_tokens":24,"cache_read_tokens":25,"cache_creation_tokens":26}]`
	if string(b) != want || invalid != 2 {
		t.Fatalf("rows %s (invalid %d)\nwant %s (invalid 2)", b, invalid, want)
	}
	if rows, invalid := Rows(bs[:1], "", nil); len(rows) != 0 || invalid != 1 {
		t.Fatalf("no account: %d rows, %d invalid; want the bucket left out", len(rows), invalid)
	}
}

// TestOverMaxRowsTheNewestAreKept: 1003 hourly buckets send the newest 1000.
func TestOverMaxRowsTheNewestAreKept(t *testing.T) {
	var bs []cctranscript.UsageBucket
	for i := 1002; i >= 0; i-- {
		bs = append(bs, cctranscript.UsageBucket{Session: fmt.Sprintf("s-%d", i), Hour: hr(0).Add(time.Duration(i) * time.Hour), Turns: int64(i)})
	}
	rows, _ := Rows(bs, "acct-a", nil)
	if len(rows) != MaxRows || rows[0].Turns != 3 || rows[len(rows)-1].Turns != 1002 {
		t.Fatalf("%d rows, first %d last %d; want 1000, the newest (3..1002)", len(rows), rows[0].Turns, rows[len(rows)-1].Turns)
	}
}

// TestTheConsumerSetIsClosed: pr-review is a consumer; officers (their own
// route) and anything else are not.
func TestTheConsumerSetIsClosed(t *testing.T) {
	if !ValidConsumer("pr-review") {
		t.Fatal("pr-review refused")
	}
	for _, c := range []string{"", "officers", "PR-Review", "other"} {
		if ValidConsumer(c) {
			t.Errorf("%q accepted", c)
		}
	}
}

// TestTheRouteIsDerivedFromThePoolPushURL: the pool route's sibling, base
// kept; a URL that is not the pool route, or carries a query, is refused.
func TestTheRouteIsDerivedFromThePoolPushURL(t *testing.T) {
	got, err := URLFromPoolPush("https://hq.example.com/api/v1/claude-pool/usage")
	if err != nil || got != "https://hq.example.com/api/v1/claude-pool/consumer-usage" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = URLFromPoolPush("http://127.0.0.1:9/base/claude-pool/usage")
	if err != nil || got != "http://127.0.0.1:9/base/claude-pool/consumer-usage" {
		t.Fatalf("loopback with a base path: %q %v", got, err)
	}
	for _, bad := range []string{"https://hq.example.com/api/usage", "https://hq.example.com/api/v1/claude-pool/usage?x=1",
		"https://hq.example.com/api/v1/claude-pool/usage/"} {
		if got, err := URLFromPoolPush(bad); err == nil {
			t.Errorf("%s: derived %q, want a refusal", bad, got)
		}
	}
}
