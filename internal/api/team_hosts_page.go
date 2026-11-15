package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// /teams/{id}/hosts: A TEAM'S HOST TOKENS (team sharing phase 2b.2;
// docs/design/team-sharing.md, "How a host joins and leaves a team").
//
// A team host is a muster-mirror connected with a TEAM-owned mirror token. On
// this page the team's MANAGER mints one (shown once), revokes one, sees each
// one's last use, whether its host is connected, and its rotate mark ("rotate:
// ben left on <date>": an operator or a manager left, dropped to viewer or was
// disabled — users/team_hosts.go), and confirms a held second connection
// ("replace the connected host?": mirror/teamhold.go).
//
// 🔴 MANAGERS ONLY, AS THE TEAM. The page and its writes take the request's
// memberships (managedTeam: a non-manager gets the 404 a missing team gets),
// then a FRESH role read (teamAdminContext), and run AS THE TEAM: the token
// rows are the team's own (mirror_tokens, owner_id = the team), read and
// written under the team acting principal. Minting, revoking and confirming
// a replacement need a fresh sign-in (stepup.Fresh): each one decides who
// receives every member's step-up'd sends.
// ---------------------------------------------------------------------------

func (s *Server) registerTeamHostPages(mux Mux) {
	mux.HandleFunc("GET /teams/{id}/hosts", s.requireSession(s.handleTeamHostsPage))
	mux.HandleFunc("POST /teams/{id}/hosts", s.requireOperatorWrite(s.handleTeamHostMint))
	mux.HandleFunc("POST /teams/{id}/hosts/{token}/revoke", s.requireOperatorWrite(s.handleTeamHostRevoke))
	mux.HandleFunc("POST /teams/{id}/hosts/replace", s.requireOperatorWrite(s.handleTeamHostReplace))
}

// teamHostTokens is the token store as this page consumes it
// (*mirror.PGStore, asserted from ext.MirrorTokens).
type teamHostTokens interface {
	CreateToken(ctx context.Context, host string) (string, mirror.Token, error)
	ListTokens(ctx context.Context) ([]mirror.Token, error)
	RevokeToken(ctx context.Context, id int64) (bool, error)
}

// teamHostHub is the mirror plugin as this page consumes it
// (*mirror.Service, asserted from ext.Mirror).
type teamHostHub interface {
	HeldReplacements(ctx context.Context) []mirror.HeldReplacement
	ConfirmReplacement(ctx context.Context, host, addr string) error
	HostToken(ctx context.Context, host string) (int64, bool)
}

// teamRotateReader is the users store's rotate-reason read
// (users.PGStore.TeamRotateReason), asserted like teamPushReader.
type teamRotateReader interface {
	TeamRotateReason(ctx context.Context, team int64, at time.Time) (users.RotateReason, error)
}

// The page's answers, one spelling each.
const (
	TeamHostsStepUp      = "Minting, revoking or replacing a team host needs a recent sign-in. Confirm it's you, then try again."
	TeamHostsNoStore     = "This muster has no mirror plugin, so it has no host tokens."
	TeamHostsSameAddress = "This machine can't be told apart from the live host by address; stop the live host first, or replace the token."
	TeamHostsNoHeld      = "No connection from that address is waiting for that host now. Choose one held machine by its address, or the host is no longer connected and its next connection is taken anyway."
	TeamHostsNoToken     = "That token does not exist or is already revoked."
	TeamHostsManager     = "Only a manager of the team manages its host tokens."
	teamHostsThreat      = "A viewer reads everything on a team host's screens and transcripts, including any secret a command printed. An operator who types runs commands as the host's Unix user: an operator on a team host has a shell there. Team members have to trust each other."
	teamHostsRotateOK    = "When an operator or a manager leaves, drops to viewer or is disabled, the team's host tokens are marked rotate: a copy they kept still connects, from any machine. Revoke the marked token and mint a new one."
)

// teamHostsWrite is the gate of the page and its writes: team {id}, managed
// by the caller (the request's memberships, then a FRESH read), and the
// team-acting context the token rows are read and written under.
func (s *Server) teamHostsWrite(w http.ResponseWriter, r *http.Request) (int64, context.Context, bool) {
	team, ok := s.managedTeam(w, r)
	if !ok {
		return 0, nil, false
	}
	tctx, err := s.teamAdminContext(r.Context(), team)
	if errors.Is(err, ErrNotTeamManager) {
		s.renderNotice(w, r, http.StatusForbidden, "Team hosts", TeamHostsManager)
		return 0, nil, false
	}
	if err != nil {
		s.logger.Printf("teams: hosts of team %d: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Team hosts", "Your team role could not be read just now. Try again.")
		return 0, nil, false
	}
	return team, tctx, true
}

