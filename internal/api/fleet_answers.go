package api

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/cichecks"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/prroute"
)

// ---------------------------------------------------------------------------
// FLEET PROPOSALS, PART 2b (docs/chief.md "Fleet proposals"): the chief's
// session_answer, muster's own ci_result, and route_to_session / route_many /
// end_session_propose filing a card when they are called outside an owner
// turn. Every one is a PROPOSAL: the owner's tap on its card sends it.
//
//   - session_answer {subject, answer_kind, text?, option?, reason}:
//       continue          the fixed "Yes, continue." (answer_rule);
//       merge_when_green  "Yes, merge #<n>. CI is green on <sha12>. …
//                         --match-head-commit <sha40>." — only when the MERGE
//                         CHECK holds (fleetMergeCheck), and again at the tap
//                         and the paste (answer_rule);
//       composed          the chief's own words, at most 4 KiB (answer_composed);
//       pick              ONE OF THE HELD PICKER'S OWN OPTIONS, else refused
//                         (not_an_option); typed into the picker through the
//                         one typing an owner's tap on that picker makes
//                         (typeAskChoices) (answer_pick).
//   - ci_result: the session's latest pull request's head had checks still
//     running when it stopped, and now every one has finished (cichecks):
//     §4.2's fixed text, facts muster read itself.
//   - route / close: the call the owner's turn would have made, as a card.
//
// 🔴 THE CI READ IS THE GITHUB APP'S, GATED AS THE PULL-REQUEST READ IS
// (githubAppForOwner): an owner the App does not admit has no checks read.
// ---------------------------------------------------------------------------

const sessionAnswerTool = "session_answer"

// More refusals.
const (
	fleetRefusedNotAnOption = "not_an_option"
	fleetRefusedNoQuestion  = "no_question"
	fleetRefusedMergeCheck  = "merge_check"
)

func sessionAnswerToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: sessionAnswerTool,
		Description: "PROPOSE an answer to the question one of your owner's stopped sessions is holding. muster puts a card in the " +
			"fleet digest showing exactly what will be sent; NOTHING is sent until your owner taps Approve. answer_kind: continue (the " +
			"fixed \"" + fleet.AnswerContinueText + "\"); merge_when_green (\"Yes, merge #<n>. CI is green on <sha>. Merge only that " +
			"commit …\" — only when the session's own open pull request has green CI on its head, which muster reads itself, and reads " +
			"again at the tap); composed (your own text, at most 4 KiB); pick (for a session showing a picker — AskUserQuestion — " +
			"option MUST be one of that picker's own options, exactly as written; anything else is refused). Returns {action_id, state, refused?, card?}.",
		Parameters: objSchema(map[string]any{
			"subject":     strProp("The session: its fleet id from fleet_list (\"mirror:<key>\" or \"agent:<id>\"), stopped now."),
			"answer_kind": map[string]any{"type": "string", "enum": fleet.AnswerKinds},
			"text":        strProp("Your answer, ONLY with composed (at most 4 KiB)."),
			"option":      strProp("The picker option to pick, ONLY with pick: one of the held question's own option labels."),
			"reason":      strProp("Your one-line reason, shown on the card (at most 300 characters)."),
		}, "subject", "answer_kind", "reason")}
}

