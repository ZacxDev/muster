package api

import (
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// What TestAUsageLimitHoldsTheQueueInsteadOfDrainingIt reads that origin/main
// has no field or constant for, in a file of their own, so that test can be
// run against a tree without them (this file swapped for one that reports
// nothing and counts nothing) and seen to fail there.

// limitHeldPrefix and limitMargin are the held line's words and the wait
// after a reset.
const (
	limitHeldPrefix = ui.SendQueueLimitHeldPrefix
	limitMargin     = sendqueue.LimitMargin
)

// limitRejects is the host's report of a turn a usage limit ended at once: the
// limit, its reset, and the prompt it answered (the limit its only reply).
func limitRejects(st *mirror.AgentState, prompt string, reset time.Time) {
	st.TurnError = &mirror.TurnError{Kind: mirror.TurnErrorLimit, At: time.Now().UnixMilli(), ResetsAt: reset.UnixMilli(),
		Prompt: mirror.PromptDigest(prompt), Quick: 1}
}

// returnsOf is how many times it went back to the queue.
func returnsOf(it sendqueue.Item) int { return it.Returns }
