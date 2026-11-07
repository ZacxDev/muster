package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/asklog"
)

// THE ASK LOG (internal/asklog, internal/api/ask_log.go, docs/asks.md): its
// switch (off by default), how long question and answer text is kept, and
// whether a kept plain-text question is classified at capture.
const (
	envAskLog         = "MUSTER_ASK_LOG"
	envAskLogTextTTL  = "MUSTER_ASK_LOG_TEXT_TTL"
	envAskLogClassify = "MUSTER_ASK_LOG_CLASSIFY"
)

// askLogConfig is the ask log's configuration.
type askLogConfig struct {
	// On records every question an agent puts to its owner (MUSTER_ASK_LOG=1;
	// off by default).
	On bool
	// TextTTL is how long question and answer text is kept
	// (MUSTER_ASK_LOG_TEXT_TTL; asklog.DefaultTextTTL, 30 days, by default),
	// from one hour to asklog.RowTTL (the rows themselves are kept that long).
	TextTTL time.Duration
	// JevAtCapture runs the stall classifier on a kept plain-text question at
	// capture (MUSTER_ASK_LOG_CLASSIFY=1; off by default, so the pre-filter's
	// volume is measured before the classifier is spent on it).
	JevAtCapture bool
}

// parseAskLog reads the ask log's configuration.
func parseAskLog(getenv func(string) string) (askLogConfig, error) {
	c := askLogConfig{On: envFlag(getenv, envAskLog), JevAtCapture: envFlag(getenv, envAskLogClassify), TextTTL: asklog.DefaultTextTTL}
	if v := strings.TrimSpace(getenv(envAskLogTextTTL)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Hour || d > asklog.RowTTL {
			return askLogConfig{}, fmt.Errorf("invalid %s %q: want a duration from 1h to %s (the rows themselves are kept that long)",
				envAskLogTextTTL, v, asklog.RowTTL)
		}
		c.TextTTL = d
	}
	return c, nil
}
