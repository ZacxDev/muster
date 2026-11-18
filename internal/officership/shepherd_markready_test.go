package officership

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Mark ready marks the head the gates passed (shepherd.go MarkReady): the
// head is read again right before the mutation and right after it.

// TestAHeadThatMovesBeforeTheMutationConvertsNothing: a push between the
// gates' read and the mutation (the second read sees another head) is
// refused, nothing converted.
func TestAHeadThatMovesBeforeTheMutationConvertsNothing(t *testing.T) {
	r := newShepRig()
	reads := 0
	r.gh.onPR = func(n int, p *PR) {
		reads++
		if reads >= 2 { // the gates' read is the first
			p.HeadSHA = oidB
		}
	}
	_, err := r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR)
	if rf, ok := AsRefusal(err); !ok || rf.Gate != GateReady || !strings.Contains(rf.Reason, "head moved from "+oidA[:7]) || !strings.Contains(rf.Reason, "nothing was converted") {
		t.Fatalf("err = %v, want the before-mutation refusal", err)
	}
	if len(r.gh.readied) != 0 || len(r.gh.drafted) != 0 {
		t.Fatalf("readied %v drafted %v, want neither", r.gh.readied, r.gh.drafted)
	}
}

// TestAHeadThatMovesDuringTheMutationIsConvertedBackToDraft: a push racing
// the mutation (the read after it sees another head) converts the pull
// request back to draft and says why; a refused conversion back says so.
func TestAHeadThatMovesDuringTheMutationIsConvertedBackToDraft(t *testing.T) {
	r := newShepRig()
	r.gh.afterReady = func() {
		p := r.gh.prs[sPR]
		p.HeadSHA = oidB
		r.gh.prs[sPR] = p
	}
	out, err := r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR)
	if rf, ok := AsRefusal(err); !ok || rf.Gate != GateReady || !strings.Contains(rf.Reason, "so muster converted it back to draft") {
		t.Fatalf("err = %v, want the converted-back refusal", err)
	}
	if out.Ready || len(r.gh.readied) != 1 || len(r.gh.drafted) != 1 || r.gh.drafted[0] != sNode || !r.gh.prs[sPR].Draft {
		t.Fatalf("ready %v readied %v drafted %v draft %v; want converted, then converted back", out.Ready, r.gh.readied, r.gh.drafted, r.gh.prs[sPR].Draft)
	}
	r = newShepRig()
	r.gh.afterReady = func() {
		p := r.gh.prs[sPR]
		p.HeadSHA = oidB
		r.gh.prs[sPR] = p
	}
	r.gh.draftErr = errors.New("fake: not permitted")
	_, err = r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR)
	if rf, ok := AsRefusal(err); !ok || !strings.Contains(rf.Reason, "could NOT convert it back to draft") {
		t.Fatalf("a refused conversion back: err = %v", err)
	}
}
