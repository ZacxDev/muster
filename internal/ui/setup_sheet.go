package ui

import "strings"

// SETUP BEHIND AN ⓘ (the operator rule: no variable, command or flag in
// muster's top layer, and setup detail is an admin's only). A settings page
// says in a plain sentence that something is not set up (and, to anyone but
// an admin, that their admin can turn it on); SetupSheet beside it holds the
// exact setup (the .env lines, the command) with Copy, for an admin. Anyone
// else's sheet says to ask their admin and carries none of it
// (InfoSheet.Withheld).

// SetupSheet is THE ⓘ of a setup detail on a settings page: id its element id
// (unique in its document), label the trigger's accessible name ("How to turn
// on the chief"), lead the sentence above the code, code the exact setup
// (several lines are drawn and copied as written). Withheld unless admin.
func SetupSheet(id, label, lead, code string, admin bool) InfoSheet {
	s := InfoSheet{ID: id, Label: label, Heading: "How to set it up", Lead: lead, Code: code,
		Multiline: strings.Contains(code, "\n"), Inline: true}
	if !admin {
		s.Lead, s.Withheld = "", true
	}
	return s
}
