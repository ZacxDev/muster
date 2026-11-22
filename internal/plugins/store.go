package plugins

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// Store is the plugin tables (migration 0015). Every method acts as ctx's
// principal, and row-level security scopes it to that principal's rows — except
// Authenticate, which the caller runs under authz.System because it is how the
// owner is LEARNED.
type Store interface {
	// CreatePlugin mints a plugin for ctx's principal and returns the token
	// ONCE, with the capabilities (and, for CapViewer, the origin and entry
	// URL) chosen. The caller validates the origin and entry
	// URL against the deployment's MUSTER_PLUGIN_ORIGINS (ValidViewerLink).
	CreatePlugin(ctx context.Context, in NewPlugin) (string, Plugin, error)
	// MintHandoff mints a single-use hand-off code for one of the principal's
	// live viewer plugins (handoffs.go). ErrNotFound for any other plugin.
	MintHandoff(ctx context.Context, pluginID int64, in HandoffIn, now time.Time) (string, Plugin, error)
	// RedeemHandoff spends a code minted for pluginID (handoffs.go): unknown,
	// used, expired or another plugin's are one ErrNotFound.
	RedeemHandoff(ctx context.Context, pluginID int64, code string, now time.Time) (Handoff, error)
	// Server-installed apps (server.go; core PR 1h).

	// RedeemServerHandoff spends a code minted for server app app and records
	// its person's grant, answering the pairwise subject and display name.
	RedeemServerHandoff(ctx context.Context, app Plugin, code string, now time.Time) (ServerHandoff, error)
	// LiveServerGrants is THE gate: every one of subjects with its state
	// under app at now (live, expired or revoked; serverGrantLive), and for a
	// live one its person.
	LiveServerGrants(ctx context.Context, app Plugin, origins []OriginPattern, subjects []string, now time.Time) (map[string]ServerGrant, error)
	// InstallServerApp, RotateServerApp and UninstallServerApp are an admin's
	// (the caller gates who); each is audited as by.
	InstallServerApp(ctx context.Context, in ServerAppIn, by users.Actor) (string, Plugin, error)
	RotateServerApp(ctx context.Context, id int64, by users.Actor) (string, Plugin, error)
	UninstallServerApp(ctx context.Context, id int64, by users.Actor) (Plugin, error)
	// ServerApps is every live server app with its live-grant count (an
	// admin's list); VisibleServerApps the ones ctx's person gets a tab for;
	// AccountServerApps every one with that person's own state.
	ServerApps(ctx context.Context, origins []OriginPattern, now time.Time) ([]ServerAppInfo, error)
	VisibleServerApps(ctx context.Context) ([]Plugin, error)
	AccountServerApps(ctx context.Context, origins []OriginPattern, now time.Time) ([]AccountServerApp, error)
	// SetServerAppHidden hides or shows a server app for the principal, a person.
	SetServerAppHidden(ctx context.Context, id int64, hidden bool, now time.Time) (bool, error)
	// List is the principal's plugins, newest first.
	List(ctx context.Context) ([]Plugin, error)
	// Revoke revokes one of the principal's plugins, reporting whether it has
	// such a plugin at all. Revoking twice keeps the first time.
	Revoke(ctx context.Context, id int64) (bool, error)
	// SetDraftChecks turns CapDraftChecks on or off for a live plugin, keeping
	// its token and row; off lapses its drafts awaiting a check.
	SetDraftChecks(ctx context.Context, id int64, on bool) (p Plugin, changed bool, lapsed []Draft, err error)
	// Authenticate resolves a token to its plugin and stamps last_seen_at. A
	// revoked or unknown token, and one whose owner is disabled, are ErrBadToken.
	Authenticate(ctx context.Context, token string) (Plugin, error)
	// Submit stores one inbound message for pluginID. created is false when the
	// plugin already sent this external_id: the stored row is returned, unchanged,
	// whatever the new body says — and that answer wins over the rate caps, so a
	// plugin retrying a POST it did not see answered is never refused for it.
	Submit(ctx context.Context, pluginID int64, in MessageIn) (m Message, created bool, err error)
	// SubmitTaskEvent stores muster's own message handing task taskID to the
	// chief (task_events.go): no_reply, and the only writer of task_id.
	SubmitTaskEvent(ctx context.Context, pluginID, taskID int64, in MessageIn) (m Message, created bool, err error)
	// Message reads one message.
	Message(ctx context.Context, id int64) (Message, error)
	// Plugin reads one plugin.
	Plugin(ctx context.Context, id int64) (Plugin, error)
	// Queued is a plugin's undelivered messages, oldest first, at most limit.
	Queued(ctx context.Context, pluginID int64, limit int) ([]Message, error)
	// SetChiefSession records the chief chat session a plugin's messages are
	// delivered into.
	SetChiefSession(ctx context.Context, pluginID, agentID, sessionID int64) error
	// Retries is how many times the principal retried a message (retry.go).
	Retries(ctx context.Context, messageID int64) (int, error)
	// RecordAttempt counts an attempt that provably delivered nothing; the
	// message stays queued.
	RecordAttempt(ctx context.Context, messageID int64) error
	// BeginDelivery moves a queued message to `delivering` to agentID and counts
	// the attempt, BEFORE the turn runs, so the chief can cite it on that turn. It
	// reports false (and writes nothing) when the message was no longer queued,
	// or a mail resume holds it (resumeHolds).
	BeginDelivery(ctx context.Context, messageID, agentID int64) (bool, error)
	// AbandonDelivery returns a message `delivering` to agentID to `queued` and
	// clears its agent: the turn provably sent nothing. The attempt stays counted.
	AbandonDelivery(ctx context.Context, messageID, agentID int64) error
	// RecordDelivery settles a message `delivering` to agentID as delivered or
	// unconfirmed and appends the `plugin.delivered` audit row in the same
	// transaction. It reports false (and writes nothing) when the message was no
	// longer delivering to agentID.
	RecordDelivery(ctx context.Context, messageID, agentID int64, outcome Status) (bool, error)
	// RecoverDeliveries settles every message still `delivering` to agentID as
	// unconfirmed (audited, with "recovered": true) and returns how many. The
	// caller must hold agentID's session claim, which proves no live delivery to
	// that agent is in flight: such a row was left by a process that died
	// mid-turn, and it may have been pasted.
	RecoverDeliveries(ctx context.Context, agentID int64) (int, error)
	// DeliveringTo is the ids of the messages still `delivering` to agentID,
	// oldest first. It writes nothing. Deleting an agent with such a row strands
	// it: agent_id goes NULL (ON DELETE SET NULL) and RecoverDeliveries, keyed on
	// the agent, can then never settle it — so the chief replacement, holding the
	// chief's session claim, settles them (RecoverDeliveries) and reports these ids
	// before it deletes.
	DeliveringTo(ctx context.Context, agentID int64) ([]int64, error)
	// Retryable is the principal's unconfirmed messages that may be retried
	// (retry.go), newest first; Requeue puts one back in the queue, or refuses
	// with ErrNotRetryable saying why.
	Retryable(ctx context.Context, limit int) ([]Message, error)
	Requeue(ctx context.Context, id int64) (Message, error)
	// ReserveGatedTask spends one unit of an agent's gated-task budget (gated.go),
	// or returns ErrGatedTaskCap.
	ReserveGatedTask(ctx context.Context, agentID int64, messageID *int64) error

	// Reply drafts (drafts.go).

	// FileDraft stores an agent's reply draft to a message delivered to it,
	// superseding that message's pending draft (superseded counts them).
	FileDraft(ctx context.Context, agentID, ownerID int64, agentName string, messageID int64, body string) (d Draft, superseded int, err error)
	// Draft reads one draft.
	Draft(ctx context.Context, id int64) (Draft, error)
	// DraftCards is the principal's drafts for /plugins, those awaiting a
	// decision first.
	DraftCards(ctx context.Context, limit int) ([]DraftCard, error)
	// DraftCard is one of the principal's drafts as a card.
	DraftCard(ctx context.Context, id int64) (DraftCard, error)
	// DraftSources is who raised each of the drafts ids names that the
	// principal can read (row-level security), in one plain read: no sweep,
	// no write. A draft it cannot read is absent.
	DraftSources(ctx context.Context, ids []int64) ([]DraftSource, error)
	// SearchDraftCards is the Asks page's search over drafts: by id, any the
	// principal can open (row-level security); by text, its OWN; newest first.
	SearchDraftCards(ctx context.Context, draftID int64, text string, limit int) ([]DraftCard, error)
	// DecideDraftFrom approves or rejects a pending draft, recording where the
	// decision was made (the Asks page or a mirrored session over MCP).
	DecideDraftFrom(ctx context.Context, id int64, dec DraftDecision) (Draft, error)
	// ClaimDraft leases one approved, unclaimed draft of pluginID's.
	ClaimDraft(ctx context.Context, pluginID int64) (c Claim, ok bool, err error)
	// RenewDraft extends pluginID's live lease on a draft it claimed, within
	// MaxLeaseTotal of the claim.
	RenewDraft(ctx context.Context, pluginID, draftID int64) (Draft, error)
	// FinishDraft records pluginID's result for a draft it claimed; a sent one
	// supersedes the message's other pending or unclaimed approved drafts.
	FinishDraft(ctx context.Context, pluginID, draftID int64, r DraftResult) (d Draft, fresh bool, err error)
	// RetryDraft is the owner's manual retry of an outbound draft's chain
	// (draft_retry.go): a successor of its latest attempt, claimable now.
	RetryDraft(ctx context.Context, id int64, via DraftDecisionSource) (Draft, error)
	// DraftsAwaitingCheck is pluginID's drafts awaiting its pre-approval check
	// (CapDraftChecks), oldest first; CheckDraft records its verdict on one.
	DraftsAwaitingCheck(ctx context.Context, pluginID int64, limit int) ([]AwaitingCheck, error)
	CheckDraft(ctx context.Context, pluginID, draftID int64, v DraftCheck) (d Draft, fresh bool, err error)
	// DraftChecksDue is every owner's drafts whose check is past
	// DraftCheckWait (system scope); LapseDraftCheck lapses one, as its owner.
	DraftChecksDue(ctx context.Context) ([]DueDraftCheck, error)
	LapseDraftCheck(ctx context.Context, id int64) (d Draft, lapsed bool, err error)
	// QueuedForOwner is ownerID's undelivered messages across live plugins,
	// except those a mail resume holds (resumeHolds).
	QueuedForOwner(ctx context.Context, ownerID int64, limit int) ([]Message, error)
	// QueuedCounts is the principal's undelivered messages per plugin.
	QueuedCounts(ctx context.Context) (map[int64]int, error)

	// A message the chief did not disposition (unrouted.go, migration 0051).

	// SettleMessage records agentID's judgement that a message delivered to it
	// needs nothing more, with reason.
	SettleMessage(ctx context.Context, agentID, ownerID, messageID int64, reason string) (m Message, fresh bool, err error)
	// UnroutedDue is every owner's messages due an ask as of asOf (system scope).
	UnroutedDue(ctx context.Context, asOf time.Time, limit int) ([]Message, error)
	// UnroutedAsksToClose is every owner's open ask raised for a message that
	// has since been dispositioned (system scope).
	UnroutedAsksToClose(ctx context.Context, limit int) ([]UnroutedAsk, error)
	// SettleByAsk settles the message ask askID was raised for, with reason.
	SettleByAsk(ctx context.Context, askID int64, reason string) (bool, error)
	// A plugin bound to an officer (outbound.go, migration 0121).
	// SetDeliverTo binds one of the principal's live plugins to an officer
	// defined for its owner, or unbinds it ("").
	SetDeliverTo(ctx context.Context, id int64, deliverTo string) (p Plugin, changed bool, err error)
	// FileOutbound stores an officer's outbound draft for its bound plugin.
	FileOutbound(ctx context.Context, agentID, ownerID int64, agentName string, in NewOutbound) (Draft, error)
	// UploadDraftMedia records one ref's uploaded bytes for an outbound draft
	// awaiting its plugin's check.
	UploadDraftMedia(ctx context.Context, pluginID, draftID int64, m DraftMedia) (DraftMedia, bool, error)
}

