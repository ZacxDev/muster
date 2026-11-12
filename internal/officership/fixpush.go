package officership

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// A DRAFT OFFICER'S FIX OF ITS OWN PULL REQUEST (docs/officers.md "The draft
// stage", "Fixing its own pull request").
//
// HQ hands the AUTHORING officer an item of kind `<type>.pr_fix` (subject
// `<repo>#<pr>`) after a clean audit round's 🟡s or a human's feedback. The
// fix is pushed to that pull request's existing branch — which is named for
// the ORIGINAL item, not the fix item.
//
// A draft officer's push to one of its own branches was already admitted
// without an item (the ref rule, allowedRef; the push re-check, afterPush,
// reads every pull request from it afterwards), and that is unchanged. What a
// held fix item adds, for a push to the branch of the pull request it names:
// the pull request must be open and authored by THIS officer's App, and the
// push is refused in shadow (ShadowRule, the shepherd's rule) before anything
// reaches GitHub. A fix item never opens a pull request (claimed).
// ---------------------------------------------------------------------------

// IsPRFix reports whether an HQ item kind is a pull-request fix item
// (officers.PRFixKindSuffix).
func IsPRFix(kind string) bool { return strings.HasSuffix(kind, officers.PRFixKindSuffix) }

// subjectPR is an item subject `<owner>/<name>#<n>`'s repository and number.
func subjectPR(subject string) (string, int, bool) {
	r, num, ok := strings.Cut(strings.TrimSpace(subject), "#")
	n, err := strconv.Atoi(num)
	return r, n, ok && err == nil && n > 0
}

// admitFixPush is the fix-item half of a draft officer's push (see the top of
// this file): for each pushed branch that heads the pull request of a fix item
// the pushing instance holds (FixItems), that pull request is open and the
// officer App's own, and the push is not in shadow. A push under no held fix
// item is not changed. 🔴 FAIL CLOSED: HQ's items, or the pull request,
// unreadable is a refusal.
func (g *PushGate) admitFixPush(ctx context.Context, sr officers.ShipRepo, cmds []pushCommand) ([]fixPush, error) {
	instance := callerInstance(ctx)
	if g.FixItems == nil || instance == "" || len(cmds) == 0 {
		return nil, nil
	}
	items, err := g.FixItems(ctx, instance)
	if err != nil {
		return nil, refused("muster could not read HQ's items to tell whether this push is under a pull-request fix item (%v); push again", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	gh := g.ghFor(sr.Repo)
	if gh == nil {
		return nil, fmt.Errorf("no GitHub reader for %s", sr.Repo)
	}
	var admitted []fixPush
	for _, c := range cmds {
		branch := strings.TrimPrefix(c.ref, "refs/heads/")
		for _, it := range items {
			repo, n, ok := subjectPR(it.SubjectKey)
			if !ok || !strings.EqualFold(repo, sr.Repo) {
				continue
			}
			pr, err := gh.PR(ctx, n)
			if err != nil {
				return nil, refused("could not read #%d, the pull request of fix item %s you hold: %v", n, it.ID, err)
			}
			if pr.HeadRef != branch || !connectors.SameRepo(pr.HeadRepo, sr.Repo) {
				continue // the fix item is another pull request's
			}
			if pr.State != "open" || pr.Merged {
				return nil, refused("a push to %s under fix item %s: #%d is %s (merged %v); a fix goes to an open pull request", branch, it.ID, n, pr.State, pr.Merged)
			}
			slug, err := gh.AppSlug(ctx)
			if err != nil {
				return nil, refused("could not read this officer App's identity: %v", err)
			}
			if !strings.EqualFold(pr.Author, BotLogin(slug)) {
				return nil, refused("a push to %s under fix item %s: #%d is authored by %s, not this officer's App (%s); a fix goes to the officer's own pull request",
					branch, it.ID, n, pr.Author, BotLogin(slug))
			}
			if why, on := g.shadowRule().refusal(ctx, it, fmt.Sprintf("pushed %s..%s to %s", head12(c.oldOID), head12(c.newOID), branch), true); on {
				return nil, shadowRefused(why)
			}
			admitted = append(admitted, fixPush{item: it, ref: c.ref, branch: branch})
		}
	}
	return admitted, nil
}

// fixPush is one ref of a push admitted under a held fix item.
type fixPush struct {
	item        officers.Item
	ref, branch string
}

// fixPushesLanded records (FixPushed) each of fixes whose ref GitHub's
// report-status says it APPLIED; a ref it refused, or a report that does not
// name it (no answer, none asked), is not a landed push.
func (g *PushGate) fixPushesLanded(fixes []fixPush, res connectors.ForwardResult) {
	if g.FixPushed == nil || len(fixes) == 0 || !res.Forwarded || (res.Status != 0 && res.Status != http.StatusOK) {
		return
	}
	report := parseReportStatus(res.Body)
	for _, f := range fixes {
		if applied, known := report[f.ref]; known && applied {
			g.FixPushed(f.item, f.branch)
		}
	}
}

// shadowRule is this gate's ShadowRule.
func (g *PushGate) shadowRule() ShadowRule {
	return ShadowRule{Officer: g.OfficerID, OfficerShadow: g.OfficerShadow, Shadowed: g.Shadowed}
}
