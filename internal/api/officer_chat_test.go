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

// TestAnOfficersChatSessionsBecomeItsPodsLaneCap: the provisioner's session
// source gives an officer's instance its declared chat_sessions as
// MaxSessions (the pod's CCD_MAX_SESSIONS), and 0 — none declared — to an
// officer that declares none, and to a tiered officer's ROUTINE pool, which
// runs no chats.
func TestAnOfficersChatSessionsBecomeItsPodsLaneCap(t *testing.T) {
	const (
		offChat, offChatTok   = "engineering:chat-web", "fixture-officer-token-chat-7a8b"
		offPlain, offPlainTok = "triage:chat-plain", "fixture-officer-token-plain-9c0d"
		offTier, offTierTok   = "investigate:chat-infra", "fixture-officer-token-tier-1e2f"
	)
	hq := hqtest.New(map[string]string{offChatTok: offChat, offPlainTok: offPlain, offTierTok: offTier})
	t.Cleanup(hq.Close)
	store := &officerAgents{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: &officerProv{store: store, phase: map[string]provision.Phase{}}, Gateway: &repromptChat{},
		SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	base := func(id, tok string) officers.Officer {
		return officers.Officer{ID: id, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(tok)
	}
	chat := base(offChat, offChatTok).WithChatSessions(2)
	plain := base(offPlain, offPlainTok)
	tier := base(offTier, offTierTok).WithChatSessions(4)
	tier.Tiers = &officers.Tiers{Routine: officers.TierPool{Model: "haiku", MaxInstances: 1}, Judgement: officers.TierPool{Model: "opus", MaxInstances: 1}}
	tier.Tier, tier.Model = officers.TierJudgement, "opus"
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{chat, plain, tier}, InstallSession: discardSession, NoShipResume: true}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{
		officers.InstanceName(offChat, 1):                 2,
		officers.InstanceName(offPlain, 1):                0,
		tier.Pool(officers.TierJudgement).InstanceName(1): 4,
		tier.Pool(officers.TierRoutine).InstanceName(1):   0,
	} {
		sess, ok := s.OfficerClaudeCodeSession(agents.Agent{Name: name, Kind: agents.KindClaudeCode, OwnerID: 1})
		if !ok || sess.MaxSessions != want {
			t.Errorf("%s: MaxSessions %d (officer %v), want %d", name, sess.MaxSessions, ok, want)
		}
	}
}
