// Package consumerusage pushes the Claude usage of a NON-officer muster's
// agent instances — a muster running PR-review lanes, say — to HQ, per
// (session, UTC hour, pool account), labelled with the muster's CONSUMER, so
// HQ can split each pooled account's spend into officers, the consumers that
// push here, and everything else (docs/officers.md, "Consumer usage to HQ").
//
// THE SOURCE is the one the officer usage collector reads
// (internal/officers/usage): each instance pod's ccd counting its own
// transcripts on GET /v1/usage. muster-server counts nothing itself.
//
// THE SINK is HQ's PUT <base>/api/v1/claude-pool/consumer-usage, with the
// pool push's bearer token (MUSTER_CC_USAGE_PUSH_TOKEN). HQ upserts each row
// on (instance, consumer, session, hour) and replaces its counts, so a push is
// the whole of the window and re-pushing is idempotent.
//
// 🔴 AN OFFICER INSTANCE IS NEVER PUSHED HERE: its usage goes to its officer's
// HQ through internal/officers/usage, and pushing it here as well would count
// it twice at HQ. The caller's instance list (internal/api) leaves officer
// instances out; this package never sees them.
package consumerusage

import (
	"regexp"
	"sort"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// The consumers a muster may push as. A closed set, because HQ refuses a
// consumer it does not know, and a typo would otherwise be found only in HQ's
// logs. Officers are not a consumer: they have their own route.
const (
	// ConsumerPRReview is a muster running pull-request review lanes.
	ConsumerPRReview = "pr-review"
)

// Consumers is every consumer, in the order a refusal names them.
var Consumers = []string{ConsumerPRReview}

// ValidConsumer reports whether c is one of Consumers.
func ValidConsumer(c string) bool {
	for _, k := range Consumers {
		if c == k {
			return true
		}
	}
	return false
}

// SessionPattern is the session id HQ accepts. ccd's sessions are Claude Code
// session ids (UUIDs, 36 characters); a row whose session does not match is
// not sent (HQ would refuse the whole push over it).
var SessionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// MaxRows is the most rows one push carries: the newest are kept (as the
// officer route's 1000 buckets).
const MaxRows = 1000

// Row is one (session, hour) of one instance on the wire, in the contract's
// field order.
type Row struct {
	// Hour is the UTC hour, RFC 3339, on the hour.
	Hour string `json:"hour"`
	// Account is the pool account the instance was on at the END of the hour
	// (AccountAt).
	Account             string `json:"account"`
	Session             string `json:"session"`
	Turns               int64  `json:"turns"`
	APICalls            int64  `json:"api_calls"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	// Model is the hour's model as ccd names it (cctranscript.UsageBucket's
	// rule: the most output tokens, a tie to the smaller id); left out when
	// unknown.
	Model string `json:"model,omitempty"`
}

// Report is one push's body. Hours is never null.
type Report struct {
	// Instance is the muster's label (MUSTER_CC_USAGE_PUSH_INSTANCE, as the
	// pool push's source_instance).
	Instance string `json:"instance"`
	Consumer string `json:"consumer"`
	Hours    []Row  `json:"hours"`
}

// Failover is one move of an instance from one pool account to another
// (internal/ccfailover's record: an agent's account changes ONLY by a
// failover, and every failover is recorded).
type Failover struct {
	From, To string
	At       time.Time
}

// AccountAt is the account an instance was on at the end of the UTC hour
// starting at hour — the LAST account it ran on in that hour: the From of the
// earliest failover at or after the hour's end, or current (the account it
// is on now) when there is none. An instance never failed over is on current
// for every hour, which is the officer push's rule (the row's account at push
// time); a failover inside the hour attributes the whole hour to the account
// it moved TO, as a job is counted on the account its last turn ran on.
func AccountAt(current string, failovers []Failover, hour time.Time) string {
	end := hour.Add(time.Hour)
	acct := current
	var first time.Time
	for _, f := range failovers {
		if !f.At.Before(end) && (first.IsZero() || f.At.Before(first)) {
			first, acct = f.At, f.From
		}
	}
	return acct
}

// Rows is buckets as contract rows, each attributed by AccountAt: sorted by
// hour then session, at most MaxRows (the newest). A bucket whose session
// HQ would refuse, whose hour is not on the hour, or whose account is empty
// is left out and counted in invalid.
func Rows(buckets []cctranscript.UsageBucket, current string, failovers []Failover) (rows []Row, invalid int) {
	bs := make([]cctranscript.UsageBucket, 0, len(buckets))
	for _, b := range buckets {
		b.Hour = b.Hour.UTC()
		bs = append(bs, b)
	}
	sort.SliceStable(bs, func(i, j int) bool {
		if !bs[i].Hour.Equal(bs[j].Hour) {
			return bs[i].Hour.Before(bs[j].Hour)
		}
		return bs[i].Session < bs[j].Session
	})
	rows = make([]Row, 0, len(bs))
	for _, b := range bs {
		acct := AccountAt(current, failovers, b.Hour)
		if !SessionPattern.MatchString(b.Session) || !b.Hour.Equal(b.Hour.Truncate(time.Hour)) || acct == "" {
			invalid++
			continue
		}
		rows = append(rows, Row{Hour: b.Hour.Format(time.RFC3339), Account: acct, Session: b.Session, Turns: b.Turns,
			APICalls: b.APICalls, InputTokens: b.InputTokens, OutputTokens: b.OutputTokens,
			CacheReadTokens: b.CacheReadTokens, CacheCreationTokens: b.CacheCreationTokens, Model: b.Model})
	}
	if n := len(rows) - MaxRows; n > 0 {
		rows = rows[n:]
	}
	return rows, invalid
}
