package api

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// Issue #660, at the two doors that save a users.display_name: the invite
// form (a person's name) and /admin/teams (a team's). A name holding a
// control or a text-direction / invisible formatting character is answered
// 400 with a sentence naming the character, nothing is stored, and the invite
// form never reaches GoTrue; an ordinary name in any script is saved as typed.
func TestADisplayNameWithAControlOrFormatCharacterIsRefusedAtEveryDoor(t *testing.T) {
	w := newInviteWorld(t)
	sys := authz.System(context.Background())
	t.Cleanup(func() {
		_, _ = w.pool.Exec(sys, `DELETE FROM team_members WHERE team_id IN (SELECT id FROM users WHERE kind = 'team' AND display_name LIKE 'Tamsin%')`)
		_, _ = w.pool.Exec(sys, `DELETE FROM users WHERE kind = 'team' AND display_name LIKE 'Tamsin%'`)
	})
	stored := func(name string) int {
		var n int
		if err := w.pool.QueryRow(sys, `SELECT count(*) FROM users WHERE display_name = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	doors := []struct {
		door string
		save func(i int, name string) (int, string)
	}{
		{"invite redemption", func(i int, name string) (int, string) {
			tok := w.createInvite("")
			calls := len(w.fake.Calls())
			rec := do(w.edge, "POST", InvitePath, redeemForm(tok, fmt.Sprintf("dn660-%d@invite.test", i), name, goodPW, goodPW),
				fromIP(fmt.Sprintf("198.51.100.%d", 100+i)))
			if rec.Code == http.StatusBadRequest && len(w.fake.Calls()) != calls {
				t.Errorf("invite: %q refused only after reaching GoTrue", name)
			}
			return rec.Code, rec.Body.String()
		}},
		{"/admin/teams", func(i int, name string) (int, string) {
			rec := do(w.edge, "POST", "/admin/teams", url.Values{"name": {name}, "manager": {"1"}}, w.asAdmin())
			return rec.Code, rec.Body.String()
		}},
	}
	refused := []rune{0x07, 0x1B, 0x85, 0x061C, 0x200B, 0x200F, 0x202E, 0x2067, 0xFEFF}
	accepted := []string{"Tamsin Okafor", "Tamsin 李小龍", "Tamsin محمد", "Tamsin Ленка", "Tamsin 🦊"}
	i := 0
	for _, d := range doors {
		for _, r := range refused {
			i++
			name := fmt.Sprintf("Tamsin%cVale%d", r, i)
			code, body := d.save(i, name)
			want := fmt.Sprintf("U+%04X: control, text-direction and invisible formatting characters are not allowed.", r)
			if code != http.StatusBadRequest || !strings.Contains(html.UnescapeString(body), want) {
				t.Errorf("%s: U+%04X: %d, want 400 saying %q; body %s", d.door, r, code, want, truncate(body, 300))
			}
			if n := stored(name); n != 0 {
				t.Errorf("%s: U+%04X: refused name stored %d time(s)", d.door, r, n)
			}
		}
		for _, base := range accepted {
			i++
			name := fmt.Sprintf("%s %d", base, i)
			if code, body := d.save(i, name); code != http.StatusSeeOther {
				t.Errorf("%s: an ordinary name %q: %d, want 303; body %s", d.door, name, code, truncate(body, 300))
			}
			if n := stored(name); n != 1 {
				t.Errorf("%s: %q stored %d time(s), want once, as typed", d.door, name, n)
			}
		}
	}
}
