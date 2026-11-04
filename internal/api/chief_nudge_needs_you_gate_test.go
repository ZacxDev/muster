package api

// needsYouLive sets MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS in f's config to
// owners (none: the list is empty). The step-3a tests set the needs-you gate
// only through this, so the same test file runs against a base that has no
// such gate (with this replaced by a no-op) to show each regression red there.
func needsYouLive(f *nudgeFixture, owners ...int64) {
	m := map[int64]bool{}
	for _, o := range owners {
		m[o] = true
	}
	f.cfg.NeedsYouLive = m
}