func (s *Server) toolSessionAnswer(ctx context.Context, arguments string) string {
	var in struct {
		Subject    string  `json:"subject"`
		AnswerKind string  `json:"answer_kind"`
		Text       *string `json:"text"`
		Option     *string `json:"option"`
		Reason     string  `json:"reason"`
	}
	dec := json.NewDecoder(strings.NewReader(orEmptyJSON(arguments)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || strings.TrimSpace(in.Subject) == "" {
		return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "arguments are {subject, answer_kind, text?, option?, reason}")
	}
	if (in.AnswerKind == "composed") != (in.Text != nil) || (in.AnswerKind == "pick") != (in.Option != nil) {
		return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "text goes with composed and option with pick, and only with them")
	}
	sub, refused, why := s.fleetStoppedSubject(ctx, in.Subject)
	if refused != "" {
		return fleetToolResult(fleet.Action{}, refused, why)
	}
	switch in.AnswerKind {
	case "continue":
		return s.fleetFile(ctx, sub, s.fleetProposalFor(sub, fleet.ClassAnswerRule, fleet.AnswerContinueText, in.Reason))
	case "merge_when_green":
		n, head, why := s.fleetMergeCheck(ctx, sub)
		if why != "" {
			return fleetToolResult(fleet.Action{}, fleetRefusedMergeCheck, why)
		}
		p := s.fleetProposalFor(sub, fleet.ClassAnswerRule, fleet.AnswerMergeText(n, head), in.Reason)
		p.Evidence.PRHeadFull, p.Evidence.PRHead, p.Evidence.MergeCheck = head, head[:12], true
		return s.fleetFile(ctx, sub, p)
	case "composed":
		t, ok := cleanPrompt(*in.Text)
		switch {
		case !ok || len(t) > fleet.MaxCustomBytes:
			return fleetToolResult(fleet.Action{}, fleetRefusedTooLong, "text is at most 4 KiB")
		case t == "":
			return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "text is empty")
		}
		return s.fleetFile(ctx, sub, s.fleetProposalFor(sub, fleet.ClassAnswerComposed, t, in.Reason))
	case "pick":
		entry, why := s.fleetHeldPicker(ctx, sub)
		if why != "" {
			return fleetToolResult(fleet.Action{}, fleetRefusedNoQuestion, why)
		}
		q := entry.Questions[0]
		option := *in.Option
		// 🔴 ONE OF THE PICKER'S OWN OPTIONS, BYTE FOR BYTE: never an answer
		// of the chief's own (the "Type something" row), never a near match.
		found := false
		var labels []string
		for _, o := range q.Options {
			labels = append(labels, o.Label)
			found = found || o.Label == option
		}
		if !found {
			return fleetToolResult(fleet.Action{}, fleetRefusedNotAnOption,
				"option is not one of the held question's options: "+strings.Join(labels, " | "))
		}
		p := s.fleetProposalFor(sub, fleet.ClassAnswerPick, fleet.PickText(option, q.Question), in.Reason)
		p.Evidence.AskID, p.Evidence.AskTool, p.Evidence.Option = entry.ID, entry.Key.ToolUseID, option
		return s.fleetFile(ctx, sub, p)
	}
	return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "answer_kind is one of continue, merge_when_green, composed, pick")
}

// --- merge when green ---------------------------------------------------------------

// fleetMergeCheck is the merge check (docs/design/ask-autoapproval.md "Merge
// when green", steps 1-3): sub is a mirrored session whose latest pull request
// (one muster recorded it creating) is open, read through the GitHub App for
// an owner it admits, with its head read here, and CI GREEN on that head
// (cichecks: every check finished, nothing failed, at least one green). The
// pull request's number and full head, or why not. ⚠ Step 4 (the audit
// ladder's verdict check) is not read in this phase: the answer's text says
// only what muster read.
func (s *Server) fleetMergeCheck(ctx context.Context, sub fleetSubject) (int64, string, string) {
	if sub.kind != chiefnudge.KindMirror || len(sub.prs) == 0 {
		return 0, "", "the session has no pull request muster recorded it creating"
	}
	pr := sub.prs[0]
	if pr.State != fleet.PROpen {
		return 0, "", "its latest pull request is not open (" + pr.State + ")"
	}
	head, ok := s.fleetPRHead(ctx, pr.Repo, pr.Number)
	if !ok {
		return 0, "", "its pull request's head could not be read through the GitHub App"
	}
	sum, ok := s.fleetCIRead(ctx, pr.Repo, head)
	switch {
	case !ok:
		return 0, "", "its CI could not be read through the GitHub App"
	case !sum.AllGreen():
		return 0, "", "its CI is not green on " + head[:12] + " (" + strconv.Itoa(sum.Pending) + " pending, " + strconv.Itoa(sum.Failed) + " failed, " +
			strconv.Itoa(sum.Green) + " green)"
	}
	return pr.Number, head, ""
}

// fleetMergeStillOn re-reads a merge_when_green answer's check: the same head,
// still green. Unknown when either cannot be read.
func (s *Server) fleetMergeStillOn(ctx context.Context, a fleet.Action) nudgeState {
	head, ok := s.fleetPRHead(ctx, a.Evidence.PRRepo, a.Evidence.PRNumber)
	if !ok {
		return nudgeUnknown
	}
	if head != a.Evidence.PRHeadFull {
		return nudgeMovedOn
	}
	sum, ok := s.fleetCIRead(ctx, a.Evidence.PRRepo, head)
	switch {
	case !ok:
		return nudgeUnknown
	case !sum.AllGreen():
		return nudgeMovedOn
	}
	return nudgeStalled
}