// PGStore is the Postgres Store.
type PGStore struct {
	pool *pgxpool.Pool
	// denyWords are the deployment's own names a reply draft may not carry
	// (WithAgentDenyWords; drafts.go).
	denyWords []string
	// officers is the builtin officer store a bound plugin delivers to
	// (WithOfficerLink; outbound.go). nil: bindings and their messages are
	// refused.
	officers OfficerLink
	// retryEnded is told, after the sweep commits, of each retry chain that
	// ended unsent at its expiry (WithRetryEnded; drafts.go).
	retryEnded func(context.Context, Draft)
}

// NewPG returns the Postgres store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

const pluginCols = `id, owner_id, name, label, data_scope, created_at, last_seen_at, revoked_at, chief_agent_id, chief_session_id, capabilities, origin, entry_url, deliver_to, rotate_at, app_label, app_icon, scope`

// pluginDest is the scan destinations for pluginCols, in its order. THE ONE
// COPY, as messageDest: every reader of pluginCols scans through it.
func pluginDest(p *Plugin) []any {
	return []any{&p.ID, ownerCol{p}, &p.Name, &p.Label, &p.DataScope, &p.CreatedAt, &p.LastSeenAt, &p.RevokedAt,
		&p.ChiefAgentID, &p.ChiefSessionID, &p.Capabilities, &p.Origin, &p.EntryURL, &p.DeliverTo, &p.RotateAt, &p.AppLabel, &p.AppIcon, &p.Scope}
}

