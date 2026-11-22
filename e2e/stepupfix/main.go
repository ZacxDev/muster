// Command stepupfix is the step-up card page spec's fixture
// (e2e/tests/stepup-card-page.spec.ts): one step-up confirmation card as
// muster files it for a draft's approval — its parts (internal/attention
// StepUpCard), written through the real attention store (attention.Clean
// builds its question from them; row-level security as in production) — so
// the spec can open its page. What is under test is how the page DRAWS a
// card, not how the gate files one (internal/api host_stepup_card_pg_test.go
// drives that through the real mirror door).
//
//	DATABASE_URL=…_e2e go run ./e2e/stepupfix card <owner>    prints the card's id
//	DATABASE_URL=…_e2e go run ./e2e/stepupfix thread <owner>  a long X thread's card (an excerpt)
//	DATABASE_URL=…_e2e go run ./e2e/stepupfix lookalike <owner>  a post with look-alike letters
//	DATABASE_URL=…_e2e go run ./e2e/stepupfix draftcard <owner> <draft-id> [reject]  a card about a REAL draft (prints its id)
//	DATABASE_URL=…_e2e go run ./e2e/stepupfix clear <owner>
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check).
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/plugins"
)

// The card's parts (synthetic): an X post of two paragraphs and one image,
// for draft 41 (the fixture's draft id; the spec serves its medium).
const (
	Title   = "Post to X? · Draft #41"
	Why     = "Marketing: rebrand announcement"
	Asker   = "work:1"
	Preview = "Big news: acme is now acme-two.\nRead more on the blog. <b>not bold</b>\n\nA second paragraph."
	Details = "A session on host-a (work:1 @ host-a) asks to approve or reject a reply or outbound draft as you.\nThe call: plugin_draft_decide, with id 41, decision “approve”." +
		"\n\nThis machine runs host officer acme:main, and its sessions are named by a header any process there can set — so this needs you."
	DraftID = 41
)

// The long thread's card, as the gate files one for an X thread of 20 posts
// of 257 characters (internal/api stepUpCutPreview, which the Go tests
// drive): its first 9 whole posts, the whole thread's count, and the line
// saying it is an excerpt.
const (
	ThreadParts   = 20
	ThreadShown   = 9
	ThreadExcerpt = "⚠ AN EXCERPT: the first 9 of the thread's 20 posts. Refuse unless you have read the rest in muster's web UI."
)

// ThreadPart is part i (1-based) of the long thread: 257 characters.
func ThreadPart(i int) string {
	p := "Post " + strconv.Itoa(100 + i)[1:] + " of the launch thread:"
	for len(p) < 257 {
		p += " word"
	}
	return p[:257]
}

// Lookalike is a post with a Cyrillic а in a domain, a pure-Cyrillic word,
// an accented word, emoji and a struck-through word (U+0336).
const Lookalike = "Claim your refund at p\u0430ypal.com/refund before Friday.\n\n" +
	"\u041F\u0440\u0438\u0432\u0435\u0442, caf\u00E9 friends \u2615 \U0001F389 \u2014 " +
	"S\u0336A\u0336L\u0336E\u0336 ends soon."

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "stepupfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: stepupfix card <owner> | draftcard <owner> <draft-id> [reject] | clear <owner>")
	}
	owner, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || owner <= 0 {
		return fmt.Errorf("owner %q is not a user id", args[1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	role := authz.RoleMember
	if owner == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	as := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: role, Via: authz.ViaSession})
	switch args[0] {
	case "draftcard":
		// A card a host session filed for plugin_draft_decide on one of the
		// owner's real drafts: the Open feed draws that draft's own decision
		// controls on it (internal/api stepup_draft_actions.go).
		if len(args) < 3 {
			return fmt.Errorf("usage: stepupfix draftcard <owner> <draft-id> [reject]")
		}
		draft, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil || draft <= 0 {
			return fmt.Errorf("draft %q is not an id", args[2])
		}
		title, why, decision := "Post to X? · Draft #"+args[2], Why, "approve"
		if len(args) > 3 && args[3] == "reject" {
			title, why, decision = "Reject this draft? · Draft #"+args[2], "Reject this draft", "reject"
		}
		// The card shows the draft's own text and media, as the gate files
		// it: only then may its face offer the draft's Approve.
		dc, err := plugins.NewPG(pool).DraftCard(as, draft)
		if err != nil {
			return fmt.Errorf("read draft %d: %w", draft, err)
		}
		var media []attention.StepUpMedium
		for i, ref := range dc.Draft.MediaRefs {
			if m, ok := dc.Draft.MediaFor(ref); ok {
				media = append(media, attention.StepUpMedium{N: i, Ref: ref, Type: m.ContentType, SHA256: m.SHA256})
			}
		}
		card := &attention.StepUpCard{Tool: "plugin_draft_decide", Title: title, Why: why, Asker: Asker, Media: media,
			Preview: dc.Draft.Body, Venue: "X", Platform: "x", Target: "post", DraftID: draft,
			Details: "A session on host-a (work:1 @ host-a) asks to approve or reject a reply or outbound draft as you.\nThe call: plugin_draft_decide, with id " +
				args[2] + ", decision “" + decision + "”."}
		a, err := attention.NewPG(pool).Create(as, attention.New{
			Asker:   attention.Asker{System: true, Label: "step-up"},
			StepUp:  card,
			Options: []string{"Confirm", "Refuse"},
		})
		if err != nil {
			return err
		}
		fmt.Println(a.ID)
		return nil
	case "clear":
		_, err := pool.Exec(as, `DELETE FROM attention_asks WHERE asker_label = 'step-up'`)
		return err
	case "card", "thread", "lookalike", "answered":
		card := &attention.StepUpCard{Tool: "plugin_draft_decide", Title: Title, Why: Why, Asker: Asker,
			Preview: strings.TrimSpace(Preview), Venue: "X", Platform: "x", Target: "post", DraftID: DraftID,
			Media:   []attention.StepUpMedium{{N: 0, Ref: "launch/hero-1", Type: "image/png", SHA256: strings.Repeat("0a", 32)}},
			Details: Details}
		switch args[0] {
		case "thread":
			var parts []string
			for i := 1; i <= ThreadShown; i++ {
				parts = append(parts, ThreadPart(i))
			}
			card.Preview, card.Parts, card.Excerpt = strings.Join(parts, "\n---8<---\n"), ThreadParts, ThreadExcerpt
		case "lookalike":
			card.Preview = Lookalike
		}
		a, err := attention.NewPG(pool).Create(as, attention.New{
			Asker:   attention.Asker{System: true, Label: "step-up"},
			StepUp:  card,
			Options: []string{"Confirm", "Refuse"},
		})
		if err != nil {
			return err
		}
		// answered: the card as an answer settles it (written as a row: the
		// owner's browser cannot answer a step-up card).
		if args[0] == "answered" {
			if _, err := pool.Exec(as, `UPDATE attention_asks SET status = 'answered', answer = 'Confirm', answered_by = 'operator', resolved_at = now() WHERE id = $1`, a.ID); err != nil {
				return fmt.Errorf("answered: %w", err)
			}
		}
		fmt.Println(a.ID)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
