package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// LENDING A HOST SPAWN TO A TEAM (docs/design/team-sharing.md, phase 3;
// migration 0130).
//
// officer_host_spawns (#649) stays PERSONAL BY DEFAULT: a row is its owner's,
// under owner_isolation. Its owner may lend one row to ONE team they are an
// operator or manager of; that team's members (viewer and above) then read it
// on /officers/spawns, and nobody but the owner writes it. Unlending puts it
// back.
//
// Two ways in, ONE gate (lendHostSpawn):
//   - the web page, /officers/spawns (the session tier; its writes are
//     same-origin forms);
//   - the chief's tool, officer_spawn_share (chief-only; in-process for a
//     gateway chief, on MCP for a claude-code chief).
//
// 🔴 THE GATE, in order:
//  1. the DOOR is the owner's browser session or the owner's own chief
//     (spawnLendDoors): its MCP door (ViaAgent), a gateway chief's
//     machine-message turn (ViaAgentMessage) and its kickoff (ViaKickoff).
//     A personal API token NEVER lends DIRECTLY (no route admits it here), nor
//     does a mirror host, a device, a plugin or any background loop.
//     🔴 OPERATOR-APPROVED EXCEPTION to team sharing 2a's "a personal API
//     token carries no team access" (#668, after round 1): a token — or the
//     hook token — that MESSAGES THE CHIEF can have the chief lend or unlend
//     the OWNER'S OWN spawn rows to a team the owner operates. That is the
//     whole exception: it reads nothing of the team's, and it does not reach
//     team drafts (draftDeciderRefusal reads TeamScope, empty on every agent
//     door) or team charters (session pages only);
//  2. the principal is not a team (a team's own spawns are already the
//     team's, and the team is nobody's member). Reachable: a team-owned
//     chief's own token acts as the team acting principal on ViaAgent;
//  3. a lend names a team the person is an operator or manager of, read from
//     the principal's memberships (Principal.Teams, read with the role on
//     every request — not TeamScope, which is empty on the chief's doors:
//     lending reads nothing of the team's, it shows the team one of the
//     person's own rows);
//  4. the database: the UPDATE names the acting person as the owner, and
//     0130's lend_target trigger refuses a lend to a team the person does not
//     operate (read from team_members itself), so 1-3 are not the only guard.
//
// Unlending (team 0) passes 1 and 2 and needs no role: the owner can always
// take a row back, even from a team they have left (internal/users clears a
// person's lends to a team when they leave it or drop to viewer).
// ---------------------------------------------------------------------------

// spawnLendDoors are the doors lendHostSpawn admits: the owner's browser
// session, and the owner's own chief — its MCP door and a gateway chief's
// machine-message and kickoff turns (the operator-approved exception above).
// TestTheSpawnLendDoorsAreExactlyTheListedDoors pins it against every request
// door; TestATokenDrivenChiefTurnCanLendTheOwnersOwnSpawn drives the chief's
// tool through the machine-message and agent doors.
var spawnLendDoors = map[authz.Via]bool{
	authz.ViaSession: true, authz.ViaAgent: true, authz.ViaKickoff: true, authz.ViaAgentMessage: true,
}

// The lend gate's refusals.
var (
	// ErrLendDoor: a door outside spawnLendDoors — a personal API token above all.
	ErrLendDoor = errors.New("a host spawn is shared with a team only from the Officer spawns page or by your chief; " +
		"a personal API token directly, a machine credential or a background loop never shares one")
	// ErrLendTeamPrincipal: the principal is a team.
	ErrLendTeamPrincipal = errors.New("a team's own host spawns are already the team's; only a person lends one")
	// ErrLendRole: the person is not an operator or manager of the team.
	ErrLendRole = errors.New("you can share a host spawn only with a team you are an operator or manager of")
	// ErrLendOff: no database.
	ErrLendOff = errors.New("host spawns are not kept on this muster (it has no database)")
)

// EnableHostSpawnLending turns on /officers/spawns and officer_spawn_share
// over st (cmd/muster-server wires it beside the database).
func (s *Server) EnableHostSpawnLending(st officers.HostSpawnLender) { s.spawnLender = st }

