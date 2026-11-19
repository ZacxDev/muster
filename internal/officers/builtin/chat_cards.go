package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// CONFIRM CARDS (officer_chat_cards, migration 0160; internal/api
// officer_own_state.go and officer_operator_chat.go; docs/officers-builtin.md
// "Chat with it")
//
// An officer chatting with a person on its page asks for one with
// officer_confirm_card; muster draws it in that person's chat showing the
// exact text and media; only that person's own signed-in click decides it.
//
// 🔴 A CARD IS BOUND TO WHAT IT SHOWED WHEN IT WAS DRAWN: a draft card keeps
// the draft's body and media SHA-256s, a batch card the batch's SHA-256 and
// its entries, and the decision is made on THOSE (the plugins store refuses
// anything else under its row lock). CardSHA256 is the SHA-256 of all of
// them, which the card's form posts back.
//
// Every read and write runs under the caller's principal: a card's row is the
// person whose chat it is (owner_id), personal under row-level security, so
// nobody else reads, decides or even sees it.
// ---------------------------------------------------------------------------

// The kinds of confirm card.
const (
	CardDraft = "draft"
	CardBatch = "batch"
	// CardRefile and CardDrop are a refusal's Re-file (at RefileAt) and
	// Drop, as officer_refusal_refile / officer_refusal_drop PROPOSE them.
	CardRefile = "refile"
	CardDrop   = "drop"
)

// The decisions a card records.
const (
	CardApproved = "approved"
	CardRejected = "rejected"
)

// ChatCard is one confirm card.
type ChatCard struct {
	ID int64
	// Owner is the person whose chat it is.
	Owner int64
	// OfficerOwner and Officer name the officer that asked for it.
	OfficerOwner int64
	Officer      string
	Kind         string
	// A draft card's draft and what it showed.
	DraftID     int64
	BodySHA256  string
	MediaSHA256 []string
	// A batch card's batch and what it showed.
	BatchID      string
	BatchSHA256  string
	BatchEntries []int64
	// A refusal card's refusal (its draft.refused item) and, for a
	// re-file, when.
	RefusalItem int64
	RefileAt    *time.Time
	CardSHA256  string
	CreatedAt    time.Time
	DecidedAt    *time.Time
	Decision     string
}