// fleetPRHead is repo#n's full head commit, through the fleet's pull-request
// cache, for an owner the App admits.
func (s *Server) fleetPRHead(ctx context.Context, repo string, n int64) (string, bool) {
	reader := s.ext.FleetPRStates
	if reader == nil || repo == "" || n <= 0 || s.githubAppForOwner(ctx, requestOwner(ctx)) == nil {
		return "", false
	}
	st, known, _ := s.fleetPRs.read(ctx, reader, prroute.Ref{Repo: repo, PR: n}, s.now(), fleetPRReadTimeout, fleetPRTTL, fleetPRTTL)
	if !known || st.State != "open" || len(st.HeadSHA) < 12 {
		return "", false
	}
	return st.HeadSHA, true
}

// --- the CI read ----------------------------------------------------------------------

// CIReader is the GitHub App's CI read of one commit (githubapp.App.CommitChecks).
type CIReader interface {
	CommitChecks(ctx context.Context, repo, sha string) (cichecks.Checks, error)
}

// fleetCITTL is how long a commit's CI answer is reused (the pull-request
// read's two-minute bound).
const fleetCITTL = fleetPRTTL

// fleetCICache is the CI answers by "repo@sha".
type fleetCICache struct {
	mu sync.Mutex
	m  map[string]fleetCIEntry
}

type fleetCIEntry struct {
	sum   cichecks.Summary
	known bool
	until time.Time
}

// fleetCIRead is commit sha of repo's CI, judged (cichecks.Summarize): through
// the App, for an owner it admits (githubAppForOwner — the predicate the
// pull-request read applies), behind a two-minute cache (a failed read cached
// as unknown, so GitHub down is not asked every pass). ok false: unknown.
func (s *Server) fleetCIRead(ctx context.Context, repo, sha string) (cichecks.Summary, bool) {
	reader := s.ext.FleetChecks
	if reader == nil || repo == "" || sha == "" || s.githubAppForOwner(ctx, requestOwner(ctx)) == nil {
		return cichecks.Summary{}, false
	}
	key, now := repo+"@"+sha, s.now()
	s.fleetCI.mu.Lock()
	e, hit := s.fleetCI.m[key]
	s.fleetCI.mu.Unlock()
	if hit && now.Before(e.until) {
		return e.sum, e.known
	}
	rctx, cancel := context.WithTimeout(ctx, fleetPRReadTimeout)
	defer cancel()
	c, err := reader.CommitChecks(rctx, repo, sha)
	if err != nil {
		s.logger.Printf("fleet: CI of %s at %.12s: %v; unknown", repo, sha, err)
	}
	e = fleetCIEntry{sum: cichecks.Summarize(c), known: err == nil, until: now.Add(fleetCITTL)}
	s.fleetCI.mu.Lock()
	if s.fleetCI.m == nil || len(s.fleetCI.m) >= 1024 {
		s.fleetCI.m = map[string]fleetCIEntry{}
	}
	s.fleetCI.m[key] = e
	s.fleetCI.mu.Unlock()
	return e.sum, e.known
}

// fleetProposeCIResults proposes a ci_result (proposer sweep_code, no model)
// for each of octx's owner's STALLED mirrored sessions whose latest pull
// request is open and whose head's checks have ALL finished — at least one of
// them AFTER the session stopped (so CI was still running at the stop: the
// session stopped waiting on it) — and that has no background work, no open
// ask and no turn error (the stall facts). Once per stop.
func (s *Server) fleetProposeCIResults(octx context.Context, facts []nudgeCandidate) {
	if s.ext.FleetChecks == nil {
		return
	}
	busy := map[string]bool{}
	for _, c := range facts {
		if c.Shell || c.OpenAsk || c.TurnError {
			busy[c.Kind+":"+c.Key] = true
		}
	}
	fr, err := s.readFleet(octx, fleetListFilter{})
	if err != nil || !fr.stateOn {
		return
	}
	for _, sub := range fr.subjects {
		ep := sub.episode
		if sub.chief || sub.kind != chiefnudge.KindMirror || ep == nil || sub.state != fleet.StateStalled || busy[ep.Kind+":"+ep.Key] ||
			len(sub.prs) == 0 || sub.prs[0].State != fleet.PROpen {
			continue
		}
		pr := sub.prs[0]
		head, ok := s.fleetPRHead(octx, pr.Repo, pr.Number)
		if !ok {
			continue
		}
		sum, ok := s.fleetCIRead(octx, pr.Repo, head)
		if !ok || !sum.Done() || !sum.LastFinished.After(ep.OpenedAt) {
			continue
		}
		if done, err := s.ext.FleetActions.ProposedOn(octx, ep.ID, fleet.ClassCIResult); err != nil || done {
			continue
		}
		p := s.fleetProposalFor(sub, fleet.ClassCIResult, fleet.CIResultText(pr.Number, head[:12], sum.Green, sum.Failed, sum.FailedNames()),
			"The checks running on "+pr.Repo+"#"+strconv.FormatInt(pr.Number, 10)+" when it stopped have finished.")
		p.Proposer = fleet.ProposerSweep
		p.Evidence.PRHead, p.Evidence.PRHeadFull = head[:12], head
		if t := s.fleetTarget(octx, fleetActionOf(p)); t.refused != "" {
			continue
		}
		if _, err := s.ext.FleetActions.Propose(octx, p); err == nil {
			s.fleetChanged(requestOwner(octx))
		}
	}
}

