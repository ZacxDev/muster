package plugins

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/flows"
)

// A REPLY MUSTER WRITES ITSELF: a request muster answers in code, with no
// chief turn and no owner decision — today only a fleet-report request
// (internal/api/fleet_report.go, docs/plugins.md#a-fleet-report-request).
//
// 🔴 WHAT STANDS IN FOR THE OWNER'S APPROVAL. Every other reply is an agent's
// text, so the control is the owner reading it (ScreenDraft says so) — or, for
// a relay, a verified sender the owner allowlisted. Here the text is not an
// agent's: muster builds it from its own rows, deterministically, for a sender
// the DEPLOYMENT listed (an AgentMailSenders list) and the plugin verified. So
// the draft is filed already approved, as an allowlisted relay's is. What the
// caller may put in is the caller's rule (fleet_report.go: only sessions under
// the configured working-directory prefixes, secret-shaped runs removed); this
// store refuses a body with a character nobody could see, or one that is not
// already in normal form.

// MusterReplier is the store side of a reply muster writes itself.
type MusterReplier interface {
	// ReplyAsMuster takes QUEUED message messageID of owner ownerID, hands it
	// to chief agent chiefID WITHOUT a turn (delivered, settled with reason)
	// and files body as its reply draft, ALREADY APPROVED, under agentName —
	// all in one transaction, audited. A message that is no longer queued (any
	// other delivery took it) is ErrNotFound and nothing is written; a
	// no_reply message is ErrNoReply.
	ReplyAsMuster(ctx context.Context, chiefID, ownerID, messageID int64, agentName, body, reason, via string) (Draft, error)
}

var _ MusterReplier = (*PGStore)(nil)

// ReplyAsMuster: see MusterReplier.
func (s *PGStore) ReplyAsMuster(ctx context.Context, chiefID, ownerID, messageID int64, agentName, body, reason, via string) (Draft, error) {
	if chiefID <= 0 || ownerID <= 0 {
		return Draft{}, invalid("the agent has no owner")
	}
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return Draft{}, invalid("body must be UTF-8 text without NUL")
	}
	// 🔴 NEVER NORMALISED HERE, ONLY CHECKED. The body is structured text (JSON)
	// muster built; NFKC can turn a character inside a string into a quote
	// (U+FF02 → '"') and so change its structure. The caller normalises each
	// field before it encodes them; a body that is still not in normal form is
	// refused rather than rewritten.
	if flows.NormalizeAgentText(body) != body {
		return Draft{}, invalid("body is not in normal form (NFKC)")
	}
	switch {
	case strings.TrimSpace(body) == "":
		return Draft{}, invalid("body is required")
	case len(body) > MaxDraftBytes:
		return Draft{}, invalid("body is over %d bytes", MaxDraftBytes)
	}
	if why := flows.HiddenCharRefusal(body); why != "" {
		return Draft{}, invalid("body: %s", why)
	}
	reason, err := cleanSettleReason(reason)
	if err != nil {
		return Draft{}, err
	}
	if err := s.sweep(ctx); err != nil {
		return Draft{}, err
	}
	var d Draft
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		m, inFlight := lockMessage(ctx, tx, messageID, 0)
		if inFlight != nil && !errors.Is(inFlight, ErrReplyInFlight) {
			return inFlight
		}
		// 🔴 ONLY A QUEUED MESSAGE, AS A ROUTE MUSTER MAKES TAKES ONE
		// (CreateRouteFrom): one another delivery already holds is not taken
		// from under it.
		if m.Status != StatusQueued || m.OwnerID != ownerID {
			return ErrNotFound
		}
		if m.NoReply {
			return ErrNoReply
		}
		if inFlight != nil {
			return inFlight
		}
		if _, err := tx.Exec(ctx, `UPDATE plugin_messages SET status = 'delivered', agent_id = $2, delivered_at = now(),
			attempts = attempts + 1, last_attempt_at = now(), settle_reason = $3 WHERE id = $1`, messageID, chiefID, reason); err != nil {
			return err
		}
		if err := audit(ctx, tx, "plugin.delivered", MessageObject(messageID), map[string]any{
			"plugin_id": m.PluginID, "agent_id": chiefID, "outcome": string(StatusDelivered), "answered_by": "muster", "via": via}); err != nil {
			return err
		}
		if err := audit(ctx, tx, "plugin.message.settled", MessageObject(messageID), map[string]any{
			"agent_id": chiefID, "by": "muster", "via": via}); err != nil {
			return err
		}
		var err error
		if d, err = scanDraft(tx.QueryRow(ctx, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body,
				expires_at, status, decided_at, approved_at)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6), 'approved', now(), now()) RETURNING `+draftCols,
			ownerID, messageID, chiefID, cutRunes(agentName, 100), body, DraftTTL.Seconds())); err != nil {
			return err
		}
		if err := audit(ctx, tx, "plugin.draft.filed", DraftObject(d.ID), map[string]any{
			"message_id": messageID, "agent_id": chiefID, "by": "muster", "via": via, "superseded": 0}); err != nil {
			return err
		}
		return audit(ctx, tx, "plugin.draft.approved", DraftObject(d.ID), map[string]any{
			"message_id": messageID, "auto": true, "via": via})
	})
	if err != nil {
		return Draft{}, err
	}
	return d, nil
}