// ownerCol scans plugins.owner_id into p's unexported owner: an owner id, or
// NULL (a server app, migration 0172) as 0. THE ONE WRITER of the field from
// a row.
type ownerCol struct{ p *Plugin }

func (o ownerCol) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		o.p.ownerID = 0
	case int64:
		o.p.ownerID = v
	default:
		return fmt.Errorf("plugins: owner_id scanned as %T", src)
	}
	return nil
}

func scanPlugin(row pgx.Row) (Plugin, error) {
	var p Plugin
	err := row.Scan(pluginDest(&p)...)
	return p, err
}

const messageCols = `id, owner_id, plugin_id, external_id, sender, subject, body, received_at, created_at, status,
	attempts, last_attempt_at, delivered_at, agent_id, no_reply, sender_verified, task_id, routed_to, routed_done_at, settle_reason, thread, human_conversation, kind, venue, officer_item_id`

// messageDest is the scan destinations for messageCols, in its order, with the
// status into *st. THE ONE COPY: every reader of messageCols scans through it,
// so a column added to the list cannot be left out of a scan (DraftCards had
// its own copy, which a new column broke).
func messageDest(m *Message, st *string) []any {
	return []any{&m.ID, &m.OwnerID, &m.PluginID, &m.ExternalID, &m.Sender, &m.Subject, &m.Body, &m.ReceivedAt,
		&m.CreatedAt, st, &m.Attempts, &m.LastAttemptAt, &m.DeliveredAt, &m.AgentID, &m.NoReply, &m.SenderVerified, &m.TaskID, &m.RoutedTo, &m.RoutedDoneAt,
		&m.SettleReason, &m.Thread, &m.HumanConversation, &m.Kind, &m.Venue, &m.OfficerItemID}
}

