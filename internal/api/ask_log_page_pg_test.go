package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
)

// TestALogRowLinksOnlyAnAskWithAPage: a Log row of an agent's question links
// to its ask's page only where that page opens for the reader (askHasPage,
// the ask page's own rule): the owner's own ask is linked; a question a
// session put to the chief — which the ask page answers 404 — is not, nor is
// an ask another user reads, nor one that does not exist.
//
// RED at 5ec49bc: every row with an ask id linked to its page, the chief's
// included.
func TestALogRowLinksOnlyAnAskWithAPage(t *testing.T) {
	f := newAttnFixture(t)
	mk := func(toChief bool) attention.Ask {
		a, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "main:1 @ ws"},
			Question: "Which branch?", ToChief: toChief})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE id = $1`, a.ID)
		})
		return a
	}
	own, chief := mk(false), mk(true)
	row := func(id int64) asklog.Ask {
		return asklog.Ask{ID: 1, Kind: asklog.KindAttention, SourceRef: strconv.FormatInt(id, 10), Question: "Which branch?", AskedAt: time.Now()}
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
		id   int64
		want string
	}{
		{"own ask", asUser(1), own.ID, askPagePath(own.ID)},
		{"to the chief", asUser(1), chief.ID, ""},
		{"another user's", asUser(2), own.ID, ""},
		{"no such ask", asUser(1), own.ID + 1_000_000, ""},
	} {
		page := f.srv.askLogPage(c.ctx, row(c.id))
		if page != c.want {
			t.Errorf("%s: page %q, want %q", c.name, page, c.want)
		}
		card := askLogCardHTML(row(c.id), time.Now(), asklog.DefaultTextTTL, page)
		if got := strings.Contains(card, "data-asks-card-link"); got != (c.want != "") {
			t.Errorf("%s: card link %v, want %v", c.name, got, c.want != "")
		}
	}
}
