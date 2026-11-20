package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// AN OFFICER BY NAME (the operator rule: no id in muster's top layer). An
// officer's id is a "<type>:<area>" slug ("marketing:acme"); where a surface
// has no name for it, it calls the officer by the slug's words —
// "Marketing — Acme" — and keeps the id itself for an ⓘ. One reading of the
// slug, so the officers list, a plugin's "Delivers to" and an ask's source
// cannot call one officer three things.

// officerWords is id's parts in words: each ":"-separated part with its
// hyphens and underscores as spaces and its first rune upper-cased.
func officerWords(id string) []string {
	var out []string
	for _, p := range strings.Split(strings.TrimSpace(id), ":") {
		p = strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(p))
		if p == "" {
			continue
		}
		r, n := utf8.DecodeRuneInString(p)
		out = append(out, string(unicode.ToUpper(r))+p[n:])
	}
	return out
}

// OfficerPlainName is what muster calls an officer: name when it has one,
// else id in words ("marketing:acme" → "Marketing — Acme"); "Officer" for an
// id with no words at all.
func OfficerPlainName(name, id string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	if w := officerWords(id); len(w) > 0 {
		return strings.Join(w, " — ")
	}
	return "Officer"
}

// OfficerSourceName is an officer as the source of an ask or a draft: "<Type>
// officer (<Area>)" — "Product officer (Acme)" — so a card says it was an
// officer that asked, by what it is for.
func OfficerSourceName(id string) string {
	w := officerWords(id)
	switch len(w) {
	case 0:
		return "An officer"
	case 1:
		return w[0] + " officer"
	}
	return w[0] + " officer (" + strings.Join(w[1:], " — ") + ")"
}