func scanMessage(row pgx.Row) (Message, error) {
	var m Message
	var st string
	err := row.Scan(messageDest(&m, &st)...)
	m.Status = Status(st)
	return m, err
}

// Object is the audit_log `object` naming a plugin.
func Object(id int64) string { return "plugin:" + strconv.FormatInt(id, 10) }

// MessageObject is the audit_log `object` naming a plugin message.
func MessageObject(id int64) string { return "plugin_message:" + strconv.FormatInt(id, 10) }

// audit appends one audit_log row as ctx's principal (0009's self_append).
func audit(ctx context.Context, tx pgx.Tx, action, object string, detail map[string]any) error {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return errors.New("plugins: an audited write needs a user principal")
	}
	return users.AuditTx(ctx, tx, users.Actor{UserID: p.UserID, Via: p.Via}, action, object, detail)
}

func (s *PGStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Create is CreatePlugin for a messages-only token. Not on Store: no request
// path mints that way any more (the /plugins form goes through CreatePlugin);
// it stays on PGStore as the tests' shorthand.
func (s *PGStore) Create(ctx context.Context, name, label, dataScope string) (string, Plugin, error) {
	return s.CreatePlugin(ctx, NewPlugin{Name: name, Label: label, DataScope: dataScope, Capabilities: []Capability{CapMessages}})
}

// NewPlugin is what CreatePlugin mints.
type NewPlugin struct {
	Name, Label, DataScope string
	Capabilities           []Capability
	// Origin and EntryURL: set exactly when Capabilities has CapViewer or
	// CapApp.
	Origin, EntryURL string
	// AppLabel and AppIcon: set exactly when Capabilities has CapApp (the
	// tab's label, trimmed here, and its AppIcons key).
	AppLabel, AppIcon string
	// DeliverTo binds the new plugin to an officer ("officer:<id>", "" for
	// none): checked as SetDeliverTo checks it.
	DeliverTo string
}

// ViewerOnlyDataScope is the data scope a token without CapMessages stores
// (it brings no messages, so it names nowhere to record them).
const ViewerOnlyDataScope = "none"

func (s *PGStore) CreatePlugin(ctx context.Context, in NewPlugin) (string, Plugin, error) {
	// Fork 9 (a personal app plugin at a live server app's entry URL is
	// refused), read BEFORE the transaction begins. 🔴 It is an authz.System
	// read on ITS OWN pool connection (the person's connection cannot see a
	// server row), and taking a second connection while this transaction
	// holds the app cap's advisory lock starved the pool: with concurrent
	// mints holding every connection and waiting on the lock, its holder
	// waited for a connection until the statement timeout (CI, a 4-connection
	// pool; TestConcurrentAppMintsStopAtFive). Nothing is held while it runs.
	if err := s.serverAppMintRefusal(ctx, in.Capabilities, in.EntryURL); err != nil {
		return "", Plugin{}, err
	}
	// READ COMMITTED, explicitly: the app cap's count (appMintChecks) relies on
	// it, whatever the server's default_transaction_isolation says.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", Plugin{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tok, p, err := s.createPluginTx(ctx, tx, in)
	if err != nil {
		return "", Plugin{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", Plugin{}, err
	}
	return tok, p, nil
}

// createPluginTx is CreatePlugin in the caller's transaction tx, acting as
// ctx's principal (the owner): the one minting path, the /plugins form's and
// a pairing's hand-out (pairing.go).
func (s *PGStore) createPluginTx(ctx context.Context, tx pgx.Tx, in NewPlugin) (string, Plugin, error) {
	name, label, dataScope := in.Name, strings.TrimSpace(in.Label), in.DataScope
	if err := ValidName(name); err != nil {
		return "", Plugin{}, err
	}
	if err := ValidLabel(label); err != nil {
		return "", Plugin{}, err
	}
	caps, err := validCapabilities(in.Capabilities)
	if err != nil {
		return "", Plugin{}, err
	}
	app := slices.Contains(caps, CapApp)
	viewer := slices.Contains(caps, CapViewer) || app
	if viewer != (in.Origin != "" && in.EntryURL != "") || (in.Origin == "") != (in.EntryURL == "") {
		return "", Plugin{}, invalid("a viewer or app plugin needs an origin and an entry URL, and only such a plugin has them")
	}
	appLabel := strings.TrimSpace(in.AppLabel)
	if !app && (appLabel != "" || in.AppIcon != "") {
		return "", Plugin{}, invalid("only an app plugin has a tab label and icon")
	}
	var tabLabel, tabIcon *string
	if app {
		if err := ValidAppLabel(appLabel); err != nil {
			return "", Plugin{}, err
		}
		if err := ValidAppIcon(in.AppIcon); err != nil {
			return "", Plugin{}, err
		}
		owner, _ := authz.From(ctx)
		if err := s.appMintChecks(ctx, tx, owner.Team); err != nil {
			return "", Plugin{}, err
		}
		tabLabel, tabIcon = &appLabel, &in.AppIcon
	}
	if !slices.Contains(caps, CapMessages) {
		dataScope = ViewerOnlyDataScope
	}
	if err := ValidScope(dataScope); err != nil {
		return "", Plugin{}, err
	}
	var origin, entry *string
	if viewer {
		origin, entry = &in.Origin, &in.EntryURL
	}
	if err := ValidDeliverTo(in.DeliverTo); err != nil {
		return "", Plugin{}, err
	}
	if in.DeliverTo != "" {
		if !slices.Contains(caps, CapMessages) {
			return "", Plugin{}, invalid("only a plugin with messages can be bound to an officer")
		}
		owner, _ := authz.From(ctx)
		if err := s.officerDefined(ctx, owner.UserID, OfficerOf(in.DeliverTo)); err != nil {
			return "", Plugin{}, err
		}
	}
	tok, err := NewToken()
	if err != nil {
		return "", Plugin{}, err
	}
	// A savepoint, so a name already taken is answered as ErrInvalid without
	// aborting the caller's transaction.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", Plugin{}, err
	}
	p, err := scanPlugin(sp.QueryRow(ctx, `
		INSERT INTO plugins (name, label, token_hash, data_scope, capabilities, origin, entry_url, deliver_to, app_label, app_icon)
		VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), $9, $10) RETURNING `+pluginCols,
		name, label, HashToken(tok), dataScope, caps, origin, entry, in.DeliverTo, tabLabel, tabIcon))
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "plugins_owner_name_live" {
		_ = sp.Rollback(ctx)
		return "", Plugin{}, invalid("you already have a plugin named %q; revoke it first", name)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return "", Plugin{}, err
	}
	if err := sp.Commit(ctx); err != nil {
		return "", Plugin{}, err
	}
	detail := map[string]any{"name": name, "data_scope": dataScope, "capabilities": caps}
	if viewer {
		detail["origin"], detail["entry_url"] = in.Origin, in.EntryURL
	}
	if app {
		detail["app_label"], detail["app_icon"] = appLabel, in.AppIcon
	}
	if in.DeliverTo != "" {
		detail["deliver_to"] = in.DeliverTo
	}
	if err := audit(ctx, tx, "plugin.created", Object(p.ID), detail); err != nil {
		return "", Plugin{}, err
	}
	return tok, p, nil
}

// ownedByPrincipal is THE ONE OWNER FILTER of the plugin rows a principal
// lists or changes (team sharing 2b.3b, #696 round 0, D2): the principal's
// OWN rows — a person's, or a team's under the team acting principal — never
// a team's that a member's session can also read (0127 shares plugins at
// operator). Every path a person's page or a manager's team page reaches
// carries it (List, Revoke, SetDraftChecks, SetDeliverTo, MintHandoff), so a
// new listing or change of plugin rows is filtered by default;
// TestEveryPluginRowReadSaysWhoseRowsItReads classes every other read.
const ownedByPrincipal = `owner_id = ` + principalOwner

func (s *PGStore) List(ctx context.Context) ([]Plugin, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pluginCols+` FROM plugins WHERE `+ownedByPrincipal+` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plugin{}
	for rows.Next() {
		p, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PGStore) Plugin(ctx context.Context, id int64) (Plugin, error) {
	p, err := scanPlugin(s.pool.QueryRow(ctx, `SELECT `+pluginCols+` FROM plugins WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plugin{}, ErrNotFound
	}
	return p, err
}

