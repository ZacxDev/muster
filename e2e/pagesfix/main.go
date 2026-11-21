// Command pagesfix is the Pages screens spec's fixture
// (e2e/tests/pages-declutter.spec.ts): an owner's pages, written through the
// real store (internal/pages: row-level security, versions, links and
// responses as in production), whose counts are pairwise distinct so each
// badge the list draws can only come from one place.
//
//	DATABASE_URL=…_e2e MUSTER_CONNECTOR_ENCRYPTION_KEY=<base64 32 bytes> \
//	  go run ./e2e/pagesfix seed <owner>    prints the pages as JSON
//	DATABASE_URL=…_e2e go run ./e2e/pagesfix clear <owner>
//	DATABASE_URL=…_e2e MUSTER_CONNECTOR_ENCRYPTION_KEY=… \
//	  go run ./e2e/pagesfix phone <owner>    the phone page (pages-phone.spec.ts), {page, token}
//	DATABASE_URL=…_e2e go run ./e2e/pagesfix phone-clear <owner>
//
// phone and phone-clear touch only the page titled PhoneTitle.
//
// The pages (all synthetic), each made by a mirrored session ("Release
// helper" on host "laptop"), times set back so each reads as a real age:
//
//   - "Quarter plan": 4 versions, no link, no reply;
//   - "Launch notes": 1 version, its live share link, 3 replies;
//   - "Venue survey": 1 version, 2 links waiting for the owner, 1 live
//     one-off link, 2 revoked and 1 declined;
//   - "Budget review": 2 versions, its live share link, 1 reply, 1 waiting;
//   - a page whose long title must wrap whole words, ending in one unbroken
//     string longer than a phone is wide: 1 version, nothing else.
//
// seed first deletes every page of the owner's (this e2e database's only).
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/pages"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pagesfix:", err)
		os.Exit(1)
	}
}

