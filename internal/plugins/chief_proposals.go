package plugins

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// CHIEF PROPOSALS (migration 0100; docs/design/chief-in-agent-view.md, phase
// 2): an action the chief PROPOSES about one of the owner's sessions — a
// succession now (a new session that continues it), ending it next — which
// only the owner's ONE TAP on its card starts. One table, one kind field, one
// decision column: every kind shares the card, the tap and the binding
// (internal/api chief_proposal.go).
//
// 🔴 ONE SEAM LEDGER, SIX WRITERS, EACH WITH ITS NAMED CALLERS (a test asserts
// the set over the Go source):
//
//   - CreateProposal: the proposal. Called only by the chief's propose tools
//     (through internal/api's fileProposal);
//   - a succession's LINK, only inside ReserveHostDispatch's reservation
//     transaction (HostDispatchNew.Succession): decided = accepted and
//     host_dispatch_id, on a row still UNDECIDED — or, for the base fallback,
//     still linked to the dispatch the caller names
//     (HostDispatchNew.SuccessionPrev). Single use: a second tap's reservation
//     finds no row to link and is rolled back (ErrProposalDecided), so it
//     inserts no dispatch either;
//   - DeclineProposal: decided = declined, on an undecided row. Called only by
//     the card's "Not now";
//   - MarkSuperseded: subject_state = superseded on an ACCEPTED replace
//     succession. Called only when the successor's dispatch turns running;
//   - SetSuccessionNotified: notified_at, once. Called only by the 30 s push;
//   - SetSuccessorSeen: successor_seen_at, once. Called only when the
//     successor's own view is opened.

// Proposal kinds.
const (
	ProposalSuccession = "succession"
	ProposalEndSession = "end_session"
)

// Succession modes (D1/D2).
const (
	SuccessionReplace = "replace"
	SuccessionSpinoff = "spinoff"
)

// Proposal decisions (decided).
const (
	ProposalOpen     = ""
	ProposalAccepted = "accepted"
	ProposalDeclined = "declined"
)

// Proposal is one row as read.
type Proposal struct {
	ID, Owner, ChiefAgent, OwnerMessage                 int64
	Kind                                                string
	SubjectKey, SubjectRuntime, SubjectHost             string
	SubjectTmuxSession, SubjectTmuxWindow, SubjectTitle string
	SubjectDispatch                                     int64
	AskID                                               int64
	Decided                                             string
	DecidedAt                                           time.Time
	// A succession's (kind ProposalSuccession).
	Mode                                   string // "" while the owner has not chosen
	Target, Model, Repo, Base              string
	Kickoff, KickoffSHA256, KickoffRef     string
	KickoffBlock                           int // -1 for the whole message
	HostDispatchID                         int64
	BaseUsed                               string
	SubjectState                           string
	SuccessorSeenAt, NotifiedAt, CreatedAt time.Time
}

// ProposalNew is a proposal's fields.
type ProposalNew struct {
	Kind                                                string
	ChiefAgent, OwnerMessage                            int64
	SubjectKey, SubjectRuntime, SubjectHost             string
	SubjectTmuxSession, SubjectTmuxWindow, SubjectTitle string
	SubjectDispatch                                     int64
	AskID                                               int64
	// A succession's.
	Mode                               string
	Target, Model, Repo, Base          string
	Kickoff, KickoffSHA256, KickoffRef string
	KickoffBlock                       int // -1 for the whole message
}

var (
	// ErrProposalNotFound: no row of the principal's has that id (or ask).
	ErrProposalNotFound = errors.New("plugins: no such proposal")
	// ErrProposalDecided: the row was already accepted or declined.
	ErrProposalDecided = errors.New("plugins: the proposal was already decided")
)

const proposalCols = `id, owner_id, kind, chief_agent, owner_message, subject_key, subject_runtime, subject_host, subject_tmux_session,
	subject_tmux_window, subject_title, coalesce(subject_dispatch, 0), coalesce(ask_id, 0), decided, coalesce(decided_at, 'epoch'),
	coalesce(mode, ''), target, model, repo, base, kickoff, kickoff_sha256, kickoff_ref, coalesce(kickoff_block, -1),
	coalesce(host_dispatch_id, 0), base_used, subject_state, coalesce(successor_seen_at, 'epoch'), coalesce(notified_at, 'epoch'),
	created_at`