func (s *PGStore) Revoke(ctx context.Context, id int64) (bool, error) {
	found := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var revoked *time.Time
		err := tx.QueryRow(ctx, `SELECT revoked_at FROM plugins WHERE id = $1 AND `+ownedByPrincipal+` FOR UPDATE`, id).Scan(&revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if revoked != nil {
			return nil // revoking twice keeps the first time, and audits once
		}
		if _, err := tx.Exec(ctx, `UPDATE plugins SET revoked_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		// A revoked plugin's chief section is gone with it (0141): never
		// rendered again, whatever was approved.
		if _, err := tx.Exec(ctx, `DELETE FROM plugin_chief_instructions WHERE plugin_id = $1`, id); err != nil {
			return err
		}
		return audit(ctx, tx, "plugin.revoked", Object(id), nil)
	})
	return found, err
}

// ChecksOffDetail is an outbound draft's detail when its plugin's
// draft_checks was turned off while it awaited the check (SetDraftChecks).
const ChecksOffDetail = "the plugin's Checks replies was turned off before it checked this draft"

// SetDraftChecks turns CapDraftChecks on or off for the principal's LIVE
// plugin id, changing that one capability and no other (and the same token:
// the plugin keeps its row — its rules, chief session, queued messages and
// drafts), through setCapabilityTx, the one writer of plugins.capabilities
// after the mint. Turning it on needs CapMessages (validCapabilities, the mint's rule). A
// plugin that already has the asked state changes nothing (changed false, no
// audit row). ErrNotFound for none, another owner's (row-level security) or
// a revoked one.
//
// 🔴 TURNING IT OFF STRANDS NO DRAFT: every one of the plugin's drafts still
// awaiting its check LAPSES in the same transaction (check_state 'lapsed', as
// LapseDraftCheck does at its deadline), so it is an ordinary pending draft the
// owner may approve; lapsed is them, for internal/api to ask the owner about.
// An OUTBOUND draft awaiting its check is REFUSED instead (outbound.go: it
// cannot be approved without the plugin's check), audited
// plugin.draft.check_refused.
// Audited: plugin.capability.changed {capability, old, new}, and a
// plugin.draft.check_lapsed per draft.
func (s *PGStore) SetDraftChecks(ctx context.Context, id int64, on bool) (p Plugin, changed bool, lapsed []Draft, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if p, changed, err = setCapabilityTx(ctx, tx, id, CapDraftChecks, on); err != nil || !changed || on {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE plugin_reply_drafts SET check_state = 'lapsed', checked_at = now()
			WHERE id IN (SELECT d.id FROM plugin_reply_drafts d JOIN plugin_messages m ON m.id = d.message_id
			              WHERE m.plugin_id = $1 AND d.status = 'pending' AND d.check_state = 'awaiting'
			                AND d.expires_at > now()
			              ORDER BY d.id FOR UPDATE OF d)
			RETURNING `+draftCols, id)
		if err != nil {
			return err
		}
		if lapsed, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Draft, error) { return scanDraft(r) }); err != nil {
			return err
		}
		for _, d := range lapsed {
			if err := audit(ctx, tx, "plugin.draft.check_lapsed", DraftObject(d.ID), map[string]any{
				"why": "the plugin's draft_checks was turned off"}); err != nil {
				return err
			}
		}
		// An OUTBOUND draft cannot lapse into an approvable one (its media
		// reach muster only through the check, outbound.go): it is REFUSED,
		// as a check refusal is, so it stops counting against its officer's
		// pending cap and its card says why.
		out, err := tx.Query(ctx, `UPDATE plugin_reply_drafts SET check_state = 'refused', checked_at = now(), status = 'rejected',
				decided_at = now(), detail = $2
			WHERE id IN (SELECT id FROM plugin_reply_drafts WHERE plugin_id = $1 AND message_id IS NULL
			              AND status = 'pending' AND check_state = 'awaiting' ORDER BY id FOR UPDATE)
			RETURNING `+draftCols, id, ChecksOffDetail)
		if err != nil {
			return err
		}
		refused, err := pgx.CollectRows(out, func(r pgx.CollectableRow) (Draft, error) { return scanDraft(r) })
		if err != nil {
			return err
		}
		for _, d := range refused {
			if err := audit(ctx, tx, "plugin.draft.check_refused", DraftObject(d.ID), map[string]any{
				"why": ChecksOffDetail}); err != nil {
				return err
			}
		}
		// Its officer's item is settled with it (outboundEnded).
		return s.outboundEnded(ctx, tx, refused, OutboundChecksOff)
	})
	return p, changed, lapsed, err
}