// lendHostSpawn is THE GATE (above): lend row id of the principal's own to
// team, or unlend it when team is 0.
func (s *Server) lendHostSpawn(ctx context.Context, id, team int64) (officers.HostSpawn, error) {
	p := authz.MustFrom(ctx)
	if !spawnLendDoors[p.Via] {
		return officers.HostSpawn{}, ErrLendDoor
	}
	if p.Team {
		return officers.HostSpawn{}, ErrLendTeamPrincipal
	}
	if team > 0 {
		role, in := p.Teams.RoleIn(team)
		if !in || (role != authz.TeamOperator && role != authz.TeamManager) {
			return officers.HostSpawn{}, ErrLendRole
		}
	}
	if s.spawnLender == nil {
		return officers.HostSpawn{}, ErrLendOff
	}
	// The team an unlend takes it back from, for the audit row (a row the
	// principal cannot read is the store's "no such spawn" below).
	prior, _ := s.spawnLender.HostSpawnByID(ctx, id)
	h, err := s.spawnLender.LendHostSpawn(ctx, id, team)
	if err != nil {
		return h, err
	}
	s.logger.Printf("officers: user %d set host spawn %d's team to %d (via %s)", p.UserID, id, team, p.Via)
	// 🔴 AN audit_log ROW, naming the person and the team (design §4; #668
	// round 0 choice 12): the team's managers see it on /teams/{id}.
	by, detail := users.Actor{UserID: p.UserID, Via: p.Via}, map[string]any{"officer": h.Officer, "instance": h.Instance, "host": h.Host}
	// A lend MOVED from one team to another takes it from the first: that
	// team's managers get the unshare row too (#668 round 1, G2).
	if prior.SharedTeam > 0 && prior.SharedTeam != team {
		s.auditTeam(ctx, by, prior.SharedTeam, "officer.spawn.unshare", hostSpawnObject(id), detail)
	}
	if team > 0 && team != prior.SharedTeam {
		s.auditTeam(ctx, by, team, "officer.spawn.share", hostSpawnObject(id), detail)
	}
	return h, err
}

// hostSpawnObject is a host spawn's audit_log object.
func hostSpawnObject(id int64) string { return "officer_host_spawn:" + strconv.FormatInt(id, 10) }

// --- /officers/spawns ---------------------------------------------------------

const officerSpawnsPath = "/officers/spawns"

// spawnPageWindow is how many of the newest rows each section of
// /officers/spawns draws: the personal section, and each team's own.
const spawnPageWindow = 200

func (s *Server) registerOfficerSpawnPages(mux Mux) {
	mux.HandleFunc("GET "+officerSpawnsPath, s.requireSession(s.handleOfficerSpawnsPage))
	mux.HandleFunc("POST "+officerSpawnsPath+"/{id}/share", s.requireOperatorWrite(s.handleOfficerSpawnShare))
	mux.HandleFunc("POST "+officerSpawnsPath+"/{id}/unshare", s.requireOperatorWrite(s.handleOfficerSpawnUnshare))
}

func (s *Server) handleOfficerSpawnsPage(w http.ResponseWriter, r *http.Request) {
	s.renderOfficerSpawns(w, r, http.StatusOK, "")
}