// --- pick --------------------------------------------------------------------------

// fleetHeldPicker is the AskUserQuestion picker sub's window holds now: ONE
// open question set (internal/askq) of ONE single-select question, or why
// there is none muster can pick in.
func (s *Server) fleetHeldPicker(ctx context.Context, sub fleetSubject) (askq.Entry, string) {
	if sub.kind != chiefnudge.KindMirror || sub.view.ClaudeSession == "" || s.asks == nil {
		return askq.Entry{}, "the session shows no picker muster holds (only a mirrored Claude Code session's AskUserQuestion)"
	}
	open := s.asks.OpenIn(windowOwner(ctx), askSourceMirror, sub.view.Host, sub.view.ClaudeSession)
	switch {
	case len(open) == 0:
		return askq.Entry{}, "the session holds no open picker now"
	case len(open) > 1:
		return askq.Entry{}, "the session holds more than one open picker"
	case len(open[0].Questions) != 1 || open[0].Questions[0].MultiSelect:
		return askq.Entry{}, "the picker asks more than one question, or takes several options: answer it yourself in its card"
	}
	return open[0], ""
}

// fleetPickStillOn re-reads an answer_pick: the same open stop, and the same
// question set still open.
func (s *Server) fleetPickStillOn(ctx context.Context, a fleet.Action) nudgeState {
	if s.ext.Episodes == nil || s.asks == nil {
		return nudgeUnknown
	}
	ep, ok, err := s.ext.Episodes.OpenFor(ctx, a.SubjectKind, a.SubjectKey)
	switch {
	case err != nil:
		return nudgeUnknown
	case !ok || ep.ID != a.EpisodeID:
		return nudgeMovedOn
	}
	e, ok := s.asks.Get(a.Evidence.AskID)
	if !ok || !e.State.Open() || e.Key.ToolUseID != a.Evidence.AskTool || e.Key.Owner != requestOwner(ctx) {
		return nudgeMovedOn
	}
	return nudgeStalled
}

// fleetDeliverPick types an approved pick into its picker — the one typing an
// owner's tap on that picker makes (typeAskChoices) — and records how it went.
func (s *Server) fleetDeliverPick(ctx context.Context, a fleet.Action) fleetApproveResult {
	fail := func(why string) fleetApproveResult {
		s.fleetMove(ctx, a, fleet.Move{State: fleet.StateFailed, Outcome: why, Finish: true}, fleet.StateApproved)
		return fleetApproveResult{note: FleetNotDeliverable + why}
	}
	e, h, ok := s.fleetWindowOf(ctx, a)
	if !ok {
		return fail("the session is not in the window it was proposed in now")
	}
	if !h.Interactive {
		return fail("its host is read-only (not -interactive)")
	}
	entry, ok := s.asks.Get(a.Evidence.AskID)
	if !ok || !entry.State.Open() || entry.Key.ToolUseID != a.Evidence.AskTool {
		s.fleetMove(ctx, a, fleet.Move{State: fleet.Suppressed(fleet.GuardMovedOn), Outcome: "the picker was answered or closed before the tap", Finish: true}, fleet.StateApproved)
		return fleetApproveResult{note: FleetMovedOn, movedOn: true}
	}
	choices, answers, err := askq.FromForm(entry.Questions, url.Values{"a0": {a.Evidence.Option}})
	if err != nil {
		return fail("the option no longer fits the picker: " + err.Error())
	}
	res := s.typeAskChoices(ctx, h, e, entry.Key, entry, choices, answers)
	if res.kind != "typed" {
		why := firstNonBlank(res.msg, "it could not be typed into the picker ("+firstNonBlank(res.kind, "failed")+")")
		if res.err != nil {
			why = "it could not be typed into the picker: " + unprefixed(res.err)
		}
		return fail(why)
	}
	s.fleetMove(ctx, a, fleet.Move{State: fleet.StateSent, Outcome: "typed into the picker", Finish: true}, fleet.StateApproved)
	return fleetApproveResult{note: FleetSent, sent: true}
}

