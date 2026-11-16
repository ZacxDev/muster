package api

import (
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// "LET MY AGENTS SHARE PAGES WITHOUT ASKING" — EACH OWNER'S OWN OPT-IN
// (Account → Pages; notify_preferences.pages_agent_share, migration 0135;
// docs/pages.md#when-an-agent-shares).
//
// Off — every user until they turn it on — an agent's page_share_request
// files a pending link and a card only the owner's own door approves
// (page_share.go). On, page_share_request for THAT OWNER'S pages makes the
// link live at once and tells them (page_tools.go pageShareAuto).
//
// 🔴 ONLY THE OWNER FLIPS IT, AND ONLY THEIR OWN: the route is a signed-in
// session's same-origin write (requireOperatorWrite — no agent token, no
// mirror token reaches it), it writes requestOwner's row and nobody else's
// (row-level security refuses another's), and the tool reads it for the
// PAGE's owner (pageAgentShareOn), never the caller's.
// ---------------------------------------------------------------------------

// accountPagesAgentSharePath is the switch's POST (on=1|0).
const accountPagesAgentSharePath = "/account/pages/agent-share"

// pagesAgentShareID is the switch's DOM id, anchor and data-testid.
const pagesAgentShareID = "pages-agent-share"

// Its words.
const (
	pagesAgentShareLabel = "Let my agents share pages without asking"
	pagesAgentShareHelp  = "When one of your agents shares a page, its link goes live at once — anyone the agent gives it to can open the page — " +
		"instead of waiting for you on Asks. You are told every time an agent shares a page — a new link or one you had already shared (a notification and the page's card in the chat), and can revoke it in one tap. " +
		"Off, you approve every link an agent asks for."
)

// pagesAgentShareSwitch is the switch in notifySwitch's shape (its drawing is
// notifySwitchHTML's).
func (s *Server) pagesAgentShareSwitch() (notifySwitch, bool) {
	p := s.ext.NotifyPrefs
	if p == nil || !s.pagesOn() {
		return notifySwitch{}, false
	}
	return notifySwitch{id: pagesAgentShareID, label: pagesAgentShareLabel, help: pagesAgentShareHelp, path: accountPagesAgentSharePath,
		saved: "pages-saved", get: p.PagesAgentShare, set: p.SetPagesAgentShare}, true
}

// accountPagesSection is the Account page's Pages section: the opt-in. ""
// with pages off, no store, or no signed-in user.
func (s *Server) accountPagesSection(r *http.Request) string {
	sw, ok := s.pagesAgentShareSwitch()
	owner := requestOwner(r.Context())
	if !ok || owner <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<section id="pages" data-testid="pages-settings" class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h2 class="text-base font-semibold text-fg">Pages</h2>
`)
	on, err := sw.get(r.Context(), owner)
	if err != nil {
		s.logger.Printf("pages: account: owner %d: read the share-without-asking opt-in: %v", owner, err)
		b.WriteString(problemNode("The pages setting could not be read."))
	} else {
		saved := ""
		if r.URL.Query().Get("notify") == sw.saved {
			saved = `<p role="status" class="text-sm font-semibold text-fg">Saved.</p>
`
		}
		b.WriteString(notifySwitchHTML(sw, on, saved))
	}
	b.WriteString(`</section>`)
	return b.String()
}

// handleAccountPagesAgentShare is POST /account/pages/agent-share (on=1|0):
// the caller's own opt-in, back to the Account page at the switch.
func (s *Server) handleAccountPagesAgentShare(w http.ResponseWriter, r *http.Request) {
	owner := requestOwner(r.Context())
	if owner <= 0 {
		http.Error(w, "no user", http.StatusForbidden)
		return
	}
	sw, ok := s.pagesAgentShareSwitch()
	if !ok {
		http.Error(w, "shareable pages are off on this server, or it has no database to keep the setting in", http.StatusConflict)
		return
	}
	var on bool
	switch r.PostFormValue("on") {
	case "1":
		on = true
	case "0":
	default:
		http.Error(w, "on is 1 or 0", http.StatusBadRequest)
		return
	}
	if err := sw.set(r.Context(), owner, on); err != nil {
		s.logger.Printf("pages: owner %d: save the share-without-asking opt-in: %v", owner, err)
		http.Error(w, "the setting could not be saved", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("pages: owner %d turned %s letting their agents share pages without asking", owner, map[bool]string{true: "ON", false: "off"}[on])
	http.Redirect(w, r, accountPath+"?notify="+sw.saved+"#"+sw.id, http.StatusSeeOther)
}