func (s *Server) renderOfficerSpawns(w http.ResponseWriter, r *http.Request, code int, problem string) {
	ctx := r.Context()
	p := authz.MustFrom(ctx)
	if s.spawnLender == nil {
		s.renderNotice(w, r, http.StatusNotFound, "No officer spawns", ErrLendOff.Error())
		return
	}
	var teamIDs []int64
	for _, m := range p.Teams.List() {
		teamIDs = append(teamIDs, m.Team)
	}
	// 🔴 ONE BOUNDED WINDOW PER SECTION (#729 round 1, F1): the personal
	// section (the viewer's own rows and the rows lent to their teams) reads
	// its own newest spawnPageWindow, and each team's own rows theirs — a
	// busy team host officer never pushes a member's own lend, and its Stop
	// sharing form, off the page.
	own, err := s.spawnLender.VisibleHostSpawns(ctx, officers.SpawnWindow{NotOwnedBy: teamIDs}, spawnPageWindow)
	teamOwned := map[int64][]officers.HostSpawn{}
	for _, t := range teamIDs {
		if err != nil {
			break
		}
		teamOwned[t], err = s.spawnLender.VisibleHostSpawns(ctx, officers.SpawnWindow{Team: t}, spawnPageWindow)
	}
	if err != nil {
		s.logger.Printf("officers: read host spawns: %v", err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load officer spawns", "Try again in a moment.")
		return
	}
	// team_tasks.go's one team-name read (#664): "Team <id>" when unnamed.
	names := s.teamNames(ctx, teamIDs)
	var lendable []authz.Membership
	for _, m := range p.Teams.List() {
		if m.Role == authz.TeamOperator || m.Role == authz.TeamManager {
			lendable = append(lendable, m)
		}
	}
	// 🔴 A TEAM'S OWN SPAWNS (team sharing 2b.5): a team host officer's spawn
	// rows are owned by the team, and its members read them at viewer
	// (team_owner_read, migration 0133). They are drawn under the team, and
	// are nobody's to lend (lend_target: the actor is not the owner): only
	// the team acting principal (the officer runtime) writes them. A row is
	// the TEAM'S when its owner is one of the viewer's teams — never a row
	// merely lent to that team, whose owner is a person.
	var b strings.Builder
	b.WriteString(problemNode(problem))
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">Officer spawns</h1>
<p class="text-sm text-fg2">The Claude Code sessions muster asked a host to start for your host officers. Each is yours alone until you share it with a team you operate; its members can then see it, and only you can change it.</p>`)
	if len(own) == 0 {
		b.WriteString(`<p class="text-sm text-fg2" id="officer-spawns-empty">No officer spawns yet.</p>`)
	}
	b.WriteString(`<ul class="flex flex-col gap-3" id="officer-spawns">`)
	for _, h := range own {
		id := strconv.FormatInt(h.ID, 10)
		mine := h.OwnerID == p.UserID
		b.WriteString(`<li class="flex flex-col gap-2" data-spawn-id="` + id + `" data-spawn-team="` + strconv.FormatInt(h.SharedTeam, 10) +
			`" data-spawn-mine="` + strconv.FormatBool(mine) + `">` + spawnLine(h))
		switch {
		case !mine:
			b.WriteString(`<span class="text-xs text-fg2">Shared with ` + html.EscapeString(names[h.SharedTeam]) + ` by a teammate (read-only).</span>`)
		case h.SharedTeam > 0:
			b.WriteString(`<span class="text-xs text-fg2">Shared with ` + html.EscapeString(names[h.SharedTeam]) + `.</span>
<form method="POST" action="` + officerSpawnsPath + `/` + id + `/unshare"><button type="submit" class="` + adminQuiet + `">Stop sharing</button></form>`)
		case len(lendable) > 0:
			b.WriteString(`<form method="POST" action="` + officerSpawnsPath + `/` + id + `/share" class="flex flex-wrap items-center gap-2">
<label class="sr-only" for="spawn-team-` + id + `">Team</label><select id="spawn-team-` + id + `" name="team" class="` + adminInput + `">`)
			for _, m := range lendable {
				b.WriteString(`<option value="` + strconv.FormatInt(m.Team, 10) + `">` + html.EscapeString(names[m.Team]) + `</option>`)
			}
			b.WriteString(`</select><button type="submit" class="` + adminQuiet + `">Share with team</button></form>`)
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></section>`)
	for _, m := range p.Teams.List() {
		list := teamOwned[m.Team]
		if len(list) == 0 {
			continue
		}
		team := strconv.FormatInt(m.Team, 10)
		b.WriteString(`<section class="` + adminCard + `" id="officer-spawns-team-` + team + `" data-spawn-owner-team="` + team + `">
<h2 class="text-base font-semibold text-fg">` + html.EscapeString(names[m.Team]) + `'s officers</h2>
<p class="text-sm text-fg2">The sessions muster started on the team's hosts for the team's host officers. Every member sees them; nobody changes them here.</p>
<ul class="flex flex-col gap-3">`)
		for _, h := range list {
			b.WriteString(`<li class="flex flex-col gap-2" data-spawn-id="` + strconv.FormatInt(h.ID, 10) + `" data-spawn-owner-team="` + team +
				`" data-spawn-mine="false">` + spawnLine(h) + `</li>`)
		}
		b.WriteString(`</ul></section>`)
	}
	s.writeNavDocument(w, r, code, "Officer spawns", ui.NavAccount, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// spawnLine is spawn h's officer, instance, host, state and time, drawn.
func spawnLine(h officers.HostSpawn) string {
	return `<span class="text-sm text-fg">` + html.EscapeString(h.Officer+" · "+h.Instance+" on "+h.Host) + `</span><span class="text-xs text-fg2">` +
		html.EscapeString(spawnState(h)) + ` · ` + fmtTime(h.CreatedAt) + `</span>`
}

// spawnState is a spawn's state in words.
func spawnState(h officers.HostSpawn) string {
	switch {
	case h.Refusal != "":
		return "refused by the host: " + h.Refusal
	case h.Window != "":
		return "running in window " + h.Window
	}
	return "waiting for the host"
}

func (s *Server) handleOfficerSpawnShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	team, tok := formID(r, "team")
	if !ok || !tok {
		s.renderOfficerSpawns(w, r, http.StatusBadRequest, "Choose a team to share it with.")
		return
	}
	s.officerSpawnLendDone(w, r, id, team)
}