// LongTitle is the wrapping page's title: whole words, then one unbroken run.
const LongTitle = "Launch thread: approve the announcement text and the media before anything is posted " +
	"Supercalifragilisticexpialidociouslylongunbrokenwordthatisfarwiderthanaphone"

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 2 || (args[0] != "seed" && args[0] != "clear" && args[0] != "phone" && args[0] != "phone-clear") {
		return fmt.Errorf("usage: pagesfix seed <owner> | clear <owner> | phone <owner> | phone-clear <owner>")
	}
	owner, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || owner <= 0 {
		return fmt.Errorf("owner %q is not a user id", args[1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	// AS the owner, never the system scope: row-level security confines the
	// seed to that owner's rows.
	as := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: role, Via: authz.ViaSession})
	if strings.HasPrefix(args[0], "phone") {
		// Only the phone page: every other page of the owner's is left alone.
		if _, err := pool.Exec(as, `DELETE FROM pages WHERE title = $1`, PhoneTitle); err != nil || args[0] == "phone-clear" {
			return err
		}
	} else if _, err := pool.Exec(as, `DELETE FROM pages`); err != nil || args[0] == "clear" {
		return err
	}
	// The server's link key, so the share links made here are its own.
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("MUSTER_CONNECTOR_ENCRYPTION_KEY")))
	if err != nil {
		return fmt.Errorf("MUSTER_CONNECTOR_ENCRYPTION_KEY: %w", err)
	}
	st, err := pages.NewPG(pool).WithLinkKey(key)
	if err != nil {
		return err
	}
	if args[0] == "phone" {
		return phone(as, st)
	}
	out, err := seed(as, st)
	if err != nil {
		return err
	}
	// Set the times back, so each reads as a real age: page i's version v was
	// made (5*i + 5 - v) hours ago, and the page updated with its newest.
	for i, p := range out.order {
		if _, err := pool.Exec(as, `UPDATE page_versions SET created_at = now() - make_interval(hours => ($2::int * 5 + 5 - version)::int) WHERE page_id = $1`, p, i); err != nil {
			return err
		}
		if _, err := pool.Exec(as, `UPDATE pages SET updated_at = (SELECT max(created_at) FROM page_versions WHERE page_id = $1) WHERE id = $1`, p); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

// seeded is what seed prints: each page's id, and the live links' tokens.
type seeded struct {
	Plan    int64  `json:"plan"`
	Notes   int64  `json:"notes"`
	Survey  int64  `json:"survey"`
	Budget  int64  `json:"budget"`
	Long    int64  `json:"long"`
	Token   string `json:"token"` // Launch notes' live link
	order   []int64
	creator pages.Creator
}

func seed(ctx context.Context, st *pages.PGStore) (seeded, error) {
	out := seeded{creator: pages.Creator{MirrorHost: "laptop", MirrorSession: "pagesfix-session", Label: "Release helper"}}
	blocks := func(text string) []pages.Block {
		return []pages.Block{{Type: "markdown", Text: text}, {Type: "question", ID: "q1", Label: "Which day suits you?"}}
	}
	page := func(title string, versions int) (int64, error) {
		p, _, err := st.Create(ctx, out.creator, title, blocks("# "+title+"\n\nThe first version."), 400)
		if err != nil {
			return 0, err
		}
		for v := 2; v <= versions; v++ {
			if _, _, err := st.AddVersion(ctx, out.creator, p.ID, title, blocks("# "+title+"\n\nVersion "+strconv.Itoa(v)+"."), 400+v); err != nil {
				return 0, err
			}
		}
		out.order = append(out.order, p.ID)
		return p.ID, nil
	}
	live := func(id int64, replies ...string) (pages.Link, error) {
		l, _, err := st.ShareLink(ctx, id, "you", false)
		if err != nil {
			return l, err
		}
		for _, r := range replies {
			p, err := st.Page(ctx, id)
			if err != nil {
				return l, err
			}
			if _, err := st.AddResponse(ctx, l, p.CurrentVersion, "q1", pages.KindSubmit, []byte(`{"answer":"`+r+`"}`), ""); err != nil {
				return l, err
			}
		}
		return l, nil
	}
	pending := func(id int64, n int) error {
		for range n {
			if _, err := st.CreateLink(ctx, id, pages.LinkPending, "Release helper", true); err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	// Created oldest first, so the list (newest first) reads top to bottom
	// as below.
	if out.Long, err = page(LongTitle, 1); err != nil {
		return out, err
	}
	if out.Budget, err = page("Budget review", 2); err != nil {
		return out, err
	}
	if _, err = live(out.Budget, "Thursday"); err != nil {
		return out, err
	}
	if err = pending(out.Budget, 1); err != nil {
		return out, err
	}
	if out.Survey, err = page("Venue survey", 1); err != nil {
		return out, err
	}
	if err = pending(out.Survey, 2); err != nil {
		return out, err
	}
	// Its link history: one live one-off link, two revoked, one declined
	// (muster has no expired state).
	if _, err = st.CreateLink(ctx, out.Survey, pages.LinkLive, "you", false); err != nil {
		return out, err
	}
	for range 2 {
		l, err := st.CreateLink(ctx, out.Survey, pages.LinkLive, "you", false)
		if err != nil {
			return out, err
		}
		if _, err = st.RevokeLink(ctx, out.Survey, l.ID); err != nil {
			return out, err
		}
	}
	asked, err := st.CreateLink(ctx, out.Survey, pages.LinkPending, "Release helper", true)
	if err != nil {
		return out, err
	}
	if _, err = st.DecideLink(ctx, asked.ID, false); err != nil {
		return out, err
	}
	if out.Notes, err = page("Launch notes", 1); err != nil {
		return out, err
	}
	l, err := live(out.Notes, "Monday", "Tuesday", "Friday")
	if err != nil {
		return out, err
	}
	out.Token = l.Token
	if out.Plan, err = page("Quarter plan", 4); err != nil {
		return out, err
	}
	// order: newest page first, so the youngest is the first one listed.
	for i, j := 0, len(out.order)-1; i < j; i, j = i+1, j-1 {
		out.order[i], out.order[j] = out.order[j], out.order[i]
	}
	return out, nil
}

// PhoneTitle is the phone page's title (e2e/tests/pages-phone.spec.ts).
const PhoneTitle = "Phone check"

// PhonePNG is a real 2x2 PNG, base64 (the pages spec's).
const PhonePNG = "iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8AAQv8ZYAwAQ84H+VjtZqAAAAAASUVORK5CYII="

// phoneBlocks is the phone page: the shapes that made a page published on a
// server with uploads off unreadable on a phone — a long paragraph ending in
// one unbroken run wider than the screen, a long inline-code URL, a 9-column
// table and an inline data: image in markdown.
func phoneBlocks() []pages.Block {
	row := func(cells ...string) string { return "| " + strings.Join(cells, " | ") + " |" }
	table := strings.Join([]string{
		row("Service", "Region", "Status", "Owner", "Latency", "Errors", "Version", "Window", "Notes"),
		row("---", "---", "---", "---", "---", "---", "---", "---", "---"),
		row("web", "east", "healthy", "team a", "120ms", "0.1%", "v1.2.3", "Mon", "steady"),
		row("api", "west", "degraded", "team b", "480ms", "2.4%", "v1.2.2", "Tue", "watch it"),
	}, "\n")
	text := "# Phone check\n\n" +
		"This paragraph runs long on purpose, the way a status report does when it explains every step that was taken, " +
		"and it ends in one unbroken run: " + strings.Repeat("unbroken", 16) + ".\n\n" +
		"The dashboard is `https://dashboard.example.com/services/web/regions/east/latency?window=7d&percentile=p99&compare=previous`.\n\n" +
		table + "\n\n" +
		"![status chart](data:image/png;base64," + PhonePNG + ")"
	return []pages.Block{{Type: pages.BlockMarkdown, Text: text}}
}

// phone publishes the phone page with a live link and prints {page, token}.
func phone(ctx context.Context, st *pages.PGStore) error {
	cr := pages.Creator{MirrorHost: "laptop", MirrorSession: "pagesfix-session", Label: "Release helper"}
	blocks, size, err := pages.Validate(phoneBlocks())
	if err != nil {
		return err
	}
	p, _, err := st.Create(ctx, cr, PhoneTitle, blocks, size)
	if err != nil {
		return err
	}
	l, _, err := st.ShareLink(ctx, p.ID, "you", false)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"page": p.ID, "token": l.Token})
}
