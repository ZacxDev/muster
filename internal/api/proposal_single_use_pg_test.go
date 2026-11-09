package api

import (
	"errors"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// ONE LINE, ONE PROPOSAL OF EACH KIND (internal/plugins CreateProposal, the one
// place the rule lives) under concurrency, and a refused one's card never
// reaching the owner's phone. Every value is invented fixture data.

// TestOneLineFilesOneProposalUnderConcurrency: N concurrent CreateProposal
// calls on one owner line, one kind, one session — exactly one is filed and
// every other is ErrProposalLineUsed (the advisory lock serialises the check
// and the insert).
func TestOneLineFilesOneProposalUnderConcurrency(t *testing.T) {
	// A pool wide enough that every call holds its own connection at once
	// (pgxpool's default is the CPU count, which would queue them).
	const n, rounds = 24, 8
	withPoolMaxConns(t, n+4)
	f := newSuccFixture(t)
	for r := 0; r < rounds; r++ {
		line := int64(920_001 + r)
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = f.plugins.CreateProposal(f.asOwner, plugins.ProposalNew{Kind: plugins.ProposalEndSession, ChiefAgent: f.chief.ID,
					OwnerMessage: line, SubjectKey: sxOpenCodeKey, SubjectRuntime: "opencode", SubjectHost: "host-echo", SubjectTmuxSession: "main",
					SubjectTmuxWindow: "@3"})
			}(i)
		}
		close(start)
		wg.Wait()
		filed, used := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				filed++
			case errors.Is(err, plugins.ErrProposalLineUsed):
				used++
			default:
				t.Errorf("round %d: an unexpected error: %v", r, err)
			}
		}
		if filed != 1 || used != n-1 {
			t.Errorf("round %d: %d concurrent proposals on one line: %d filed, %d refused as used; want 1 and %d", r, n, filed, used, n-1)
		}
	}
}

// TestARefusedRepeatProposalPushesNothing: a proposal refused because its line
// was used already (ErrProposalLineUsed) never reaches the owner's phone — its
// card is notified only once the proposal is recorded. The control: the
// accepted one's card is pushed.
func TestARefusedRepeatProposalPushesNothing(t *testing.T) {
	f := newSuccFixture(t)
	pusher, store := &payloadPusher{}, &memPush{}
	f.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, nil)
	claim(t, store, f.owner, ep("succ-owner"))
	alerts := func() []string {
		f.srv.WaitForPushes()
		var out []string
		for _, p := range pusher.to(ep("succ-owner")) {
			if p.Type == attentionNotificationType {
				out = append(out, p.Tag)
			}
		}
		return out
	}
	f.viewLine(t, sxOpenCodeKey, "close this agent session")
	f.plainLine(t, "Plugin Example mail: the weekly newsletter", 0)
	_, id := f.proposeEnd(t, sxOpenCodeKey)
	if id <= 0 {
		t.Fatal("the first proposal was not filed")
	}
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	if got := alerts(); len(got) != 1 || got[0] != attentionTag(attentionAskKey(x.AskID)) {
		t.Fatalf("control: the filed proposal's card pushed %v; want exactly its own", got)
	}
	f.plainLine(t, "Plugin Example mail: please propose it again", 0)
	if got := errorText(t, f.callRaw(f.chief, endSessionProposeTool, map[string]any{"session": sxOpenCodeKey})); got != proposalLineUsed {
		t.Fatalf("the repeat: %q; want %q", got, proposalLineUsed)
	}
	if got := alerts(); len(got) != 1 {
		t.Errorf("after the refused repeat the owner's phone got %v; want only the first card", got)
	}
}