// fleetWindowOf is action a's window, still holding its conversation, and its
// host.
func (s *Server) fleetWindowOf(ctx context.Context, a fleet.Action) (mirror.SessionView, mirror.HostView, bool) {
	for _, x := range s.mirrorFocusEntries(ctx) {
		if x.SessionKey() == a.SubjectKey && x.Host == a.Host && x.Session == a.TmuxSession && x.Window == a.Window {
			h, ok := s.hostView(ctx, x.Host)
			return x, h, ok
		}
	}
	return mirror.SessionView{}, mirror.HostView{}, false
}

// --- route and end_session outside an owner turn ----------------------------------------

// fleetCardsFor reports whether chief's call outside an owner turn files a
// card: fleet proposals on, and chief a PERSON's (a team's chief has no fleet
// tools in this phase: the plan's "a team's chief gets none of these").
func (s *Server) fleetCardsFor(ctx context.Context, chief agents.Agent) bool {
	return s.fleetProposeOn() && !s.ownerIsTeam(ctx, agentOwnerID(chief))
}

// fleetRouteCard files a route card: the chief's text for session subject
// (route_to_session's), which its call outside an owner turn could not route.
// ok false when no card is filed this way (the call then refuses as before).
func (s *Server) fleetRouteCard(ctx context.Context, chief agents.Agent, subject focus.Subject, text string) (string, bool) {
	if !s.fleetCardsFor(ctx, chief) {
		return "", false
	}
	sub, refused, why := s.fleetStoppedSubject(ctx, subject.String())
	if refused != "" {
		return fleetToolResult(fleet.Action{}, refused, why+" (outside your owner's turn, a route is a card on a stopped session)"), true
	}
	return s.fleetFile(ctx, sub, s.fleetProposalFor(sub, fleet.ClassRoute, text, "a request outside your owner's turn")), true
}

// fleetRouteManyCards files a route card per subject of a route_many called
// outside an owner turn: {proposed: [{subject, action_id, card}], refused:
// [{subject, refused, error}]}.
func (s *Server) fleetRouteManyCards(ctx context.Context, chief agents.Agent, subjects []string, text string) (string, bool) {
	if !s.fleetCardsFor(ctx, chief) {
		return "", false
	}
	proposed, refusedList := []map[string]any{}, []map[string]any{}
	seen := map[string]bool{}
	for _, raw := range subjects {
		sub, err := focus.ParseSubject(raw)
		if err != nil || seen[sub.String()] {
			refusedList = append(refusedList, map[string]any{"subject": capRunes(raw, 120), "refused": fleetRefusedBadArgs})
			continue
		}
		seen[sub.String()] = true
		out, _ := s.fleetRouteCard(ctx, chief, sub, text)
		var m map[string]any
		_ = json.Unmarshal([]byte(out), &m)
		if m["refused"] != nil {
			refusedList = append(refusedList, map[string]any{"subject": sub.String(), "refused": m["refused"], "error": m["error"]})
			continue
		}
		proposed = append(proposed, map[string]any{"subject": sub.String(), "action_id": m["action_id"], "card": m["card"]})
	}
	return toolJSON(map[string]any{"proposed": proposed, "refused": refusedList,
		"next": "Outside your owner's turn nothing is routed: each session got a card in the fleet digest, sent only when your owner taps Approve. Tell them."}), true
}

// fleetCloseCard files a close card for the session end_session_propose named
// (a mirrored session's key, or agent:<name>) outside an owner turn. ok false
// when no card is filed this way.
func (s *Server) fleetCloseCard(ctx context.Context, chief agents.Agent, key string) (string, bool) {
	if !s.fleetCardsFor(ctx, chief) {
		return "", false
	}
	id := "mirror:" + key
	if name, isAgent := strings.CutPrefix(key, "agent:"); isAgent {
		ag, err := s.ext.Agents.GetByName(ctx, name)
		if err != nil || ag.OwnerID != agentOwnerID(chief) {
			return fleetToolResult(fleet.Action{}, fleetRefusedNotFound, fleetNotFoundWhy), true
		}
		id = "agent:" + strconv.FormatInt(ag.ID, 10)
	}
	sub, refused, why := s.fleetStoppedSubject(ctx, id)
	if refused != "" {
		return fleetToolResult(fleet.Action{}, refused, why), true
	}
	return s.fleetFile(ctx, sub, s.fleetProposalFor(sub, fleet.ClassClose, fleetCloseText(fleetRuntime(sub)), "your owner's end, outside their turn")), true
}
