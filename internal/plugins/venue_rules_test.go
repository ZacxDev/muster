package plugins

import (
	"errors"
	"strings"
	"testing"
)

// TestXWeightedLength pins X's weighting with literal values: a link is 23
// whatever its length, a weight-1 code point 1, a CJK character or an emoji 2.
func TestXWeightedLength(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"hello", 5},
		{"see https://github.com/example/project now", 31},
		{"see github.com/example/project", 27},
		{"https://example.com/" + strings.Repeat("p", 200), 23},
		{"日本", 4},
		{"ok 🙂", 5},
		{"‘quoted’ — dash", 15}, // U+2018/2019/2014: weight-1 punctuation
		{"", 0},
	} {
		if got := XWeightedLength(c.in); got != c.want {
			t.Errorf("XWeightedLength(%q) = %d; want %d", c.in, got, c.want)
		}
	}
}

// venueErr is CheckVenueRules' refusal text ("" for none), failing the test
// on any error that is not ErrInvalid.
func venueErr(t *testing.T, venue, kind, body string, media int) string {
	t.Helper()
	err := CheckVenueRules(venue, kind, body, media)
	if err == nil {
		return ""
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("CheckVenueRules(%s, %s): %v; want ErrInvalid", venue, kind, err)
	}
	return err.Error()
}

func TestCheckVenueRulesXLength(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	url := "https://github.com/example/project/blob/main/docs/a-long-path-to-a-page.md" // 74 bytes, weighs 23
	sep := "\n" + XThreadSeparator + "\n"
	for _, c := range []struct {
		name, kind, body string
		media            int
		want             string // the exact refusal, "" for accepted
	}{
		{"reply at exactly 280", TargetReply, long(280), 0, ""},
		{"reply at 281 (draft 447's shape)", TargetReply, long(281), 0,
			"plugins: invalid: x part 1 of 1 is 281 characters as X counts them; the limit is 280 (a link counts 23, CJK and emoji 2): " +
				"shorten it (a reply is one part: ---8<--- in a reply is not a split)"},
		{"post at exactly 280", TargetPost, long(280), 0, ""},
		{"post at 281", TargetPost, long(281), 0,
			"plugins: invalid: x part 1 of 1 is 281 characters as X counts them; the limit is 280 (a link counts 23, CJK and emoji 2): " +
				"shorten it, or split the post into a thread with a line that is exactly ---8<---"},
		// 256 + 1 + 23 = 280 by X's count, though 331 characters long.
		{"a link counts 23: 280 with a long link", TargetReply, long(256) + " " + url, 0, ""},
		{"a link counts 23: 281 with a long link", TargetReply, long(257) + " " + url, 0,
			"plugins: invalid: x part 1 of 1 is 281 characters as X counts them; the limit is 280 (a link counts 23, CJK and emoji 2): " +
				"shorten it (a reply is one part: ---8<--- in a reply is not a split)"},
		{"CJK counts 2: 140 is 280", TargetPost, strings.Repeat("日", 140), 0, ""},
		{"CJK counts 2: 141 is 282", TargetPost, strings.Repeat("日", 141), 0,
			"plugins: invalid: x part 1 of 1 is 282 characters as X counts them; the limit is 280 (a link counts 23, CJK and emoji 2): " +
				"shorten it, or split the post into a thread with a line that is exactly ---8<---"},
		{"trailing whitespace is not posted", TargetReply, long(280) + "   \n\n", 0, ""},
		{"thread: each part at 280", TargetPost, long(280) + sep + long(280), 0, ""},
		{"thread: part 2 at 281", TargetPost, long(280) + sep + long(281) + sep + long(3), 0,
			"plugins: invalid: x part 2 of 3 is 281 characters as X counts them; the limit is 280 (a link counts 23, CJK and emoji 2): " +
				"shorten it, or split the post into a thread with a line that is exactly ---8<---"},
		{"thread: a separator with trailing spaces splits", TargetPost, long(200) + "\n" + XThreadSeparator + "  \n" + long(200), 0, ""},
		{"thread: an empty part", TargetPost, "one" + sep + "  \n" + sep + "three", 0,
			"plugins: invalid: x part 2 of 3 is empty: a thread part needs text (parts are separated by a line that is exactly ---8<---)"},
		{"thread: a trailing separator is an empty last part", TargetPost, "one" + sep, 0,
			"plugins: invalid: x part 2 of 2 is empty: a thread part needs text (parts are separated by a line that is exactly ---8<---)"},
		{"a reply is one part: the separator does not split it", TargetReply, long(200) + sep + long(200), 0,
			"plugins: invalid: x part 1 of 1 is 410 characters as X counts them; the limit is 280 (a link counts 23, CJK and emoji 2): " +
				"shorten it (a reply is one part: ---8<--- in a reply is not a split)"},
		{"thread: 25 parts", TargetPost, strings.Repeat("p"+sep, 24) + "p", 0, ""},
		{"thread: 26 parts", TargetPost, strings.Repeat("p"+sep, 25) + "p", 0,
			"plugins: invalid: an x thread has at most 25 parts; this one has 26"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := venueErr(t, VenueX, c.kind, c.body, c.media); got != c.want {
				t.Fatalf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestCheckVenueRulesMedia(t *testing.T) {
	sep := "\n" + XThreadSeparator + "\n"
	for _, c := range []struct {
		name, venue, kind, body string
		media                   int
		want                    string
	}{
		{"x single post: 4 media", VenueX, TargetPost, "one", 4, ""},
		{"x reply: 4 media", VenueX, TargetReply, "one", 4, ""},
		{"x thread: as many media as parts", VenueX, TargetPost, "one" + sep + "two", 2, ""},
		{"x thread: more media than parts", VenueX, TargetPost, "one" + sep + "two", 3,
			"plugins: invalid: an x thread puts media[i] on part i+1: 3 media for 2 parts"},
		{"reddit reply: no media", VenueReddit, TargetReply, "one", 0, ""},
		{"reddit reply: one medium", VenueReddit, TargetReply, "one", 1,
			"plugins: invalid: a reddit reply carries no media: leave media out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := venueErr(t, c.venue, c.kind, c.body, c.media); got != c.want {
				t.Fatalf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// TestCheckVenueRulesLeavesUnknownVenuesAlone: no limit is invented for a
// venue (or a target kind) the rules do not name.
func TestCheckVenueRulesLeavesUnknownVenuesAlone(t *testing.T) {
	long := strings.Repeat("a", 5000)
	for _, c := range []struct{ venue, kind string }{
		{"example.forum", TargetReply}, {"example.forum", TargetPost}, {"xx", TargetPost}, {"X", TargetPost},
		{VenueReddit, TargetPost}, {VenueX, TargetDM},
	} {
		if got := venueErr(t, c.venue, c.kind, long, 1); got != "" {
			t.Errorf("%s %s with a 5000-character body: %s; want no rule", c.venue, c.kind, got)
		}
	}
}
