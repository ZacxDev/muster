package ui

import "testing"

// TestAnOfficerIsCalledByItsWords: an officer's id is read into words one
// way — its name when it has one, else "Type — Area" — and as an ask's or a
// draft's source "Type officer (Area)".
func TestAnOfficerIsCalledByItsWords(t *testing.T) {
	for _, c := range []struct{ name, id, plain, source string }{
		{"", "marketing:acme", "Marketing — Acme", "Marketing officer (Acme)"},
		{"", "product:acme-api", "Product — Acme api", "Product officer (Acme api)"},
		{"", "minder", "Minder", "Minder officer"},
		{"Launch desk", "growth:atlas", "Launch desk", "Growth officer (Atlas)"},
		{"", "", "Officer", "An officer"},
	} {
		if got := OfficerPlainName(c.name, c.id); got != c.plain {
			t.Errorf("OfficerPlainName(%q, %q) = %q, want %q", c.name, c.id, got, c.plain)
		}
		if got := OfficerSourceName(c.id); got != c.source {
			t.Errorf("OfficerSourceName(%q) = %q, want %q", c.id, got, c.source)
		}
	}
}
