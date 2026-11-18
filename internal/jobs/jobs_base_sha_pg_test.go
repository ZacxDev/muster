package jobs

import "testing"

// TestThePGStoreRoundTripsBaseSHA: a job recorded with a BaseSHA reads it back,
// by id and by idempotency key; one recorded without it reads back "".
func TestThePGStoreRoundTripsBaseSHA(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 1, "job-base")
	ctx := as(1)
	const base = "fedcba9876543210fedcba9876543210fedcba98"
	with, created, err := s.Create(ctx, NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "review", Repo: "acme/widgets",
		SHA: fullSHA, PR: 7, TimeoutSeconds: 90, IdempotencyKey: "with-base", BaseSHA: base})
	if err != nil || !created {
		t.Fatalf("create = %v, %v", created, err)
	}
	for _, read := range []func() (Job, error){
		func() (Job, error) { return with, nil },
		func() (Job, error) { return s.Get(ctx, with.ID) },
		func() (Job, error) { return s.ByIdempotencyKey(ctx, "with-base") },
	} {
		if got, err := read(); err != nil || got.BaseSHA != base {
			t.Fatalf("read = base_sha %q (%v), want the base", got.BaseSHA, err)
		}
	}
	without, _, err := s.Create(ctx, NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "review", Repo: "acme/widgets",
		SHA: fullSHA, PR: 7, TimeoutSeconds: 90})
	if err != nil || without.BaseSHA != "" {
		t.Fatalf("a job recorded without one = base_sha %q (%v), want none", without.BaseSHA, err)
	}
}