// setCapabilityTx turns capability c on or off for the principal's LIVE
// plugin id in tx, changing that one capability and no other: THE ONE WRITER
// of plugins.capabilities after the mint (SetDraftChecks, SetChiefExtension,
// SetTasks).
// The new set passes validCapabilities, the mint's rule. A plugin that already
// has the asked state changes nothing (changed false, no audit row).
// ErrNotFound for none, another owner's (row-level security and
// ownedByPrincipal) or a revoked one. Audited plugin.capability.changed
// {capability, old, new}.
func setCapabilityTx(ctx context.Context, tx pgx.Tx, id int64, c Capability, on bool) (p Plugin, changed bool, err error) {
	cur, err := scanPlugin(tx.QueryRow(ctx, `SELECT `+pluginCols+` FROM plugins WHERE id = $1 AND revoked_at IS NULL AND `+ownedByPrincipal+` FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plugin{}, false, ErrNotFound
	}
	if err != nil {
		return Plugin{}, false, err
	}
	if cur.Has(c) == on {
		return cur, false, nil
	}
	want := []Capability{}
	for _, have := range cur.Capabilities {
		if have != c {
			want = append(want, have)
		}
	}
	if on {
		want = append(want, c)
	}
	caps, err := validCapabilities(want)
	if err != nil {
		return Plugin{}, false, err
	}
	if p, err = scanPlugin(tx.QueryRow(ctx, `UPDATE plugins SET capabilities = $2 WHERE id = $1 RETURNING `+pluginCols, id, caps)); err != nil {
		return Plugin{}, false, err
	}
	if err := audit(ctx, tx, "plugin.capability.changed", Object(id), map[string]any{
		"capability": string(c), "old": cur.Capabilities, "new": caps}); err != nil {
		return Plugin{}, false, err
	}
	return p, true, nil
}

// SetChiefExtension turns CapChiefExtension on or off for the principal's
// LIVE plugin id, keeping its token and row (setCapabilityTx). 🔴 TURNING IT
// OFF DELETES THE PLUGIN'S STORED SECTION in the same transaction (approved or
// not): nothing stays approved behind a capability that is off, so turning it
// on again needs new text and a new approval. ErrNotFound as setCapabilityTx.
func (s *PGStore) SetChiefExtension(ctx context.Context, id int64, on bool) (p Plugin, changed bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if p, changed, err = setCapabilityTx(ctx, tx, id, CapChiefExtension, on); err != nil || !changed || on {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM plugin_chief_instructions WHERE plugin_id = $1`, id)
		return err
	})
	return p, changed, err
}

