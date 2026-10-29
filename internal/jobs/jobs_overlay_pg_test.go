package jobs

import "testing"

// TestThePGStoreRoundTripsRepoConfigOverlay: a job recorded with
// SkipRepoConfig reads back RepoConfigOverlay false, by id and by idempotency
// key; one recorded without it (the zero NewJob) reads back true.
func TestThePGStoreRoundTripsRepoConfigOverlay(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 1, "job-overlay")
	ctx := as(1)
	off, created, err := s.Create(ctx, NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "review", Repo: "acme/widgets",
		SHA: fullSHA, PR: 7, TimeoutSeconds: 90, IdempotencyKey: "overlay-off", SkipRepoConfig: true})
	if err != nil || !created {
		t.Fatalf("create = %v, %v", created, err)
	}
	for _, read := range []func() (Job, error){
		func() (Job, error) { return off, nil },
		func() (Job, error) { return s.Get(ctx, off.ID) },
		func() (Job, error) { return s.ByIdempotencyKey(ctx, "overlay-off") },
	} {
		if got, err := read(); err != nil || got.RepoConfigOverlay {
			t.Fatalf("read = repo_config_overlay %t (%v), want false", got.RepoConfigOverlay, err)
		}
	}
	on, _, err := s.Create(ctx, NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "review", Repo: "acme/widgets",
		SHA: fullSHA, PR: 7, TimeoutSeconds: 90})
	if err != nil || !on.RepoConfigOverlay {
		t.Fatalf("a job recorded without it = repo_config_overlay %t (%v), want true", on.RepoConfigOverlay, err)
	}
}