func scanProposal(row pgx.Row) (Proposal, error) {
	var x Proposal
	err := row.Scan(&x.ID, &x.Owner, &x.Kind, &x.ChiefAgent, &x.OwnerMessage, &x.SubjectKey, &x.SubjectRuntime, &x.SubjectHost,
		&x.SubjectTmuxSession, &x.SubjectTmuxWindow, &x.SubjectTitle, &x.SubjectDispatch, &x.AskID, &x.Decided, &x.DecidedAt,
		&x.Mode, &x.Target, &x.Model, &x.Repo, &x.Base, &x.Kickoff, &x.KickoffSHA256, &x.KickoffRef, &x.KickoffBlock,
		&x.HostDispatchID, &x.BaseUsed, &x.SubjectState, &x.SuccessorSeenAt, &x.NotifiedAt, &x.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrProposalNotFound
	}
	for _, t := range []*time.Time{&x.DecidedAt, &x.SuccessorSeenAt, &x.NotifiedAt} {
		if t.Unix() == 0 {
			*t = time.Time{}
		}
	}
	return x, err
}

func nullable[T comparable](v, zero T) any {
	if v == zero {
		return nil
	}
	return v
}

// CreateProposal records the chief's proposal, as the principal.
func (s *PGStore) CreateProposal(ctx context.Context, n ProposalNew) (Proposal, error) {
	owner, err := hostDispatchOwner(ctx, "a proposal")
	if err != nil {
		return Proposal{}, err
	}
	var block any
	if n.KickoffBlock >= 0 && n.Kind == ProposalSuccession {
		block = n.KickoffBlock
	}
	return scanProposal(s.pool.QueryRow(ctx, `INSERT INTO chief_proposals (owner_id, kind, chief_agent, owner_message, subject_key,
		subject_runtime, subject_host, subject_tmux_session, subject_tmux_window, subject_title, subject_dispatch, ask_id, mode, target,
		model, repo, base, kickoff, kickoff_sha256, kickoff_ref, kickoff_block)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21) RETURNING `+proposalCols,
		owner, n.Kind, n.ChiefAgent, n.OwnerMessage, n.SubjectKey, n.SubjectRuntime, n.SubjectHost, n.SubjectTmuxSession, n.SubjectTmuxWindow,
		n.SubjectTitle, nullable(n.SubjectDispatch, 0), nullable(n.AskID, 0), nullable(n.Mode, ""), n.Target, n.Model, n.Repo, n.Base,
		n.Kickoff, n.KickoffSHA256, n.KickoffRef, block))
}

// ProposalByID is the principal's proposal id.
func (s *PGStore) ProposalByID(ctx context.Context, id int64) (Proposal, error) {
	owner, err := hostDispatchOwner(ctx, "a proposal")
	if err != nil {
		return Proposal{}, err
	}
	return scanProposal(s.pool.QueryRow(ctx, `SELECT `+proposalCols+` FROM chief_proposals WHERE id = $1 AND owner_id = $2`, id, owner))
}

// ProposalByAsk is the principal's proposal whose card is ask askID.
func (s *PGStore) ProposalByAsk(ctx context.Context, askID int64) (Proposal, error) {
	owner, err := hostDispatchOwner(ctx, "a proposal")
	if err != nil {
		return Proposal{}, err
	}
	return scanProposal(s.pool.QueryRow(ctx, `SELECT `+proposalCols+` FROM chief_proposals
		WHERE owner_id = $1 AND ask_id = $2 ORDER BY id DESC LIMIT 1`, owner, askID))
}

// LatestProposalFor is the principal's newest proposal of kind about the
// session keyed key (for a succession: "my successor").
func (s *PGStore) LatestProposalFor(ctx context.Context, kind, key string) (Proposal, error) {
	owner, err := hostDispatchOwner(ctx, "a proposal")
	if err != nil {
		return Proposal{}, err
	}
	return scanProposal(s.pool.QueryRow(ctx, `SELECT `+proposalCols+` FROM chief_proposals
		WHERE owner_id = $1 AND kind = $2 AND subject_key = $3 ORDER BY id DESC LIMIT 1`, owner, kind, key))
}