// SetTasks turns CapTasks on or off for the principal's LIVE plugin id,
// keeping its token and row (setCapabilityTx: the owner filter, the mint's
// rule, the plugin.capability.changed audit row). Nothing else moves with it:
// the tasks it filed stay. ErrNotFound as setCapabilityTx; ErrInvalid when off
// would leave no capability.
func (s *PGStore) SetTasks(ctx context.Context, id int64, on bool) (p Plugin, changed bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		p, changed, err = setCapabilityTx(ctx, tx, id, CapTasks, on)
		return err
	})
	return p, changed, err
}

func (s *PGStore) Authenticate(ctx context.Context, token string) (Plugin, error) {
	if !TokenShapeOK(token) {
		return Plugin{}, ErrBadToken
	}
	var p Plugin
	err := s.pool.QueryRow(ctx, `
		UPDATE plugins p SET last_seen_at = now()
		  FROM users u
		 WHERE p.token_hash = $1 AND p.revoked_at IS NULL
		   AND u.id = p.owner_id AND u.id IN (`+users.EnabledPersonOrTeamIDs+`)
		RETURNING `+prefixed("p", pluginCols)+`, coalesce(nullif(u.email, ''), u.display_name), u.kind = 'team'`, HashToken(token)).
		Scan(append(pluginDest(&p), &p.OwnerLogin, &p.OwnerTeam)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.authenticateServerApp(ctx, token)
	}
	return p, err
}

