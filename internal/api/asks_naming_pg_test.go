package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
)

// TestAnAnsweredCardIsTitledByTheSessionsName: a named mirrored session's ask
// is titled "Ledger cleanup" on the Open tab; answered, its Answered card is
// titled the SAME (the one naming rule, attentionTitle), "on <host>" under it
// and its tmux target in its ⓘ — never its window's place as the title.
// (Its delivery line names it in words too, and its ⓘ holds the target:
// TestAnAnswersDeliveryLineNamesTheExactTmuxTarget.)
//
// RED at origin/main d1ff2dbe: the Answered card's title was the window's
// place, "tmux main:1 @ ws".
func TestAnAnsweredCardIsTitledByTheSessionsName(t *testing.T) {
	f := newAttnFixture(t)
	if _, err := focus.NewPG(f.pool).SetManualName(asUser(1), focus.MirrorSubject(focusSID), "Ledger cleanup"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, focusSID)
	})
	a := f.mirrorAsk(t, "Ship the ledger fix?")
	open := f.page(t, "/attention")
	if !strings.Contains(itemLi(t, open, attentionAskKey(a.ID)), `data-attention-title data-attention-name>Ledger cleanup</span>`) {
		t.Fatalf("the Open card is not titled by the name:\n%s", itemLi(t, open, attentionAskKey(a.ID)))
	}
	rd, hdr := form(url.Values{"answer": {"Ship it"}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(a.ID, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	card := itemLi(t, f.page(t, "/attention?tab=answered"), "answered-"+strconv.FormatInt(a.ID, 10))
	for _, want := range []string{
		`data-attention-title>Ledger cleanup</span>`,
		// Where it runs in words; the exact target in the card's ⓘ.
		`data-attention-source>on ws</span>`,
		`data-info-code="">tmux main:1 @ ws</code>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the Answered card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, `font-semibold text-fg" data-attention-source>`) {
		t.Errorf("the title is still the window's place:\n%s", card)
	}
}
