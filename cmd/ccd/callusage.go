package main

import (
	"strconv"

	"github.com/ZacxDev/muster/internal/podagent"
)

// callLedger is THE rule for counting a transcript's API calls and their
// tokens, used by a turn's usage (turnResult.addUsage) and by the hourly usage
// GET /v1/usage answers (usage_route.go). One rule, one place: a second copy
// would drift on exactly the cases below.
//
//   - The CLI writes ONE assistant record per content block (thinking, text,
//     tool_use), each repeating the SAME API call's usage. A call is counted
//     once.
//   - A call's key is its message id AND its input side (uncached input, cache
//     read, cache creation), which every block record of that call repeats and
//     the next call — its context larger — does not. The id alone is not
//     enough: a transcript whose ids were normalised (the fixtures) or left out
//     would fold distinct calls into one.
//   - Output is not in the key: a block written mid-stream can carry a smaller
//     output count than the last. Each field keeps the call's LARGEST value.
//   - An API-error record (isApiErrorMessage) and a "<synthetic>" record (the
//     CLI's own, no API call behind it) are not calls.
type callLedger struct {
	byKey map[string]podagent.Usage
	order []string
}

// add records one assistant record's usage. counted is false when the record
// is not a call (no usage, an API error, a synthetic record), and then nothing
// is recorded; first reports that this record began a call the ledger had not
// seen. key names the call (call).
func (l *callLedger) add(id, model string, apiError bool, u *podagent.Usage) (key string, first, counted bool) {
	if u == nil || apiError || model == "<synthetic>" {
		return "", false, false
	}
	if l.byKey == nil {
		l.byKey = map[string]podagent.Usage{}
	}
	key = id + ":" + strconv.FormatInt(u.InputTokens, 10) + ":" + strconv.FormatInt(u.CacheReadInputTokens, 10) +
		":" + strconv.FormatInt(u.CacheCreationInputTokens, 10)
	cur, seen := l.byKey[key]
	if !seen {
		l.order = append(l.order, key)
	}
	cur.InputTokens = max(cur.InputTokens, u.InputTokens)
	cur.OutputTokens = max(cur.OutputTokens, u.OutputTokens)
	cur.CacheReadInputTokens = max(cur.CacheReadInputTokens, u.CacheReadInputTokens)
	cur.CacheCreationInputTokens = max(cur.CacheCreationInputTokens, u.CacheCreationInputTokens)
	l.byKey[key] = cur
	return key, !seen, true
}

// call is the call key's usage, as merged so far.
func (l *callLedger) call(key string) podagent.Usage { return l.byKey[key] }

// keys are the calls in the order they were first seen.
func (l *callLedger) keys() []string { return l.order }

// sum is every call's usage added up.
func (l *callLedger) sum() podagent.Usage {
	var s podagent.Usage
	for _, k := range l.order {
		m := l.byKey[k]
		s.InputTokens += m.InputTokens
		s.OutputTokens += m.OutputTokens
		s.CacheReadInputTokens += m.CacheReadInputTokens
		s.CacheCreationInputTokens += m.CacheCreationInputTokens
	}
	return s
}
