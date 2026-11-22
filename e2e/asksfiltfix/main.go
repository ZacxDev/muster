// Command asksfiltfix is the Asks page's filter-chip, scroll and video
// fixture (e2e/tests/asks-filters.spec.ts, asks-scroll.spec.ts and
// draft-video.spec.ts): one owner's queue holding items from several sources —
// an officer's questions, its outbound draft and the step-up card a SESSION
// filed for that draft, an agent's questions and reply draft, a mirrored
// session's questions, muster's notice — enough of them that the Open feed
// scrolls at a phone's height, and ANOTHER owner's officer items that must
// never reach the first owner's page. Written through the real attention store
// (its Clean, its constraints, row-level security as the owner) and as plain
// rows where the e2e harness has no door (a plugin, a draft).
//
//	DATABASE_URL=…_e2e go run ./e2e/asksfiltfix seed <owner> <stamp>    prints JSON ids
//	DATABASE_URL=…_e2e go run ./e2e/asksfiltfix video <owner> <stamp> <object-key> <sha256> <size>
//	DATABASE_URL=…_e2e go run ./e2e/asksfiltfix ping <owner> <stamp>     one more question (a live refresh)
//	DATABASE_URL=…_e2e go run ./e2e/asksfiltfix clear <owner> <stamp>
//
// `video` files a pending outbound X post draft whose one medium is an MP4
// stored under <object-key> (the spec puts the bytes in its fake S3), and the
// step-up card that asks to approve it, and prints {"draft":…, "ask":…}.
//
// Every row is synthetic. 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END
// IN "_e2e" (e2edb.Check).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
)

// The sources, as the fixture names them.
const (
	// Marketing is the owner's marketing officer (its asks' label is
	// "officer <id>", its drafts' officer column the id).
	Marketing = "marketing:acme"
	// Product is a second officer of the owner's, with one question.
	Product = "product:acme"
	// OtherOfficer is an officer only the OTHER owner has: its chip must
	// never be drawn for the owner.
	OtherOfficer = "sales:zeta"
	// SessionLabel names the mirrored session whose questions fill the feed.
	SessionLabel = "Release notes"
)