// teamHostStores is the page's two stores, or false when the mirror plugin is
// not wired: answered 501, with the page's own "no mirror plugin" card.
func (s *Server) teamHostStores(w http.ResponseWriter, r *http.Request, team int64, tctx context.Context) (teamHostTokens, teamHostHub, bool) {
	toks, ok1 := s.ext.MirrorTokens.(teamHostTokens)
	hub, ok2 := s.ext.Mirror.(teamHostHub)
	if s.ext.MirrorTokens == nil || s.ext.Mirror == nil || !ok1 || !ok2 {
		s.renderTeamHosts(w, r, team, tctx, http.StatusNotImplemented, "", "")
		return nil, nil, false
	}
	return toks, hub, true
}

func (s *Server) handleTeamHostsPage(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamHostsWrite(w, r)
	if !ok {
		return
	}
	s.renderTeamHosts(w, r, team, tctx, http.StatusOK, "", "")
}

// renderTeamHosts draws the page. minted, when set, is a token just minted:
// shown ONCE, here, and never again.
func (s *Server) renderTeamHosts(w http.ResponseWriter, r *http.Request, team int64, tctx context.Context, code int, problem, minted string) {
	toks, ok1 := s.ext.MirrorTokens.(teamHostTokens)
	hub, ok2 := s.ext.Mirror.(teamHostHub)
	if s.ext.MirrorTokens == nil || s.ext.Mirror == nil || !ok1 || !ok2 {
		// The mirror plugin is off (MUSTER_MIRROR): the page says so, in place.
		name := s.teamNames(r.Context(), []int64{team})[team]
		s.writeNavDocument(w, r, code, "Team hosts", ui.NavAccount, true, `<div class="flex flex-col gap-4"><section class="`+adminCard+
			`" id="team-hosts-off"><h1 class="text-base font-semibold text-fg">Team hosts · `+html.EscapeString(name)+`</h1><p class="text-sm text-fg2">`+
			html.EscapeString(TeamHostsNoStore)+`</p></section></div>`)
		return
	}
	list, err := toks.ListTokens(tctx)
	if err != nil {
		s.logger.Printf("teams: list team %d's host tokens: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the team's hosts", "Try again in a moment.")
		return
	}
	id := strconv.FormatInt(team, 10)
	name := s.teamNames(r.Context(), []int64{team})[team]
	var b strings.Builder
	b.WriteString(problemNode(problem))
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">Team hosts · ` + html.EscapeString(name) + `</h1>
<p class="text-sm text-fg2">A team host is a machine (or a Unix user on one) the team treats as shared, running its own muster-mirror with a team token. ` + html.EscapeString(teamHostsThreat) + `</p>
<p class="text-sm text-fg2">` + html.EscapeString(teamHostsRotateOK) + `</p></section>`)
	if minted != "" {
		b.WriteString(`<section class="` + adminCard + `" id="team-host-minted"><h2 class="text-base font-semibold text-fg">New team host token</h2>
<p class="text-sm text-fg2">Shown once. Put it in the muster-mirror token file of the Unix user the team shares on that machine, then restart its unit.</p>
<pre class="text-sm text-fg break-all whitespace-pre-wrap" id="team-host-token">` + html.EscapeString(minted) + `</pre></section>`)
	}
	held := hub.HeldReplacements(tctx)
	if len(held) > 0 {
		b.WriteString(`<section class="` + adminCard + `" id="team-host-held"><h2 class="text-base font-semibold text-fg">Replace the connected host?</h2>
<p class="text-sm text-fg2">Another machine presented this team's token for a host that is already connected. Each such machine is listed below by the address it connected from. It is held: it receives nothing and shows nothing until you confirm. Confirm only the machine you moved the host to, by its address: its next attempt from that address replaces the connected host. An address is all muster can tell machines apart by. Behind a proxy or a Kubernetes NodePort every machine shares one address, so there the confirm cannot tell the moved machine from a kept copy of the token, and an attempt from the live host's own address cannot be confirmed at all. If you do not recognise an address, or cannot tell, stop the live host first (its replacement is then taken at once) or revoke the token and mint a new one.</p><ul class="flex flex-col gap-2">`)
		for _, h := range held {
			b.WriteString(`<li class="flex flex-wrap items-center gap-2" data-held-host="` + html.EscapeString(h.Host) + `" data-held-addr="` + html.EscapeString(h.Addr) +
				`" data-confirmed="` + strconv.FormatBool(h.Confirmed) + `">` +
				`<span class="text-sm text-fg">` + html.EscapeString(h.Host) + ` from ` + html.EscapeString(h.Addr) + `</span><span class="text-sm text-fg2">` +
				strconv.Itoa(h.Attempts) + ` attempt(s), first ` + fmtTime(h.First) + `, last ` + fmtTime(h.Last) + `</span>`)
			switch {
			case h.Confirmed:
				b.WriteString(`<span class="text-sm text-fg2">Confirmed: this machine's next attempt replaces the connected host.</span>`)
			case h.SameAsLive:
				b.WriteString(`<span class="text-sm text-fg2" data-indistinguishable>` + html.EscapeString(TeamHostsSameAddress) + `</span>`)
			default:
				b.WriteString(`<form method="POST" action="/teams/` + id + `/hosts/replace"><input type="hidden" name="host" value="` + html.EscapeString(h.Host) + `">` +
					`<input type="hidden" name="addr" value="` + html.EscapeString(h.Addr) + `">` +
					`<button type="submit" class="` + adminButton + `">Replace with this machine</button></form>`)
			}
			b.WriteString(`</li>`)
		}
		b.WriteString(`</ul></section>`)
	}
	b.WriteString(`<section class="` + adminCard + `"><h2 class="text-base font-semibold text-fg">Tokens</h2><ul class="flex flex-col gap-2" id="team-host-tokens">`)
	live := 0
	for _, t := range list {
		if t.RevokedAt != nil {
			continue
		}
		live++
		tid := strconv.FormatInt(t.ID, 10)
		used := "never used"
		if t.LastUsed != nil {
			used = "last used " + fmtTime(*t.LastUsed)
		}
		online := "not connected"
		if cur, tracked := hub.HostToken(tctx, t.Host); tracked && cur == t.ID {
			online = "connected"
		}
		b.WriteString(`<li class="flex flex-wrap items-center gap-2" data-token-id="` + tid + `" data-rotate="` + strconv.FormatBool(t.RotateAt != nil) + `">` +
			`<span class="text-sm text-fg">` + html.EscapeString(t.Host) + `</span><span class="text-sm text-fg2">token ` + tid + ` · ` + used + ` · ` + online + `</span>`)
		if t.RotateAt != nil {
			b.WriteString(`<span class="text-sm font-medium text-fg" data-rotate-reason>` + html.EscapeString(s.teamRotateLine(r.Context(), team, *t.RotateAt)) + `</span>`)
		}
		b.WriteString(`<form method="POST" action="/teams/` + id + `/hosts/` + tid + `/revoke"><button type="submit" class="` + adminQuiet + `">Revoke</button></form></li>`)
	}
	if live == 0 {
		b.WriteString(`<li class="text-sm text-fg2" id="team-host-tokens-none">This team has no host tokens.</li>`)
	}
	b.WriteString(`</ul></section>
<form method="POST" action="/teams/` + id + `/hosts" class="` + adminCard + `">
<h2 class="text-base font-semibold text-fg">Add a team host</h2>
<label for="team-host-name" class="text-sm font-medium text-fg">Host name (as its muster-mirror names it)</label>
<input id="team-host-name" name="host" required autocomplete="off" class="` + adminInput + `">
<button type="submit" class="` + adminButton + `">Mint a token</button></form>`)
	s.writeNavDocument(w, r, code, "Team hosts", ui.NavAccount, true, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// teamRotateLine is a rotate mark's sentence: "rotate: ben left on <date>",
// from the audit row the mark's transaction wrote (users.TeamRotateReason),
// or "rotate (marked <date>)" when no row explains it.
func (s *Server) teamRotateLine(ctx context.Context, team int64, at time.Time) string {
	if rr, ok := s.gt.Accounts.(teamRotateReader); ok {
		reason, err := rr.TeamRotateReason(ctx, team, at)
		if err == nil {
			who := mirror.OneLine(reason.Name, users.TeamNameMax)
			switch reason.Kind {
			case users.RotateLeft:
				return "rotate: " + who + " left on " + fmtTime(reason.At)
			case users.RotateToViewer:
				return "rotate: " + who + " dropped to viewer on " + fmtTime(reason.At)
			case users.RotateDisabled:
				return "rotate: " + who + " was disabled on " + fmtTime(reason.At)
			}
		} else if !errors.Is(err, users.ErrNoRotateReason) {
			s.logger.Printf("teams: rotate reason for team %d at %s: %v", team, at, err)
		}
	}
	return "rotate (marked " + fmtTime(at) + ")"
}

// teamHostsFresh is the step-up check of the page's writes: false has
// answered (403, with the step-up header).
func (s *Server) teamHostsFresh(w http.ResponseWriter, r *http.Request, team int64, tctx context.Context) bool {
	if stepup.Fresh(r.Context(), s.stepUpNow()) {
		return true
	}
	if isDocumentNavigation(r) {
		w.Header().Set("X-Muster-Step-Up", "required")
		s.renderTeamHosts(w, r, team, tctx, http.StatusForbidden, TeamHostsStepUp, "")
		return false
	}
	refuseStale(w)
	return false
}

// handleTeamHostMint is POST /teams/{id}/hosts (host): mint a team host token
// AS THE TEAM, show it once, and audit it as the manager.
func (s *Server) handleTeamHostMint(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamHostsWrite(w, r)
	if !ok || !s.teamHostsFresh(w, r, team, tctx) {
		return
	}
	toks, _, ok := s.teamHostStores(w, r, team, tctx)
	if !ok {
		return
	}
	host := strings.TrimSpace(r.FormValue("host"))
	if err := mirror.ValidHost(host); err != nil {
		s.renderTeamHosts(w, r, team, tctx, http.StatusBadRequest, err.Error(), "")
		return
	}
	tok, rec, err := toks.CreateToken(tctx, host)
	if err != nil {
		s.logger.Printf("teams: mint a host token for team %d: %v", team, err)
		s.renderTeamHosts(w, r, team, tctx, http.StatusInternalServerError, "The token could not be minted. Try again.", "")
		return
	}
	s.auditTeamLevel(r.Context(), actorOf(r), team, "team.host.token.create", "mirror_token:"+strconv.FormatInt(rec.ID, 10), map[string]any{"host": host}, true)
	s.logger.Printf("teams: manager %d minted host token %d (%s) for team %d", actorOf(r).UserID, rec.ID, host, team)
	// The page is a document: writeDocumentHeaders already says no-store.
	s.renderTeamHosts(w, r, team, tctx, http.StatusOK, "", tok)
}

// handleTeamHostRevoke is POST /teams/{id}/hosts/{token}/revoke: revoke one
// of the team's host tokens AS THE TEAM (a live connection using it ends at
// its next recheck), and audit it as the manager.
func (s *Server) handleTeamHostRevoke(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamHostsWrite(w, r)
	if !ok || !s.teamHostsFresh(w, r, team, tctx) {
		return
	}
	toks, _, ok := s.teamHostStores(w, r, team, tctx)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("token"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	revoked, err := toks.RevokeToken(tctx, id)
	if err != nil {
		s.logger.Printf("teams: revoke host token %d of team %d: %v", id, team, err)
		s.renderTeamHosts(w, r, team, tctx, http.StatusInternalServerError, "The token could not be revoked. Try again.", "")
		return
	}
	if !revoked {
		// Another owner's token is invisible to the team (row-level security):
		// the same answer as a missing one.
		s.renderTeamHosts(w, r, team, tctx, http.StatusNotFound, TeamHostsNoToken, "")
		return
	}
	s.auditTeamLevel(r.Context(), actorOf(r), team, "team.host.token.revoke", "mirror_token:"+strconv.FormatInt(id, 10), nil, true)
	s.logger.Printf("teams: manager %d revoked host token %d of team %d", actorOf(r).UserID, id, team)
	http.Redirect(w, r, "/teams/"+strconv.FormatInt(team, 10)+"/hosts", http.StatusSeeOther)
}

// handleTeamHostReplace is POST /teams/{id}/hosts/replace (host): a manager
// confirms a held second connection — its next attempt replaces the
// connected host (mirror.Hub.ConfirmReplacement) — and it is audited.
func (s *Server) handleTeamHostReplace(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamHostsWrite(w, r)
	if !ok || !s.teamHostsFresh(w, r, team, tctx) {
		return
	}
	_, hub, ok := s.teamHostStores(w, r, team, tctx)
	if !ok {
		return
	}
	// 🔴 THE CONFIRM NAMES ONE HELD ATTEMPT, by its address: there is no
	// confirm without one (mirror/teamhold.go, #685 round 0).
	host, addr := strings.TrimSpace(r.FormValue("host")), strings.TrimSpace(r.FormValue("addr"))
	if mirror.ValidHost(host) != nil || addr == "" {
		s.renderTeamHosts(w, r, team, tctx, http.StatusConflict, TeamHostsNoHeld, "")
		return
	}
	switch err := hub.ConfirmReplacement(tctx, host, addr); {
	case errors.Is(err, mirror.ErrIndistinguishable):
		s.renderTeamHosts(w, r, team, tctx, http.StatusConflict, TeamHostsSameAddress, "")
		return
	case err != nil:
		s.renderTeamHosts(w, r, team, tctx, http.StatusConflict, TeamHostsNoHeld, "")
		return
	}
	s.auditTeamLevel(r.Context(), actorOf(r), team, "team.host.replace", "host:"+host, map[string]any{"addr": addr}, true)
	s.logger.Printf("teams: manager %d confirmed replacing team %d's host %s", actorOf(r).UserID, team, host)
	http.Redirect(w, r, "/teams/"+strconv.FormatInt(team, 10)+"/hosts", http.StatusSeeOther)
}
