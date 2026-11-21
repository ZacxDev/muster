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
)

// The card's parts (synthetic): an X post of two paragraphs and one image,
// for draft 41 (the fixture's draft id; the spec serves its medium).
const (
	Title   = "Post to X? · #41"
	Why     = "Marketing: rebrand announcement"
	Asker   = "work:1"
	Preview = "Big news: acme is now acme-two.\nRead more on the blog. <b>not bold</b>\n\nA second paragraph."
	Details = "A session on host-a (work:1 @ host-a) asks to approve or reject a reply or outbound draft as you.\nThe call: plugin_draft_decide, with id 41, decision “approve”." +
		"\n\nThis machine runs host officer acme:main, and its sessions are named by a header any process there can set — so this needs you."
	DraftID = 41
)

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
	if len(args) != 2 {
		return fmt.Errorf("usage: stepupfix card <owner> | clear <owner>")
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
	case "clear":
		_, err := pool.Exec(as, `DELETE FROM attention_asks WHERE asker_label = 'step-up'`)
		return err
	case "card":
		a, err := attention.NewPG(pool).Create(as, attention.New{
			Asker: attention.Asker{System: true, Label: "step-up"},
			StepUp: &attention.StepUpCard{Tool: "plugin_draft_decide", Title: Title, Why: Why, Asker: Asker,
				Preview: strings.TrimSpace(Preview), Venue: "X", Platform: "x", Target: "post", DraftID: DraftID,
				Media:   []attention.StepUpMedium{{N: 0, Ref: "launch/hero-1", Type: "image/png", SHA256: strings.Repeat("0a", 32)}},
				Details: Details},
			Options: []string{"Confirm", "Refuse"},
		})
		if err != nil {
			return err
		}
		fmt.Println(a.ID)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