// SuccessionForDispatch is the principal's succession whose successor is host
// dispatch id ("my predecessor").
func (s *PGStore) SuccessionForDispatch(ctx context.Context, hostDispatchID int64) (Proposal, error) {
	owner, err := hostDispatchOwner(ctx, "a proposal")
	if err != nil {
		return Proposal{}, err
	}
	return scanProposal(s.pool.QueryRow(ctx, `SELECT `+proposalCols+` FROM chief_proposals
		WHERE owner_id = $1 AND kind = 'succession' AND host_dispatch_id = $2`, owner, hostDispatchID))
}

// SupersededKeys is every session key of the principal's that a running
// replace superseded (the sidebar's Superseded group).
func (s *PGStore) SupersededKeys(ctx context.Context) (map[string]bool, error) {
	owner, err := hostDispatchOwner(ctx, "proposals")
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT subject_key FROM chief_proposals
		WHERE owner_id = $1 AND kind = 'succession' AND subject_state = 'superseded'`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// linkSuccessionTx is a succession's link (see the ledger): inside the
// reservation's transaction, the row is accepted with the new dispatch — only
// while it is undecided (prev 0), or still linked to dispatch prev (the base
// fallback's second reservation). mode is the mode the owner's tap chose.
func linkSuccessionTx(ctx context.Context, tx pgx.Tx, owner, id, prev, dispatch int64, mode, base string) error {
	var tag interface{ RowsAffected() int64 }
	var err error
	if prev == 0 {
		tag, err = tx.Exec(ctx, `UPDATE chief_proposals SET decided = 'accepted', decided_at = now(), host_dispatch_id = $3,
			mode = $4, base_used = $5 WHERE id = $1 AND owner_id = $2 AND kind = 'succession' AND decided = ''`, id, owner, dispatch, mode, base)
	} else {
		tag, err = tx.Exec(ctx, `UPDATE chief_proposals SET host_dispatch_id = $3, base_used = $5
			WHERE id = $1 AND owner_id = $2 AND kind = 'succession' AND decided = 'accepted' AND host_dispatch_id = $6 AND mode = $4`,
			id, owner, dispatch, mode, base, prev)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrProposalDecided
	}
	return nil
}

// DeclineProposal records the owner's "Not now" on an undecided row.
func (s *PGStore) DeclineProposal(ctx context.Context, id int64) error {
	owner, err := hostDispatchOwner(ctx, "a proposal")
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE chief_proposals SET decided = 'declined', decided_at = now()
		WHERE id = $1 AND owner_id = $2 AND decided = ''`, id, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrProposalDecided
	}
	return nil
}

// MarkSuperseded marks an ACCEPTED replace succession's subject superseded,
// once; reports whether it changed.
func (s *PGStore) MarkSuperseded(ctx context.Context, id int64) (bool, error) {
	owner, err := hostDispatchOwner(ctx, "a proposal")
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE chief_proposals SET subject_state = 'superseded'
		WHERE id = $1 AND owner_id = $2 AND kind = 'succession' AND decided = 'accepted' AND mode = 'replace' AND subject_state = 'active'`,
		id, owner)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetSuccessionNotified records the 30 s push, once; reports whether this
// call set it.
func (s *PGStore) SetSuccessionNotified(ctx context.Context, id int64) (bool, error) {
	owner, err := hostDispatchOwner(ctx, "a proposal")
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE chief_proposals SET notified_at = now()
		WHERE id = $1 AND owner_id = $2 AND kind = 'succession' AND notified_at IS NULL AND successor_seen_at IS NULL`, id, owner)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetSuccessorSeen records that the successor's view was opened, once.
func (s *PGStore) SetSuccessorSeen(ctx context.Context, id int64) error {
	owner, err := hostDispatchOwner(ctx, "a proposal")
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE chief_proposals SET successor_seen_at = now()
		WHERE id = $1 AND owner_id = $2 AND kind = 'succession' AND successor_seen_at IS NULL`, id, owner)
	return err
}
