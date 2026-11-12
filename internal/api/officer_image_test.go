package api

import (
	"bytes"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestAnOfficerInstanceRunsItsPoolsImage: the provisioner's session source
// hands each instance its POOL's image — the untiered officer's own, the
// judgement and routine pools' each their own — and an officer with none
// gets "" (the deployment's image). Each also carries
// OfficersConfig.ImagePullSecret (MUSTER_OFFICER_IMAGE_PULL_SECRET), which
// agentspec drops beside an empty image (ccImagePullSecret).
func TestAnOfficerInstanceRunsItsPoolsImage(t *testing.T) {
	const (
		offImg, offImgTok   = "engineering:img-web", "fixture-officer-token-img-1a2b"
		offTier, offTierTok = "investigate:img-infra", "fixture-officer-token-tier-3c4d"
		offNone, offNoneTok = "triage:img-plain", "fixture-officer-token-none-5e6f"
		own                 = "ghcr.io/example-org/officer-runner:1.0.0"
		judge               = "ghcr.io/example-org/officer-judge:2.0.0"
		routine             = "ghcr.io/example-org/officer-routine:3.0.0"
	)
	hq := hqtest.New(map[string]string{offImgTok: offImg, offTierTok: offTier, offNoneTok: offNone})
	t.Cleanup(hq.Close)
	store := &officerAgents{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: &officerProv{store: store, phase: map[string]provision.Phase{}}, Gateway: &repromptChat{},
		SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	base := func(id, tok string) officers.Officer {
		return officers.Officer{ID: id, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(tok)
	}
	img := base(offImg, offImgTok)
	img.Image = own
	tier := base(offTier, offTierTok)
	tier.Tiers = &officers.Tiers{Routine: officers.TierPool{Model: "haiku", MaxInstances: 1, Image: routine},
		Judgement: officers.TierPool{Model: "opus", MaxInstances: 1, Image: judge}}
	tier.Tier, tier.Model, tier.Image = officers.TierJudgement, "opus", judge
	none := base(offNone, offNoneTok)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{img, tier, none}, InstallSession: discardSession, NoShipResume: true,
		ImagePullSecret: "officer-image-pull"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		officers.InstanceName(offImg, 1):                  own,
		tier.Pool(officers.TierJudgement).InstanceName(1): judge,
		tier.Pool(officers.TierRoutine).InstanceName(1):   routine,
		officers.InstanceName(offNone, 1):                 "",
	} {
		sess, ok := s.OfficerClaudeCodeSession(agents.Agent{Name: name, Kind: agents.KindClaudeCode})
		if !ok || sess.Image != want {
			t.Errorf("%s: session image %q (officer %v), want %q", name, sess.Image, ok, want)
		}
		if sess.ImagePullSecret != "officer-image-pull" {
			t.Errorf("%s: session pull Secret %q, want the configured %q", name, sess.ImagePullSecret, "officer-image-pull")
		}
	}
}
