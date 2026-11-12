package officership

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// The shepherd's shadow (shepherd_loop.go): every write — push, comment,
// update-branch, mark ready, merge ask — is refused when the held item runs
// in shadow, or (the item saying nothing) the officer does, or the officer
// row cannot be read; nothing reaches GitHub, and the "would have …" line is
// recorded against the item.

// shadowWrite is one of the shepherd's five writes against a rig: it returns
// the refusal (nil: admitted) and whether anything was forwarded/sent.
type shadowWrite struct {
	name  string
	would string
	do    func(t *testing.T, r *shepRig) (err error, sent bool)
}

// sentToGitHub counts every write the fake GitHub saw.
func sentToGitHub(r *shepRig) int {
	return len(r.gh.createdRefs) + len(r.gh.updated) + len(r.gh.readied) + len(r.gh.deletedRefs) + len(r.gh.merged)
}

func shadowWrites(mergeAsks *int) []shadowWrite {
	return []shadowWrite{
		{"push", fmt.Sprintf("pushed %s..%s to %s", oidA[:12], oidB[:12], sBranch), func(t *testing.T, r *shepRig) (error, bool) {
			out, err := r.push()
			return err, out != nil
		}},
		{"comment", fmt.Sprintf("commented on %s#%d (%d chars)", sRepo, sPR, len("Round 2: fixed the limit.")), func(t *testing.T, r *shepRig) (error, bool) {
			out, err := r.api(sInstance, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/comments", sRepo, sPR), map[string]any{"body": "Round 2: fixed the limit."})
			return err, out != nil
		}},
		{"update-branch", "updated " + sBranch + " from its base", func(t *testing.T, r *shepRig) (error, bool) {
			out, err := r.api(sInstance, http.MethodPut, fmt.Sprintf("repos/%s/pulls/%d/update-branch", sRepo, sPR), map[string]any{"expected_head_sha": oidA})
			return err, out != nil
		}},
		{"mark ready", fmt.Sprintf("marked %s#%d ready", sRepo, sPR), func(t *testing.T, r *shepRig) (error, bool) {
			_, err := r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR)
			return err, len(r.gh.readied) > 0
		}},
		{"merge ask", fmt.Sprintf("asked HQ for a merge ask of %s#%d at %s", sRepo, sPR, oidA[:12]), func(t *testing.T, r *shepRig) (error, bool) {
			before := *mergeAsks
			_, err := r.gates.MergeAsk(context.Background(), sInstance, validMergeAsk())
			return err, *mergeAsks > before
		}},
	}
}

// shadowRig is a rig whose merge ask reaches a counting fake.
func shadowRig(mergeAsks *int) *shepRig {
	r := newShepRig()
	r.gates.ItemByID = func(_ context.Context, instance string, id officers.ItemID, repo string, n int) (officers.Item, error) {
		if r.held[officers.ShepherdSubject(repo, n)] != instance || id != r.item.ID {
			return officers.Item{}, fmt.Errorf("fake HQ: %s does not hold item %s for %s", instance, id, officers.ShepherdSubject(repo, n))
		}
		return r.item, nil
	}
	r.gates.PostMergeAsk = func(context.Context, officers.ItemID, []byte) ([]byte, error) {
		*mergeAsks++
		return []byte(`{"merge":{},"ask":{},"item":{}}`), nil
	}
	return r
}

func TestEveryShepherdWriteIsRefusedInShadow(t *testing.T) {
	for _, c := range []struct {
		name    string
		set     func(r *shepRig)
		refused bool
		why     string
	}{
		{"item shadow true", func(r *shepRig) { r.item.Shadow = boolp(true) }, true, "HQ runs item " + sItem + " in shadow"},
		{"item shadow false (officer shadow true is not read)", func(r *shepRig) { r.item.Shadow = boolp(false); r.officerShadow = true }, false, ""},
		{"item shadow absent, officer shadow true", func(r *shepRig) { r.item.Shadow = nil; r.officerShadow = true }, true, "HQ runs officer " + sOfficer + " in shadow"},
		{"item shadow absent, officer shadow false", func(r *shepRig) { r.item.Shadow = nil; r.officerShadow = false }, false, ""},
		{"item shadow absent, the officer row unreadable", func(r *shepRig) {
			r.item.Shadow = nil
			r.officerShadowErr = errors.New("fake HQ: 503")
		}, true, "could not be read for its shadow (fake HQ: 503)"},
		{"item shadow absent, no officer reader", func(r *shepRig) { r.item.Shadow = nil; r.g.Shepherd.OfficerShadow = nil }, true, "cannot read HQ's officer row"},
	} {
		mergeAsks := 0
		for _, w := range shadowWrites(&mergeAsks) {
			t.Run(c.name+"/"+w.name, func(t *testing.T) {
				r := shadowRig(&mergeAsks)
				c.set(r)
				err, sent := w.do(t, r)
				if !c.refused {
					if err != nil || !sent {
						t.Fatalf("not in shadow: err = %v, sent %v; want it admitted", err, sent)
					}
					if len(r.shadowLines) != 0 {
						t.Fatalf("recorded %v outside shadow", r.shadowLines)
					}
					return
				}
				want := "shadow: would have " + w.would
				if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), c.why) {
					t.Fatalf("err = %v, want %q with %q", err, want, c.why)
				}
				if sent || sentToGitHub(r) != 0 {
					t.Fatalf("a shadowed %s was sent (sent %v, GitHub writes %d: refs %v updated %v readied %v)", w.name, sent, sentToGitHub(r),
						r.gh.createdRefs, r.gh.updated, r.gh.readied)
				}
				if wantLine := sItem + ": would have " + w.would; len(r.shadowLines) != 1 || r.shadowLines[0] != wantLine {
					t.Fatalf("recorded %q, want [%q]", r.shadowLines, wantLine)
				}
				// Only a PUSH is recorded as one (it blocks the item's hand-off).
				if wantPush := w.name == "push"; len(r.shadowPush) != 1 || r.shadowPush[0] != wantPush {
					t.Fatalf("recorded push flags %v, want [%v]", r.shadowPush, wantPush)
				}
				if w.name == "push" || w.name == "comment" || w.name == "update-branch" {
					if !errors.Is(err, connectors.ErrGateRefused) {
						t.Fatalf("a shadowed %s is not a gate refusal (403): %v", w.name, err)
					}
				} else if rf, ok := AsRefusal(err); !ok || rf.Gate != GateShadow {
					t.Fatalf("a shadowed %s is not refused by gate %s: %v", w.name, GateShadow, err)
				}
			})
		}
	}
}

// TestAShadowedPushTakesNoBranchLock: the refusal releases the branch, so the
// same push is admitted once shadow is off (and is the first push to it).
func TestAShadowedPushTakesNoBranchLock(t *testing.T) {
	r := newShepRig()
	r.item.Shadow = boolp(true)
	if _, err := r.push(); err == nil {
		t.Fatal("a shadowed push was admitted")
	}
	r.item.Shadow = boolp(false)
	if out, err := r.push(); err != nil || out == nil {
		t.Fatalf("after shadow, the push: err = %v", err)
	}
}