// authenticateServerApp is Authenticate's SECOND ARM (core PR 1h): a live
// SERVER app's token, with no owner to join. It comes back with Scope
// ScopeServer and no owner (PersonalOwner is false), and the door
// (internal/api serverPluginRoutes) decides what it is served. Run under
// the caller's authz.System, as Authenticate is.
func (s *PGStore) authenticateServerApp(ctx context.Context, token string) (Plugin, error) {
	p, err := scanPlugin(s.pool.QueryRow(ctx, `
		UPDATE plugins SET last_seen_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL AND scope = 'server' AND owner_id IS NULL
		RETURNING `+pluginCols, HashToken(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plugin{}, ErrBadToken
	}
	return p, err
}

func (s *PGStore) Submit(ctx context.Context, pluginID int64, in MessageIn) (Message, bool, error) {
	return s.submit(ctx, pluginID, in, nil, "")
}

// submit is Submit, and SubmitTaskEvent when taskID is set. One body, so a task
// event is held to every rule an inbound message is: the plugin row lock, the
// external_id idempotency, the rate caps and the field bounds.
// submit stores in for pluginID (once per external_id) under the caps of its
// bucket. taskID links a task (a task event, or a notice route); routedTo, set
// only with taskID, stores the row already delivered, routed around the chief.
func (s *PGStore) submit(ctx context.Context, pluginID int64, in MessageIn, taskID *int64, routedTo string) (Message, bool, error) {
	// An engagement's shape first: its defaults (external_id, body) come from
	// its person and event (engagement.go).
	if err := engagementShape(in); err != nil {
		return Message{}, false, err
	}
	in = in.withEngagementDefaults()
	if err := in.Validate(); err != nil {
		return Message{}, false, err
	}
	var m Message
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 🔴 THE PLUGIN ROW IS LOCKED FIRST, so the caps below are checked and the
		// insert made by one transaction at a time per plugin: two concurrent POSTs
		// cannot both see 59 and both land the 60th-and-61st.
		var live bool
		var p Plugin
		err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL, id, owner_id, name, deliver_to FROM plugins WHERE id = $1 FOR UPDATE`,
			pluginID).Scan(&live, &p.ID, ownerCol{&p}, &p.Name, &p.DeliverTo)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		m, err = scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM plugin_messages
			WHERE plugin_id = $1 AND external_id = $2`, pluginID, in.ExternalID))
		if err == nil {
			return nil // a repeat: the stored row is the answer
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// 🔴 A PLUGIN BOUND TO AN OFFICER (outbound.go): its message becomes an
		// item in that officer's queue, in THIS transaction — never a chief
		// turn. Task events are muster's own and never take this path.
		officer := ""
		if taskID == nil {
			if officer, err = boundOfficer(p, in); err != nil {
				return err
			}
		} else if in.Kind != "" || in.Venue != "" || in.URL != "" || in.Person != nil || in.Event != nil {
			return invalid("kind, venue and url are a plugin bound to an officer's")
		}
		// 🔴 TWO ALLOWANCES, COUNTED APART: chief mail (task_id NULL) against
		// MaxPerHour / MaxQueued, task events (task_id set) against
		// MaxTaskEventsPerHour / MaxTaskEventsQueued. Counted together, a burst
		// of task notices got the plugin's ordinary chief mail answered 429
		// (audit round 1).
		perHour, maxQueued := MaxPerHour, MaxQueued
		if taskID != nil {
			perHour, maxQueued = MaxTaskEventsPerHour, MaxTaskEventsQueued
		}
		var lastHour, queued int
		if err := tx.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE created_at > now() - make_interval(secs => $2)),
				count(*) FILTER (WHERE status = 'queued')
			  FROM plugin_messages WHERE plugin_id = $1 AND (task_id IS NOT NULL) = $3`,
			pluginID, rateWindow.Seconds(), taskID != nil).Scan(&lastHour, &queued); err != nil {
			return err
		}
		// A notice route (routedTo) spends the task-event HOUR only: it is
		// delivered from birth and never waits on the chief, so the chief's
		// undelivered queue must not refuse it (audit round 3).
		switch {
		case lastHour >= perHour:
			return ErrRateLimited
		case routedTo == "" && queued >= maxQueued:
			return ErrQueueFull
		}
		var routed any
		if routedTo != "" {
			routed = routedTo
		}
		if officer != "" {
			routed = RoutedToOfficer
		}
		m, err = scanMessage(tx.QueryRow(ctx, `
			INSERT INTO plugin_messages (plugin_id, external_id, sender, subject, body, received_at, no_reply, sender_verified, task_id,
			                             routed_to, status, delivered_at, thread, human_conversation, kind, venue, routed_done_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text,
			        CASE WHEN $10::text IS NULL THEN 'queued' ELSE 'delivered' END,
			        CASE WHEN $10::text IS NULL THEN NULL ELSE now() END, nullif($11, ''), $12, nullif($13, ''), nullif($14, ''),
			        CASE WHEN $10::text = 'officer' THEN now() END) RETURNING `+messageCols,
			pluginID, in.ExternalID, in.Sender, in.Subject, in.Body, in.ReceivedAt, in.NoReply, in.SenderVerified, taskID, routed, in.Thread,
			in.HumanConversation, officerKind(officer, in.Kind), in.Venue))
		if err != nil {
			return err
		}
		switch {
		case officer != "" && in.Kind == MessageKindEngagement:
			if m, err = s.routeEngagement(ctx, tx, p, officer, m, in); err != nil {
				return err
			}
		case officer != "":
			if m, err = s.routeToOfficer(ctx, tx, p, officer, m, in.URL); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	if err != nil {
		return Message{}, false, err
	}
	return m, created, nil
}

func (s *PGStore) Message(ctx context.Context, id int64) (Message, error) {
	m, err := scanMessage(s.pool.QueryRow(ctx, `SELECT `+messageCols+` FROM plugin_messages WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

func (s *PGStore) Queued(ctx context.Context, pluginID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		return []Message{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+messageCols+` FROM plugin_messages
		WHERE plugin_id = $1 AND status = 'queued' ORDER BY id LIMIT $2`, pluginID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PGStore) SetChiefSession(ctx context.Context, pluginID, agentID, sessionID int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE plugins SET chief_agent_id = $2, chief_session_id = $3 WHERE id = $1`,
		pluginID, agentID, sessionID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *PGStore) RecordAttempt(ctx context.Context, messageID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE plugin_messages SET attempts = attempts + 1, last_attempt_at = now()
		WHERE id = $1 AND status = 'queued'`, messageID)
	return err
}

func (s *PGStore) BeginDelivery(ctx context.Context, messageID, agentID int64) (bool, error) {
	// Not a message a resume holds (resumeHolds): the resume routes it, or
	// gives it back to the chief once it has finished.
	tag, err := s.pool.Exec(ctx, `UPDATE plugin_messages m SET status = 'delivering', agent_id = $2,
		       attempts = attempts + 1, last_attempt_at = now()
		 WHERE m.id = $1 AND m.status = 'queued' AND NOT `+resumeHolds, messageID, agentID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PGStore) AbandonDelivery(ctx context.Context, messageID, agentID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE plugin_messages SET status = 'queued', agent_id = NULL
		WHERE id = $1 AND status = 'delivering' AND agent_id = $2`, messageID, agentID)
	return err
}

func (s *PGStore) RecordDelivery(ctx context.Context, messageID, agentID int64, outcome Status) (bool, error) {
	if outcome != StatusDelivered && outcome != StatusUnconfirmed {
		return false, invalid("a delivery settles as delivered or unconfirmed, not %q", outcome)
	}
	settled := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var pluginID int64
		err := tx.QueryRow(ctx, `
			UPDATE plugin_messages SET status = $3, delivered_at = now()
			 WHERE id = $1 AND status = 'delivering' AND agent_id = $2
			RETURNING plugin_id`, messageID, agentID, string(outcome)).Scan(&pluginID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		settled = true
		return audit(ctx, tx, "plugin.delivered", MessageObject(messageID), map[string]any{
			"plugin_id": pluginID, "agent_id": agentID, "outcome": string(outcome),
		})
	})
	return settled, err
}

func (s *PGStore) DeliveringTo(ctx context.Context, agentID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM plugin_messages
		WHERE agent_id = $1 AND status = 'delivering' ORDER BY id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *PGStore) RecoverDeliveries(ctx context.Context, agentID int64) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE plugin_messages SET status = 'unconfirmed', delivered_at = now()
			 WHERE agent_id = $1 AND status = 'delivering'
			RETURNING id, plugin_id`, agentID)
		if err != nil {
			return err
		}
		type rec struct{ id, plugin int64 }
		var got []rec
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.id, &r.plugin); err != nil {
				rows.Close()
				return err
			}
			got = append(got, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range got {
			if err := audit(ctx, tx, "plugin.delivered", MessageObject(r.id), map[string]any{
				"plugin_id": r.plugin, "agent_id": agentID, "outcome": string(StatusUnconfirmed), "recovered": true,
			}); err != nil {
				return err
			}
		}
		n = len(got)
		return nil
	})
	return n, err
}

// prefixed qualifies a column list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
