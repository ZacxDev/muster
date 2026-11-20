package plugins

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// TestValidAppLabel: a tab label is 1-24 letters, digits, single spaces and
// - . & ', trimmed. Each refused case is its own rule: the length, markup, a
// doubled space, an untrimmed end, a bidi override, a control character and
// an invisible one. The accepted ones are the positive control, a 24-character
// label and a non-ASCII letter among them.
func TestValidAppLabel(t *testing.T) {
	for _, ok := range []string{
		"Meetings", "Q&A 2", "O'Brien notes", "v1.2-beta", "Réunions",
		strings.Repeat("m", MaxAppLabelRunes),
	} {
		if err := ValidAppLabel(ok); err != nil {
			t.Errorf("ValidAppLabel(%q) = %v; want accepted", ok, err)
		}
	}
	for why, bad := range map[string]string{
		"empty":               "",
		"25 characters":       strings.Repeat("m", MaxAppLabelRunes+1),
		"markup":              "<b>Meetings",
		"two spaces":          "Team  calls",
		"a leading space":     " Meetings",
		"a trailing space":    "Meetings ",
		"a bidi override":     "Meet‮ings",
		"a tab":               "Meet\tings",
		"a zero-width joiner": "Meet‍ings",
		"an emoji":            "Meetings 📹",
		"a slash":             "calls/notes",
		"invalid UTF-8":       "Meet\xffings",
	} {
		if err := ValidAppLabel(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidAppLabel(%s, %q) = %v; want ErrInvalid", why, bad, err)
		}
	}
}

// TestValidAppIcon: only a key from AppIcons is a tab icon: not an emoji
// itself, an unknown key or nothing. Every key in the list passes and matches
// 0166's plugins_app_tab pattern, and no key or emoji appears twice.
func TestValidAppIcon(t *testing.T) {
	dbKey := regexp.MustCompile(`^[a-z]{1,16}$`)
	keys, emoji := map[string]bool{}, map[string]bool{}
	for _, i := range AppIcons {
		if err := ValidAppIcon(i.Key); err != nil {
			t.Errorf("ValidAppIcon(%q) = %v; want accepted", i.Key, err)
		}
		if !dbKey.MatchString(i.Key) {
			t.Errorf("icon key %q does not match 0166's ^[a-z]{1,16}$", i.Key)
		}
		if keys[i.Key] || emoji[i.Emoji] {
			t.Errorf("icon %q / %q is listed twice", i.Key, i.Emoji)
		}
		keys[i.Key], emoji[i.Emoji] = true, true
	}
	if len(AppIcons) != 16 {
		t.Errorf("AppIcons has %d entries; the design offers 16", len(AppIcons))
	}
	for _, bad := range []string{"", "skull", "📹", "Video", "video "} {
		if err := ValidAppIcon(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidAppIcon(%q) = %v; want ErrInvalid", bad, err)
		}
	}
}

// TestAPairingCannotAskForApp: POST /plugin/v1/pair refuses `app`, alone or
// beside messages: its origin widens muster's own pages, so the owner picks it
// on /plugins, never a plugin. An INVARIANT GUARD: before `app` existed the
// pairing allowlist refused it too (as any unknown name); it turns red only if
// app is added to the pairable set.
func TestAPairingCannotAskForApp(t *testing.T) {
	for _, caps := range [][]Capability{{CapMessages, CapApp}, {CapApp}} {
		in := PairIn{Name: "example-app", Label: "Example", Owner: "owner@example.invalid", Capabilities: caps}
		if err := in.validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("pairing with %v: %v; want ErrInvalid", caps, err)
		}
	}
	ok := PairIn{Name: "example-app", Label: "Example", Owner: "owner@example.invalid", Capabilities: []Capability{CapMessages}}
	if err := ok.validate(); err != nil {
		t.Fatalf("CONTROL: pairing with messages alone: %v", err)
	}
}
