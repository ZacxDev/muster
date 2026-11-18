package plugins

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/github"
)

// AUTO-START (migration 0152, docs/plugins.md#auto-start): the owner opts ONE
// plugin in, on its /plugins card behind a fresh sign-in, and a task that
// plugin then files starts an agent on it with no tap — at a target and in a
// repository the owner chose at opt-in, at most DailyCap times a day, inside
// the chief's dispatch budget (internal/api plugin_autostart.go).
//
// 🔴 A PLUGIN CANNOT OPT ITSELF IN. Its token acts as its owner
// (authz.ViaPlugin), so row-level security cannot tell the two apart; this
// store can, and every writer here refuses any principal but the owner's
// signed-in browser (autostartWriter). No /plugin/v1/* route writes it either.

// The audit actions of auto-start.
const (
	ActionAutostartOn  = "plugin.autostart.on"
	ActionAutostartOff = "plugin.autostart.off"
	// ActionAutostartStarted records one auto-start's agent and the ask that
	// lets its owner stop it (object NoteObject(task)).
	ActionAutostartStarted = "plugin.autostart.started"
	// ActionAutostartSkipped records why a task of an opted-in plugin was NOT
	// auto-started (object NoteObject(task)): the budget, the cap, the task.
	ActionAutostartSkipped = "plugin.autostart.skipped"
	// DetailAutostartPlugin is the reservation detail key (ActionChiefDispatch)
	// naming the plugin an auto-start was for: what AutostartsInWindow counts.
	DetailAutostartPlugin = "autostart_plugin"
)

// MaxAutostartDailyCap is the highest per-plugin cap an owner may set (the
// 0152 CHECK): the chief's own daily budget, which caps it anyway.
const MaxAutostartDailyCap = MaxChiefDispatchesPerDay

// AutostartWindow is the per-plugin cap's window.
const AutostartWindow = 24 * time.Hour

// AutostartStopWindow bounds how old an auto-start's ask may be for its Stop
// answer to be read back (AutostartByAsk): an ask lives at most
// attention.MaxTimeout, well inside it.
const AutostartStopWindow = 30 * 24 * time.Hour

// ErrAutostartDoor: a write to a plugin's auto-start from anything but its
// owner's signed-in browser (a plugin token, an agent, a personal API token).
var ErrAutostartDoor = errors.New("plugins: only the owner, signed in, can change a plugin's auto-start")

// Autostart is one plugin's auto-start setting: off (the zero value, On
// false), or on with its fixed target, repository and per-day cap.
type Autostart struct {
	On       bool
	Target   string
	Repo     string
	DailyCap int
	Since    time.Time
}

// autostartWriter is ctx's owner for an auto-start write, refusing every door
// but authz.ViaSession (the owner's signed-in browser, where /plugins lives).
func autostartWriter(ctx context.Context) (int64, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return 0, errors.New("plugins: an auto-start write needs a user principal")
	}
	if p.Via != authz.ViaSession {
		return 0, ErrAutostartDoor
	}
	return p.UserID, nil
}

// SetAutostart turns pluginID's auto-start ON with target, repo and cap, as
// ctx's principal — which must be its owner's signed-in browser
// (ErrAutostartDoor otherwise). ErrNotFound for a plugin that is not live, not
// the principal's, or a TEAM's (auto-start is a person's). ErrInvalid for a
// target, repository or cap the 0152 CHECKs would refuse. Setting it again
// replaces the target, repository and cap and restarts Since.
//
// Audited plugin.autostart.on {target, repo, daily_cap}.
func (s *PGStore) SetAutostart(ctx context.Context, pluginID int64, target, repo string, dailyCap int) (Autostart, error) {
	owner, err := autostartWriter(ctx)
	if err != nil {
		return Autostart{}, err
	}
	switch {
	case target != "pod/claude-code" && target != "pod/gateway":
		return Autostart{}, invalid("target must be pod/claude-code or pod/gateway")
	case !github.ValidRepoName(repo):
		return Autostart{}, invalid("repository must be owner/name")
	case dailyCap < 1 || dailyCap > MaxAutostartDailyCap:
		return Autostart{}, invalid("the cap must be 1 to %d a day", MaxAutostartDailyCap)
	}
	var a Autostart
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE plugins p SET autostart_target = $2, autostart_repo = $3, autostart_daily_cap = $4,
				autostart_at = now()
			  FROM users u
			 WHERE p.id = $1 AND p.owner_id = $5 AND p.revoked_at IS NULL AND u.id = p.owner_id AND u.kind <> 'team'
			RETURNING p.autostart_target, p.autostart_repo, p.autostart_daily_cap, p.autostart_at`,
			pluginID, target, repo, dailyCap, owner).Scan(&a.Target, &a.Repo, &a.DailyCap, &a.Since)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		a.On = true
		return audit(ctx, tx, ActionAutostartOn, Object(pluginID), map[string]any{"target": target, "repo": repo, "daily_cap": dailyCap})
	})
	return a, err
}

// ClearAutostart turns pluginID's auto-start OFF, as ctx's principal (its
// owner's signed-in browser: ErrAutostartDoor otherwise). It reports whether
// it was on. ErrNotFound for a plugin that is not the principal's.
//
// Audited plugin.autostart.off when it was on.
func (s *PGStore) ClearAutostart(ctx context.Context, pluginID int64) (bool, error) {
	owner, err := autostartWriter(ctx)
	if err != nil {
		return false, err
	}
	changed := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var was *string
		err := tx.QueryRow(ctx, `SELECT autostart_target FROM plugins WHERE id = $1 AND owner_id = $2 FOR UPDATE`, pluginID, owner).Scan(&was)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || was == nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE plugins SET autostart_target = NULL, autostart_repo = NULL, autostart_daily_cap = NULL,
				autostart_at = NULL WHERE id = $1 AND owner_id = $2`, pluginID, owner); err != nil {
			return err
		}
		changed = true
		return audit(ctx, tx, ActionAutostartOff, Object(pluginID), nil)
	})
	return changed, err
}

