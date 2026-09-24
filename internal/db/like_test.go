package db

import "testing"

// TestLikeEscape pins the FULL output string for each case rather than asserting
// "contains a backslash".
//
// 🔴 A CONTAINS-STYLE ASSERTION IS WALKABLE BY ESCAPING THE WRONG CHARACTER, and
// the three substitutions here are independent — a function that escaped only
// `%` would satisfy any guard phrased as "metacharacters are escaped" while
// still answering `a_b` with rows containing `axb`. Each case below therefore
// names the exact bytes the SQL side will receive.
func TestLikeEscape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// The whole point of the function: a lone wildcard must stop being
			// a wildcard, or a search box answers "%" with the entire table.
			name: "percent is escaped",
			in:   "%",
			want: `\%`,
		},
		{
			// The quieter half. `_` matches ANY single character, so an
			// unescaped `a_b` silently returns `axb` — a wrong answer that
			// looks like working search, which is why it needs its own case.
			name: "underscore is escaped",
			in:   "a_b",
			want: `a\_b`,
		},
		{
			name: "backslash is escaped",
			in:   `a\b`,
			want: `a\\b`,
		},
		{
			// 🔴 THE ORDERING CASE, AND IT IS THE REASON THIS TEST EXISTS AS
			// MORE THAN THREE ONE-LINERS. The backslash must be doubled FIRST.
			// Escaping `%` first turns `\%` into `\\%`, and the later backslash
			// pass then doubles BOTH of those backslashes to `\\\\%` — an
			// escaped backslash followed by a live wildcard, i.e. the exact
			// bug the function exists to prevent, reintroduced by a statement
			// swap that no single-metacharacter case can see.
			name: "backslash before a percent: escaped first, not twice",
			in:   `\%`,
			want: `\\\%`,
		},
		{
			name: "all three together",
			in:   `100%_of\them`,
			want: `100\%\_of\\them`,
		},
		{
			// A string with nothing to escape must come back byte-identical:
			// a function that prefixed everything would pass every case above.
			name: "ordinary text is untouched",
			in:   "deploy the gateway",
			want: "deploy the gateway",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LikeEscape(c.in); got != c.want {
				t.Fatalf("LikeEscape(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
