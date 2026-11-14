package api

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
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
		// save reports the answer, and whether the save reached GoTrue
		// (a refusal must not: the store refuses before create).
		save func(i int, name string) (int, string, bool)
	}{
		{"invite redemption", func(i int, name string) (int, string, bool) {
			tok := w.createInvite("")
			calls := len(w.fake.Calls())
			rec := do(w.edge, "POST", InvitePath, redeemForm(tok, fmt.Sprintf("dn660-%d@invite.test", i), name, goodPW, goodPW),
				fromIP(fmt.Sprintf("198.51.100.%d", 100+i)))
			return rec.Code, rec.Body.String(), len(w.fake.Calls()) != calls
		}},
		{"/admin/teams", func(i int, name string) (int, string, bool) {
			rec := do(w.edge, "POST", "/admin/teams", url.Values{"name": {name}, "manager": {"1"}}, w.asAdmin())
			return rec.Code, rec.Body.String(), false
		}},
	}
	refused := []rune{0x07, 0x1B, 0x85, 0x061C, 0x200B, 0x200F, 0x202E, 0x2067, 0xFEFF}
	accepted := []string{"Tamsin Okafor", "Tamsin 李小龍", "Tamsin محمد", "Tamsin Ленка", "Tamsin 🦊",
		"Tamsin \U0001F468\u200D\U0001F469\u200D\U0001F467", "Tamsin \u0639\u0644\u06CC\u200C\u0631\u0636\u0627"}
	i := 0
	for _, d := range doors {
		for _, r := range refused {
			i++
			name := fmt.Sprintf("Tamsin%cVale%d", r, i)
			code, body, reached := d.save(i, name)
			want := fmt.Sprintf("U+%04X: control, text-direction and invisible formatting characters are not allowed.", r)
			if reached {
				t.Errorf("%s: U+%04X refused only after reaching GoTrue", d.door, r)
			}
			if d.door == "invite redemption" {
				// The form comes back with the name field EMPTY: what was
				// refused is invisible, so it would only be sent again (#666 G1).
				m := inviteNameValue.FindStringSubmatch(body)
				if m == nil || m[1] != "" {
					t.Errorf("invite: U+%04X: the name field is not drawn empty: %q", r, m)
				}
			}
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
			if code, body, _ := d.save(i, name); code != http.StatusSeeOther {
				t.Errorf("%s: an ordinary name %q: %d, want 303; body %s", d.door, name, code, truncate(body, 300))
			}
			if n := stored(name); n != 1 {
				t.Errorf("%s: %q stored %d time(s), want once, as typed", d.door, name, n)
			}
		}
		// Bytes that are not UTF-8 are refused with a 400 before GoTrue,
		// not a database error after it (#666 G2).
		i++
		name := fmt.Sprintf("Tamsin\xffVale%d", i)
		code, body, reached := d.save(i, name)
		if code != http.StatusBadRequest || !strings.Contains(html.UnescapeString(body), "is not valid text: it holds bytes that are not UTF-8.") || reached {
			t.Errorf("%s: invalid UTF-8: %d (reached GoTrue: %t), want 400 saying it is not valid text; body %s", d.door, code, reached, truncate(body, 300))
		}
	}
}

// inviteNameValue is the invite form's name field's value attribute.
var inviteNameValue = regexp.MustCompile(`<input id="invite-name"[^>]*\svalue="([^"]*)"`)