// CardSHA256 is THE hash a card is bound to: every field it shows a decision
// on, in one fixed order, each on its own line — so no two cards that show
// different things share it.
func CardSHA256(c ChatCard) string {
	var b strings.Builder
	b.WriteString("muster officer confirm card v1\n")
	b.WriteString(strconv.FormatInt(c.OfficerOwner, 10) + "\n" + c.Officer + "\n" + c.Kind + "\n")
	switch c.Kind {
	case CardDraft:
		b.WriteString(strconv.FormatInt(c.DraftID, 10) + "\n" + c.BodySHA256 + "\n" + strings.Join(c.MediaSHA256, ",") + "\n")
	case CardBatch:
		ids := make([]string, len(c.BatchEntries))
		for i, id := range c.BatchEntries {
			ids[i] = strconv.FormatInt(id, 10)
		}
		b.WriteString(c.BatchID + "\n" + c.BatchSHA256 + "\n" + strings.Join(ids, ",") + "\n")
	case CardRefile, CardDrop:
		at := ""
		if c.RefileAt != nil {
			at = c.RefileAt.UTC().Format(time.RFC3339)
		}
		b.WriteString(strconv.FormatInt(c.RefusalItem, 10) + "\n" + at + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// MaxOpenCards bounds the undecided cards one person's chat with one officer
// holds: an officer asking again and again cannot flood the chat.
const MaxOpenCards = 20

// ErrTooManyCards is a card refused because the chat already holds
// MaxOpenCards undecided ones.
var ErrTooManyCards = errors.New("officers: this chat already holds the most undecided confirm cards muster keeps")

// ErrCardDecided is a decision on a card already decided.
var ErrCardDecided = errors.New("officers: this confirm card was already decided")

const chatCardCols = `id, owner_id, officer_owner, officer, kind, coalesce(draft_id, 0), coalesce(body_sha256, ''), coalesce(media_sha256, '{}'),
	coalesce(batch_id, ''), coalesce(batch_sha256, ''), coalesce(batch_entries, '{}'), coalesce(refusal_item, 0), refile_at, card_sha256, created_at, decided_at, coalesce(decision, '')`

func scanChatCard(row pgx.Row) (ChatCard, error) {
	var c ChatCard
	err := row.Scan(&c.ID, &c.Owner, &c.OfficerOwner, &c.Officer, &c.Kind, &c.DraftID, &c.BodySHA256, &c.MediaSHA256,
		&c.BatchID, &c.BatchSHA256, &c.BatchEntries, &c.RefusalItem, &c.RefileAt, &c.CardSHA256, &c.CreatedAt, &c.DecidedAt, &c.Decision)
	return c, err
}

// AddChatCard records card c in the chat of ctx's principal (owner_id is the
// principal, by the column's default) and returns it with its id, time and
// CardSHA256. An UNDECIDED card for the same draft (or the same batch at the
// same batch SHA-256) is returned instead of a second one.
func (s *Store) AddChatCard(ctx context.Context, c ChatCard, at time.Time) (ChatCard, error) {
	var draft, batchEntries any
	var body, batch, batchSHA any
	var media, refusal, refileAt any
	if c.RefileAt != nil {
		t := c.RefileAt.UTC().Truncate(time.Second)
		c.RefileAt = &t
	}
	c.CardSHA256 = CardSHA256(c)
	switch c.Kind {
	case CardDraft:
		if c.MediaSHA256 == nil {
			c.MediaSHA256 = []string{}
		}
		draft, body, media = c.DraftID, c.BodySHA256, c.MediaSHA256
	case CardBatch:
		batch, batchSHA, batchEntries = c.BatchID, c.BatchSHA256, c.BatchEntries
	case CardRefile, CardDrop:
		refusal = c.RefusalItem
		if c.Kind == CardRefile {
			refileAt = c.RefileAt
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChatCard{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One writer per (chat, officer) at a time, so the bound and the
	// "same card again" read below are one decision.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('officer_chat_cards:' || nullif(current_setting('muster.uid', true), '') || ':' || $1::bigint::text || ':' || $2::text, 0))`,
		c.OfficerOwner, c.Officer); err != nil {
		return ChatCard{}, err
	}
	same, err := scanChatCard(tx.QueryRow(ctx, `SELECT `+chatCardCols+` FROM officer_chat_cards
		WHERE owner_id = nullif(current_setting('muster.uid', true), '')::bigint AND officer_owner = $1 AND officer = $2
		  AND decided_at IS NULL AND card_sha256 = $3 ORDER BY id DESC LIMIT 1`, c.OfficerOwner, c.Officer, c.CardSHA256))
	switch {
	case err == nil:
		return same, tx.Commit(ctx)
	case !errors.Is(err, pgx.ErrNoRows):
		return ChatCard{}, err
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM officer_chat_cards
		WHERE owner_id = nullif(current_setting('muster.uid', true), '')::bigint AND officer_owner = $1 AND officer = $2 AND decided_at IS NULL`,
		c.OfficerOwner, c.Officer).Scan(&open); err != nil {
		return ChatCard{}, err
	}
	if open >= MaxOpenCards {
		return ChatCard{}, ErrTooManyCards
	}
	out, err := scanChatCard(tx.QueryRow(ctx, `INSERT INTO officer_chat_cards
		(officer_owner, officer, kind, draft_id, body_sha256, media_sha256, batch_id, batch_sha256, batch_entries, refusal_item, refile_at, card_sha256, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING `+chatCardCols,
		c.OfficerOwner, c.Officer, c.Kind, draft, body, media, batch, batchSHA, batchEntries, refusal, refileAt, c.CardSHA256, at))
	if err != nil {
		return ChatCard{}, classify(err)
	}
	return out, tx.Commit(ctx)
}

// ChatCards is the chat of ctx's principal with officerOwner's officer: its
// newest limit cards, oldest first.
func (s *Store) ChatCards(ctx context.Context, officerOwner int64, officer string, limit int) ([]ChatCard, error) {
	rows, err := s.pool.Query(ctx, `SELECT * FROM (SELECT `+chatCardCols+` FROM officer_chat_cards
		WHERE owner_id = nullif(current_setting('muster.uid', true), '')::bigint AND officer_owner = $1 AND officer = $2
		ORDER BY id DESC LIMIT $3) c ORDER BY id`, officerOwner, officer, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatCard
	for rows.Next() {
		c, err := scanChatCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChatCard is card id of ctx's principal's chat with officerOwner's officer;
// ErrNotFound for any other card (another person's, another officer's, gone).
func (s *Store) ChatCard(ctx context.Context, officerOwner int64, officer string, id int64) (ChatCard, error) {
	c, err := scanChatCard(s.pool.QueryRow(ctx, `SELECT `+chatCardCols+` FROM officer_chat_cards
		WHERE id = $1 AND owner_id = nullif(current_setting('muster.uid', true), '')::bigint AND officer_owner = $2 AND officer = $3`,
		id, officerOwner, officer))
	if errors.Is(err, pgx.ErrNoRows) {
		return ChatCard{}, ErrNotFound
	}
	return c, err
}

// DecideChatCard records card id's decision, once: ErrCardDecided when it was
// already decided, ErrNotFound when it is not the principal's card.
func (s *Store) DecideChatCard(ctx context.Context, id int64, decision string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE officer_chat_cards SET decided_at = $2, decision = $3
		WHERE id = $1 AND owner_id = nullif(current_setting('muster.uid', true), '')::bigint AND decided_at IS NULL`, id, at, decision)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM officer_chat_cards
		WHERE id = $1 AND owner_id = nullif(current_setting('muster.uid', true), '')::bigint`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return ErrCardDecided
}