// Fillers is how many of the mirrored session's questions the seed files,
// open and answered each: with the rest, enough cards that the Open and the
// Answered tabs scroll at 390×844 and at 1440×900.
const Fillers = 10

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "asksfiltfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) < 3 {
		return fmt.Errorf("usage: asksfiltfix seed|clear <owner> <stamp> | video <owner> <stamp> <key> <sha256> <size>")
	}
	owner, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || owner <= 0 {
		return fmt.Errorf("owner %q is not a user id", args[1])
	}
	stamp := args[2]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	as := asUser(ctx, owner)
	switch args[0] {
	case "clear":
		return clear(ctx, dsn, stamp)
	case "seed":
		if err := clear(ctx, dsn, stamp); err != nil {
			return err
		}
		return seed(ctx, as, pool, dsn, owner, stamp)
	case "ping":
		// One more question from the mirrored session: the queue changes, so a
		// page's live refresh runs.
		_, err := attention.NewPG(pool).Create(as, attention.New{Asker: attention.Asker{MirrorHost: "ws-filters", MirrorSession: "6f0c2c1e-7d1a-4c55-9a39-0d6f3c1b2a10", Label: SessionLabel},
			Question: "filt: One more release note?", Options: []string{"Keep", "Drop"}})
		return err
	case "video":
		if len(args) != 6 {
			return fmt.Errorf("usage: video <owner> <stamp> <key> <sha256> <size>")
		}
		size, err := strconv.ParseInt(args[5], 10, 64)
		if err != nil {
			return fmt.Errorf("size %q: %w", args[5], err)
		}
		return video(as, pool, owner, stamp, args[3], args[4], size)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// asUser is ctx acting as user id (row-level security confines every write to
// that user's rows).
func asUser(ctx context.Context, id int64) context.Context {
	role := authz.RoleMember
	if id == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(ctx, authz.Principal{UserID: id, Role: role, Via: authz.ViaSession})
}

func agentName(stamp string) string { return "e2e-filt-" + stamp }
func otherName(stamp string) string { return "e2e-filt-other-" + stamp }

// clear removes everything seed and video wrote, the other user's rows too:
// over a plain connection as the database's owner (no principal, as the e2e
// seed itself runs), since no one user's row-level security reaches both.
func clear(ctx context.Context, dsn, stamp string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	name, other := agentName(stamp), otherName(stamp)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM attention_asks WHERE question LIKE 'filt: %'`, nil},
		{`DELETE FROM attention_asks WHERE asker_label = 'step-up' AND question LIKE '%filt-' || $1 || '%'`, []any{stamp}},
		{`DELETE FROM plugin_reply_drafts WHERE agent_id IN (SELECT id FROM agents WHERE name IN ($1, $2))`, []any{name, other}},
		{`DELETE FROM plugin_messages WHERE agent_id IN (SELECT id FROM agents WHERE name IN ($1, $2))`, []any{name, other}},
		{`DELETE FROM plugins WHERE name IN ($1 || '-inbox', $2 || '-inbox', $1 || '-x', $2 || '-x', $1 || '-video')`, []any{name, other}},
		{`DELETE FROM agents WHERE name IN ($1, $2)`, []any{name, other}},
		{`DELETE FROM users WHERE display_name = $1`, []any{other}},
	} {
		if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("%s: %w", q.sql, err)
		}
	}
	return nil
}

// sourceItems is one owner's rows: an agent, a plugin, and the items. It
// returns the ids by name.
type sourceItems map[string]int64

func seed(ctx context.Context, as context.Context, pool *pgxpool.Pool, dsn string, owner int64, stamp string) error {
	name := agentName(stamp)
	out := sourceItems{}
	st := attention.NewPG(pool)
	mk := func(c context.Context, key string, n attention.New) (attention.Ask, error) {
		n.Question = "filt: " + n.Question
		a, err := st.Create(c, n)
		if err != nil {
			return a, fmt.Errorf("%s: %w", key, err)
		}
		if key != "" {
			out[key] = a.ID
		}
		return a, nil
	}
	// THE OWNER'S ROWS, oldest first (the queue is newest first within a kind).
	var agentID int64
	if err := pool.QueryRow(as, `INSERT INTO agents (owner_id, name, namespace, display_name, status, kicked_off)
		VALUES ($1, $2, $3, 'Changelog writer', 'running', true) RETURNING id`, owner, name, "devpod-"+name).Scan(&agentID); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	out["agent"] = agentID
	agent := attention.Asker{AgentID: agentID, Label: name}
	marketing := attention.Asker{System: true, Label: "officer " + Marketing}
	product := attention.Asker{System: true, Label: "officer " + Product}
	session := attention.Asker{MirrorHost: "ws-filters", MirrorSession: "6f0c2c1e-7d1a-4c55-9a39-0d6f3c1b2a10", Label: SessionLabel}

	// Answered: one marketing question and one of the agent's.
	for _, a := range []struct {
		key   string
		asker attention.Asker
		q     string
	}{
		{"mkt_answered", marketing, "Use the blue banner for the launch post?"},
		{"agent_answered", agent, "Keep the old changelog format?"},
	} {
		x, err := mk(as, a.key, attention.New{Asker: a.asker, Question: a.q, Options: []string{"Yes", "No"}})
		if err != nil {
			return err
		}
		if _, err := st.Answer(as, x.ID, "Yes"); err != nil {
			return fmt.Errorf("answer %s: %w", a.key, err)
		}
	}
	for i := 1; i <= Fillers; i++ {
		// Answered too, so the Answered tab is as long as a page.
		x, err := mk(as, "", attention.New{Asker: session, Question: fmt.Sprintf("Earlier release note %d: keep it?", i), Options: []string{"Keep", "Drop"}})
		if err != nil {
			return err
		}
		if _, err := st.Answer(as, x.ID, "Keep"); err != nil {
			return fmt.Errorf("answer filler %d: %w", i, err)
		}
		if _, err := mk(as, "", attention.New{Asker: session, Question: fmt.Sprintf("Release note %d: keep this line in the summary?", i), Options: []string{"Keep", "Drop"}}); err != nil {
			return err
		}
	}
	if _, err := mk(as, "notice", attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, Notice: true,
		Question: "This session may be waiting on you: it stopped after asking about the schema."}); err != nil {
		return err
	}
	if _, err := mk(as, "product_ask", attention.New{Asker: product, Question: "Ship the onboarding checklist this week?", Options: []string{"Ship it", "Hold"}}); err != nil {
		return err
	}
	if _, err := mk(as, "agent_ask", attention.New{Asker: agent, Question: "Which release should the changelog start from?"}); err != nil {
		return err
	}
	if _, err := mk(as, "agent_ask2", attention.New{Asker: agent, Question: "Group the fixes by area?", Options: []string{"By area", "By date"}}); err != nil {
		return err
	}
	if _, err := mk(as, "session_ask", attention.New{Asker: session, Question: "Publish the release notes now?", Options: []string{"Publish", "Wait"}}); err != nil {
		return err
	}
	if _, err := mk(as, "mkt_ask2", attention.New{Asker: marketing, Question: "Post the launch thread at noon?", Options: []string{"Noon", "Later"}}); err != nil {
		return err
	}
	if _, err := mk(as, "mkt_ask", attention.New{Asker: marketing, Question: "Approve the launch tagline?", Options: []string{"Approve", "Rewrite"}}); err != nil {
		return err
	}
	// The drafts: the marketing officer's outbound post, the agent's reply.
	draft, reply, err := drafts(as, pool, owner, agentID, name, Marketing)
	if err != nil {
		return err
	}
	out["mkt_draft"], out["agent_draft"] = draft, reply
	// The step-up card a SESSION filed to approve the marketing draft: its
	// asker is muster ("step-up"), its source the draft's officer.
	card, err := st.Create(as, attention.New{
		Asker: attention.Asker{System: true, Label: "step-up"},
		StepUp: &attention.StepUpCard{Tool: "plugin_draft_decide", Title: fmt.Sprintf("Post to X? · Draft #%d", draft), Why: "Launch announcement",
			Asker: "work:1", Preview: "We launched. filt-" + stamp, Venue: "X", Platform: "x", Target: "post", DraftID: draft,
			Details: fmt.Sprintf("A session asks to approve draft %d.", draft)},
		Options: []string{"Confirm", "Refuse"},
	})
	if err != nil {
		return fmt.Errorf("step-up card: %w", err)
	}
	out["mkt_stepup"] = card.ID

	// THE OTHER OWNER: a member with the SAME marketing officer id and an
	// officer of its own, a question, an outbound draft and a step-up card.
	var otherID int64
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if err := conn.QueryRow(ctx, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, otherName(stamp)).Scan(&otherID); err != nil {
		return fmt.Errorf("other user: %w", err)
	}
	out["other_user"] = otherID
	oas := asUser(ctx, otherID)
	var otherAgent int64
	if err := pool.QueryRow(oas, `INSERT INTO agents (owner_id, name, namespace, display_name, status, kicked_off)
		VALUES ($1, $2, $3, 'Other writer', 'running', true) RETURNING id`, otherID, otherName(stamp), "devpod-"+otherName(stamp)).Scan(&otherAgent); err != nil {
		return fmt.Errorf("other agent: %w", err)
	}
	if _, err := mk(oas, "other_mkt_ask", attention.New{Asker: marketing, Question: "OTHER OWNER: approve their tagline?", Options: []string{"Yes", "No"}}); err != nil {
		return err
	}
	if _, err := mk(oas, "other_sales_ask", attention.New{Asker: attention.Asker{System: true, Label: "officer " + OtherOfficer}, Question: "OTHER OWNER: call the lead?", Options: []string{"Yes", "No"}}); err != nil {
		return err
	}
	odraft, _, err := drafts(oas, pool, otherID, otherAgent, otherName(stamp), OtherOfficer)
	if err != nil {
		return fmt.Errorf("other drafts: %w", err)
	}
	out["other_draft"] = odraft
	j, _ := json.Marshal(out)
	fmt.Println(string(j))
	return nil
}

// drafts files, as ctx's owner, an outbound X post by officer and a reply
// draft by the agent, and returns their ids.
func drafts(ctx context.Context, pool *pgxpool.Pool, owner, agentID int64, name, officer string) (outbound, reply int64, err error) {
	var inbox, x, msg int64
	if err = pool.QueryRow(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES ($1, $2 || '-inbox', 'Example inbox', 'e2e-fixture-' || $2 || '-' || gen_random_uuid()::text, $2 || '-inbox') RETURNING id`, owner, name).Scan(&inbox); err != nil {
		return 0, 0, fmt.Errorf("inbox plugin: %w", err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES ($1, $2 || '-x', 'X', 'e2e-fixture-x-' || $2 || '-' || gen_random_uuid()::text, $2 || '-x') RETURNING id`, owner, name).Scan(&x); err != nil {
		return 0, 0, fmt.Errorf("x plugin: %w", err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, sender, body, status, delivered_at, agent_id)
		VALUES ($1, $2, 'filt-1', 'someone', 'Is the changelog out yet?', 'delivered', now(), $3) RETURNING id`, owner, inbox, agentID).Scan(&msg); err != nil {
		return 0, 0, fmt.Errorf("message: %w", err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES ($1, $2, $3, $4, 'Tomorrow morning.', now() + interval '1 hour') RETURNING id`, owner, msg, agentID, name).Scan(&reply); err != nil {
		return 0, 0, fmt.Errorf("reply draft: %w", err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, plugin_id, venue, target_kind, officer, media_refs, media)
		VALUES ($1, $2, $3, 'We launched today. Read the post on the blog.', now() + interval '1 hour', $4, 'x', 'post', $5, '{}', '[]'::jsonb) RETURNING id`,
		owner, agentID, name, x, officer).Scan(&outbound); err != nil {
		return 0, 0, fmt.Errorf("outbound draft: %w", err)
	}
	return outbound, reply, nil
}

// video files the outbound draft whose one medium is the MP4 at key, and its
// step-up card, as the owner.
func video(ctx context.Context, pool *pgxpool.Pool, owner int64, stamp, key, sha string, size int64) error {
	name := agentName(stamp)
	var agentID, x, draft int64
	if err := pool.QueryRow(ctx, `SELECT id FROM agents WHERE owner_id = $1 AND name = $2`, owner, name).Scan(&agentID); err != nil {
		if err := pool.QueryRow(ctx, `INSERT INTO agents (owner_id, name, namespace, display_name, status, kicked_off)
			VALUES ($1, $2, $3, 'Changelog writer', 'running', true) RETURNING id`, owner, name, "devpod-"+name).Scan(&agentID); err != nil {
			return fmt.Errorf("agent: %w", err)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES ($1, $2 || '-video', 'X', 'e2e-fixture-v-' || $2 || '-' || gen_random_uuid()::text, $2 || '-video') RETURNING id`, owner, name).Scan(&x); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	const ref = "launch/film"
	media, _ := json.Marshal([]map[string]any{{"ref": ref, "sha256": sha, "content_type": "video/mp4", "size": size, "key": key, "uploaded_at": time.Now().UTC()}})
	if err := pool.QueryRow(ctx, `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, plugin_id, venue, target_kind, officer, media_refs, media)
		VALUES ($1, $2, $3, 'Watch the launch film.', now() + interval '1 hour', $4, 'x', 'post', $5, $6, $7::jsonb) RETURNING id`,
		owner, agentID, name, x, Marketing, []string{ref}, string(media)).Scan(&draft); err != nil {
		return fmt.Errorf("draft: %w", err)
	}
	card, err := attention.NewPG(pool).Create(ctx, attention.New{
		Asker: attention.Asker{System: true, Label: "step-up"},
		StepUp: &attention.StepUpCard{Tool: "plugin_draft_decide", Title: fmt.Sprintf("Post to X? · Draft #%d", draft), Why: "Launch film",
			Asker: "work:1", Preview: "Watch the launch film. filt-" + stamp, Venue: "X", Platform: "x", Target: "post", DraftID: draft,
			Media:   []attention.StepUpMedium{{N: 0, Ref: ref, Type: "video/mp4", SHA256: strings.ToLower(sha)}},
			Details: fmt.Sprintf("A session asks to approve draft %d.", draft)},
		Options: []string{"Confirm", "Refuse"},
	})
	if err != nil {
		return fmt.Errorf("card: %w", err)
	}
	j, _ := json.Marshal(map[string]int64{"draft": draft, "ask": card.ID})
	fmt.Println(string(j))
	return nil
}