func (s *Server) handleOfficerSpawnUnshare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.renderOfficerSpawns(w, r, http.StatusNotFound, "No such officer spawn.")
		return
	}
	s.officerSpawnLendDone(w, r, id, 0)
}

func (s *Server) officerSpawnLendDone(w http.ResponseWriter, r *http.Request, id, team int64) {
	_, err := s.lendHostSpawn(r.Context(), id, team)
	if err == nil {
		http.Redirect(w, r, officerSpawnsPath, http.StatusSeeOther)
		return
	}
	code, msg := lendRefusal(err)
	if code == http.StatusInternalServerError {
		s.logger.Printf("officers: lend host spawn %d to %d: %v", id, team, err)
	}
	s.renderOfficerSpawns(w, r, code, msg)
}

// lendRefusal is a refused lend's status and words.
func lendRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, officers.ErrHostSpawnNotFound):
		return http.StatusNotFound, "No officer spawn of yours has that id."
	case errors.Is(err, ErrLendRole), errors.Is(err, officers.ErrLendRefused):
		return http.StatusForbidden, ErrLendRole.Error() + "."
	case errors.Is(err, ErrLendDoor), errors.Is(err, ErrLendTeamPrincipal):
		return http.StatusForbidden, err.Error() + "."
	case errors.Is(err, ErrLendOff):
		return http.StatusNotFound, err.Error() + "."
	}
	return http.StatusInternalServerError, "That change could not be made. Try again."
}

// --- the chief's tool -----------------------------------------------------------

const officerSpawnShareTool = "officer_spawn_share"

func officerSpawnShareToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: officerSpawnShareTool,
		Description: "Your owner's host officer spawns (the Claude Code sessions muster started on a host for their host officers): " +
			"list them, or share one with a team your owner operates, or stop sharing it. A shared spawn is READ by that team's " +
			"members; only your owner ever changes it. Do it only when your owner asked you to. " +
			"action list: the newest 50 of your owner's own spawns, each with its id and the team it is shared with (0 for none). " +
			"action share: needs id and team_id (a team your owner is an operator or manager of). action unshare: needs id.",
		Parameters: objSchema(map[string]any{
			"action":  map[string]any{"type": "string", "enum": []string{"list", "share", "unshare"}},
			"id":      intProp("share or unshare: the spawn's id, as list returns it.", 1, 1<<53),
			"team_id": intProp("share: the team's id.", 1, 1<<53),
		}, "action")}
}

// toolOfficerSpawnShare is officer_spawn_share, for the chief (chief-only:
// dispatchAgentTool re-checks it). Every write goes through lendHostSpawn.
func (s *Server) toolOfficerSpawnShare(ctx context.Context, arguments string) string {
	var in struct {
		Action string `json:"action"`
		ID     int64  `json:"id"`
		TeamID int64  `json:"team_id"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("arguments are {action: list | share | unshare, id?, team_id?}")
	}
	view := func(h officers.HostSpawn) map[string]any {
		return map[string]any{"id": h.ID, "officer": h.Officer, "instance": h.Instance, "host": h.Host,
			"state": spawnState(h), "shared_team_id": h.SharedTeam, "created_at": h.CreatedAt}
	}
	switch in.Action {
	case "list":
		if s.spawnLender == nil {
			return toolErr("%s", ErrLendOff.Error())
		}
		// The chief's door carries no teams, so this reads its owner's own
		// rows only.
		rows, err := s.spawnLender.VisibleHostSpawns(ctx, officers.SpawnWindow{}, 50)
		if err != nil {
			return toolErr("could not read the spawns")
		}
		out := make([]map[string]any, 0, len(rows))
		for _, h := range rows {
			out = append(out, view(h))
		}
		return toolJSON(map[string]any{"spawns": out})
	case "share", "unshare":
		if in.ID <= 0 || (in.Action == "share") != (in.TeamID > 0) {
			return toolErr("share takes id and team_id; unshare takes id only")
		}
		team := in.TeamID
		if in.Action == "unshare" {
			team = 0
		}
		h, err := s.lendHostSpawn(ctx, in.ID, team)
		if err != nil {
			_, msg := lendRefusal(err)
			return toolErr("%s", msg)
		}
		return toolJSON(view(h))
	}
	return toolErr("action must be list, share or unshare, not %q", in.Action)
}
