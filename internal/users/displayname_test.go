package users

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/branding"
)

// Issue #660: a display name (users.display_name) holding a control or a
// text-direction / invisible formatting character is refused when it is
// saved, by every writer — the same rule as the instance name
// (branding.CheckNameText). Existing rows are not rewritten.

// displayNameWriter saves name through one writer and reports the error,
// and whether a row holding name now exists (a refusal must leave none).
type displayNameWriter struct {
	name  string
	write func(t *testing.T, s *PGStore, i int, name string) (stored bool, err error)
}

func displayNameWriters() []displayNameWriter {
	return []displayNameWriter{
		{"users.RedeemInvite", func(t *testing.T, s *PGStore, i int, name string) (bool, error) {
			ctx := context.Background()
			tok := fmt.Sprintf("dn660-%d", i)
			if _, err := s.CreateInvite(ctx, opActor, hash(tok), "", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			called := false
			sub := fmt.Sprintf("660%05d-0000-4000-a000-000000000000", i)
			u, err := s.RedeemInvite(ctx, hash(tok), fmt.Sprintf("dn660-%d@example.test", i), name,
				func(context.Context) (string, error) { called = true; return sub, nil }, nil)
			if err != nil && called {
				t.Errorf("RedeemInvite(%q) refused, but only AFTER creating the GoTrue user", name)
			}
			return err == nil && u.DisplayName != "", err
		}},
		{"users.CreateTeam", func(t *testing.T, s *PGStore, i int, name string) (bool, error) {
			tm, err := s.CreateTeam(context.Background(), admin, name, authz.OperatorUserID)
			return err == nil && tm.Name != "", err
		}},
	}
}

// displayNameRefused is every refused range, at its edges and inside it. The
// names around each character are ordinary letters, so only the character
// can be what is refused, and each case's character differs from every
// other's (an error naming a hardcoded character fails all but one).
var displayNameRefused = []struct {
	why string
	r   rune
}{
	{"C0 NUL", 0x00},
	{"C0 BEL", 0x07},
	{"C0 tab", 0x09},
	{"C0 newline", 0x0A},
	{"C0 ESC", 0x1B},
	{"C0 US (last C0)", 0x1F},
	{"DEL", 0x7F},
	{"C1 first", 0x80},
	{"C1 NEL", 0x85},
	{"C1 last", 0x9F},
	{"Arabic letter mark", 0x061C},
	{"zero-width space", 0x200B},
	{"left-to-right mark", 0x200E},
	{"right-to-left mark", 0x200F},
	{"LRE (first embedding)", 0x202A},
	{"RLO", 0x202E},
	{"LRI (first isolate)", 0x2066},
	{"PDI (last isolate)", 0x2069},
	{"BOM / ZWNBSP", 0xFEFF},
}

// displayNameAccepted are ordinary names, kept as written: non-Latin
// scripts, combining marks, single emoji, and the two joiners (ZWJ in an
// emoji sequence, ZWNJ in Persian), which are allowed.
var displayNameAccepted = []string{
	"Wren Halvorsen",
	"Zofia Wąsowska-Krol",
	"Ngozi Adébáyọ̀",
	"Lenka Орлова",
	"李小龍",
	"山田 花子",
	"김민지",
	"محمد الفاسي",
	"דנה לוי",
	"दीपिका राव",
	"Kai 🦊",
	"🌻",
	// The joiners are allowed (#666): a ZWJ emoji sequence and a Persian
	// name spelled with ZWNJ, written as code points so they can be seen.
	"Rui \U0001F468\u200D\U0001F469\u200D\U0001F467",                                              // family: man, woman, girl
	"Rui \U0001F9D1\u200D\U0001F4BB",                                                              // technologist
	"\u0639\u0644\u06CC\u200C\u0631\u0636\u0627 \u0645\u06CC\u200C\u062E\u0648\u0627\u0647\u0645", // Alireza, with ZWNJ
}

func TestEveryDisplayNameWriterRefusesControlAndFormatCharacters(t *testing.T) {
	s, pool := storeFor(t)
	i := 0
	for _, w := range displayNameWriters() {
		for _, c := range displayNameRefused {
			i++
			name := fmt.Sprintf("Quill%cBrandt%d", c.r, i)
			stored, err := w.write(t, s, i, name)
			var ce *branding.NameCharError
			switch {
			case !errors.Is(err, branding.ErrNameControl) || !errors.As(err, &ce):
				t.Errorf("%s: %s (U+%04X) not refused with name_control: err=%v", w.name, c.why, c.r, err)
			case ce.Rune != c.r:
				t.Errorf("%s: %s refused, but naming U+%04X, want U+%04X", w.name, c.why, ce.Rune, c.r)
			case stored:
				t.Errorf("%s: %s refused AND stored", w.name, c.why)
			}
			if c.r == 0 {
				continue // Postgres refuses a NUL in any text value, so there is nothing to look up.
			}
			var n int
			if err := pool.QueryRow(authz.System(context.Background()),
				`SELECT count(*) FROM users WHERE display_name = $1`, name).Scan(&n); err != nil || n != 0 {
				t.Errorf("%s: %s: %d row(s) hold the refused name (%v)", w.name, c.why, n, err)
			}
		}
		for _, name := range displayNameAccepted {
			i++
			// A team name is unique, so each writer's copy is told apart.
			full := fmt.Sprintf("%s %d", name, i)
			if stored, err := w.write(t, s, i, full); err != nil || !stored {
				t.Errorf("%s: an ordinary name %q was refused: %v", w.name, full, err)
				continue
			}
			var n int
			if err := pool.QueryRow(authz.System(context.Background()),
				`SELECT count(*) FROM users WHERE display_name = $1`, full).Scan(&n); err != nil || n != 1 {
				t.Errorf("%s: %q stored %d time(s), want once, unchanged (%v)", w.name, full, n, err)
			}
		}
	}
}

// TestEveryDisplayNameWriterRefusesInvalidUTF8 (#666 G2): bytes that are not
// UTF-8 are refused by the writer itself (branding.ErrNameInvalid), before
// an invite's GoTrue user is created — not by Postgres afterwards.
func TestEveryDisplayNameWriterRefusesInvalidUTF8(t *testing.T) {
	s, _ := storeFor(t)
	for i, w := range displayNameWriters() {
		name := fmt.Sprintf("Quill\xffBrandt%d", 900+i)
		stored, err := w.write(t, s, 900+i, name)
		if !errors.Is(err, branding.ErrNameInvalid) || stored {
			t.Errorf("%s: invalid UTF-8: err=%v stored=%t, want branding.ErrNameInvalid", w.name, err, stored)
		}
	}
}