// AutostartOf is pluginID's auto-start as ctx's principal reads it (the
// owner's card, and the plugin's own task filing, which acts as the owner).
// The zero Autostart (off) for a plugin that is revoked, not the principal's,
// a team's, or simply not opted in.
func (s *PGStore) AutostartOf(ctx context.Context, pluginID int64) (Autostart, error) {
	var (
		a      Autostart
		target *string
		repo   *string
		cap    *int
		since  *time.Time
	)
	err := s.pool.QueryRow(ctx, `SELECT p.autostart_target, p.autostart_repo, p.autostart_daily_cap, p.autostart_at
		  FROM plugins p JOIN users u ON u.id = p.owner_id
		 WHERE p.id = $1 AND p.revoked_at IS NULL AND u.kind <> 'team' AND p.`+ownedByPrincipal, pluginID).
		Scan(&target, &repo, &cap, &since)
	if errors.Is(err, pgx.ErrNoRows) {
		return Autostart{}, nil
	}
	if err != nil {
		return Autostart{}, err
	}
	if target == nil || repo == nil || cap == nil || since == nil {
		return Autostart{}, nil
	}
	a = Autostart{On: true, Target: *target, Repo: *repo, DailyCap: *cap, Since: *since}
	return a, nil
}

// AutostartsInWindow is how many chief-ledger reservations the principal made
// for pluginID's auto-starts in the last AutostartWindow — the reservation rows
// (ActionChiefDispatch) whose detail names the plugin
// (DetailAutostartPlugin), so a start that then failed still counts
// (fail-closed, as the chief's own daily cap). internal/api reads it inside
// the reservation's check, under the owner's reservation lock, so two
// concurrent tasks cannot both take the last unit.
func (s *PGStore) AutostartsInWindow(ctx context.Context, pluginID int64) (int, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return 0, errors.New("plugins: counting auto-starts needs a user principal")
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log
		WHERE action = $1 AND user_id = $2 AND at > now() - make_interval(secs => $3) AND detail->>$4 = $5`,
		ActionChiefDispatch, p.UserID, AutostartWindow.Seconds(), DetailAutostartPlugin, strconv.FormatInt(pluginID, 10)).Scan(&n)
	return n, err
}

// AutostartRun is one auto-start as recorded (ActionAutostartStarted).
type AutostartRun struct {
	PluginID int64
	TaskID   int64
	AgentID  int64
	AskID    int64
}

// RecordAutostartStarted records an auto-start's agent and the ask filed for it.
func (s *PGStore) RecordAutostartStarted(ctx context.Context, r AutostartRun) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionAutostartStarted, NoteObject(r.TaskID), map[string]any{
			"plugin_id": r.PluginID, "agent_id": r.AgentID, "ask_id": r.AskID})
	})
}

// RecordAutostartSkipped records why a task of pluginID was not auto-started.
func (s *PGStore) RecordAutostartSkipped(ctx context.Context, pluginID, taskID int64, reason string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionAutostartSkipped, NoteObject(taskID), map[string]any{"plugin_id": pluginID, "reason": reason})
	})
}

// AutostartByAsk is the auto-start the principal recorded with ask askID (its
// Stop ask), from the last AutostartStopWindow; ok false when there is none.
func (s *PGStore) AutostartByAsk(ctx context.Context, askID int64) (AutostartRun, bool, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return AutostartRun{}, false, errors.New("plugins: reading an auto-start needs a user principal")
	}
	var r AutostartRun
	var object string
	err := s.pool.QueryRow(ctx, `SELECT object, (detail->>'plugin_id')::bigint, (detail->>'agent_id')::bigint, (detail->>'ask_id')::bigint
		  FROM audit_log WHERE action = $1 AND user_id = $2 AND at > now() - make_interval(secs => $3) AND detail->>'ask_id' = $4
		 ORDER BY id DESC LIMIT 1`,
		ActionAutostartStarted, p.UserID, AutostartStopWindow.Seconds(), strconv.FormatInt(askID, 10)).
		Scan(&object, &r.PluginID, &r.AgentID, &r.AskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AutostartRun{}, false, nil
	}
	if err != nil {
		return AutostartRun{}, false, err
	}
	r.TaskID = noteObjectID(object)
	return r, r.TaskID > 0 && r.AgentID > 0, nil
}
