package pages

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Users in this package's scratch database: 1 is the seeded operator; 2 and 3
// are members, each its own owner.
func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE pages CASCADE`)
	return NewPG(pool), pool
}

func asUser(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

var (
	agentA = Creator{AgentID: 501, Label: "worker-a"}
	agentB = Creator{AgentID: 502, Label: "worker-b"}
	hostW  = Creator{MirrorHost: "workstation", Label: "workstation · build"}
)

func blocksOf(t *testing.T, raw string) ([]Block, int) {
	t.Helper()
	b, n, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return b, n
}

const qBlocks = `[{"type":"question","id":"q1","label":"Which date?"}]`

// TestAPageIsItsOwnersAndItsCreators: a page is created as the principal's
// user; another user reads none of it (page, versions, links, responses) and
// changes nothing; another CREATOR of the same owner cannot reach it through
// the …For methods, while the owner's own reads (any creator) can.
func TestAPageIsItsOwnersAndItsCreators(t *testing.T) {
	s, _ := pgStore(t)
	two, three := asUser(2), asUser(3)
	b, n := blocksOf(t, qBlocks)
	p, v, err := s.Create(two, agentA, "Launch", b, n)
	if err != nil {
		t.Fatal(err)
	}
	if p.OwnerID != 2 || p.CurrentVersion != 1 || v.Version != 1 || p.Creator.AgentID != agentA.AgentID {
		t.Fatalf("created %+v %+v", p, v)
	}
	l, err := s.CreateLink(two, p.ID, LinkLive, "you")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResponse(two, l, 1, "q1", KindSubmit, []byte(`{"answer":"May"}`), ""); err != nil {
		t.Fatal(err)
	}
	// User 3: nothing.
	if _, err := s.Page(three, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 read user 2's page: %v", err)
	}
	if _, err := s.PageFor(three, agentA, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 read user 2's page as its creator: %v", err)
	}
	if list, _ := s.Pages(three); len(list) != 0 {
		t.Errorf("user 3 lists %d of user 2's pages", len(list))
	}
	if vs, _ := s.Versions(three, p.ID); len(vs) != 0 {
		t.Errorf("user 3 reads %d versions", len(vs))
	}
	if _, err := s.Version(three, p.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 read a version: %v", err)
	}
	if ls, _ := s.Links(three, p.ID); len(ls) != 0 {
		t.Errorf("user 3 reads %d links", len(ls))
	}
	if rs, _ := s.Responses(three, p.ID, 0, 10); len(rs) != 0 {
		t.Errorf("user 3 reads %d responses", len(rs))
	}
	if _, _, err := s.AddVersion(three, agentA, p.ID, "Hijack", b, n); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 added a version: %v", err)
	}
	if _, err := s.SetCurrent(three, p.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 rolled back: %v", err)
	}
	if _, err := s.CreateLink(three, p.ID, LinkLive, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 minted a link: %v", err)
	}
	if _, err := s.RevokeLink(three, p.ID, l.ID); !errors.Is(err, ErrLinkNotFound) {
		t.Errorf("user 3 revoked a link: %v", err)
	}
	if _, err := s.DecideLink(three, l.ID, true); err == nil {
		t.Error("user 3 decided a link")
	}
	// Another creator of the same owner: not through the …For methods.
	if _, err := s.PageFor(two, agentB, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("agent B read agent A's page: %v", err)
	}
	if _, err := s.PageFor(two, hostW, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a host read an agent's page: %v", err)
	}
	if _, _, err := s.AddVersion(two, agentB, p.ID, "Mine now", b, n); !errors.Is(err, ErrNotFound) {
		t.Errorf("agent B added a version to agent A's page: %v", err)
	}
	if list, _ := s.PagesFor(two, agentB); len(list) != 0 {
		t.Errorf("agent B lists %d of agent A's pages", len(list))
	}
	// The positive controls, after everyone else acted.
	got, err := s.PageFor(two, agentA, p.ID)
	if err != nil || got.Title != "Launch" || got.CurrentVersion != 1 {
		t.Fatalf("agent A's own read: %+v %v", got, err)
	}
	list, err := s.Pages(two)
	if err != nil || len(list) != 1 || list[0].LiveLinks != 1 || list[0].Responses != 1 || list[0].Versions != 1 {
		t.Fatalf("user 2's list: %+v %v", list, err)
	}
	mine, _ := s.PagesFor(two, agentA)
	if len(mine) != 1 {
		t.Fatalf("agent A lists %d pages, want 1", len(mine))
	}
}

// TestVersionsAndRollback: an update adds the next version and makes it
// current; a link serves whatever is current; a rollback moves the pointer
// back and keeps every version.
func TestVersionsAndRollback(t *testing.T) {
	s, _ := pgStore(t)
	two := asUser(2)
	b1, n1 := blocksOf(t, `[{"type":"markdown","text":"one"}]`)
	b2, n2 := blocksOf(t, `[{"type":"markdown","text":"two"}]`)
	p, _, err := s.Create(two, hostW, "Showcase", b1, n1)
	if err != nil {
		t.Fatal(err)
	}
	p, v2, err := s.AddVersion(two, hostW, p.ID, "Showcase v2", b2, n2)
	if err != nil || v2.Version != 2 || p.CurrentVersion != 2 || p.Title != "Showcase v2" {
		t.Fatalf("AddVersion: %+v %+v %v", p, v2, err)
	}
	cur, err := s.Version(two, p.ID, 0)
	if err != nil || cur.Version != 2 || cur.Blocks[0].Text != "two" {
		t.Fatalf("current: %+v %v", cur, err)
	}
	if _, err := s.SetCurrent(two, p.ID, 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("rolled back to a version that does not exist: %v", err)
	}
	if _, err := s.SetCurrent(two, p.ID, 1); err != nil {
		t.Fatal(err)
	}
	cur, _ = s.Version(two, p.ID, 0)
	if cur.Version != 1 || cur.Blocks[0].Text != "one" {
		t.Fatalf("after rollback the current version is %d (%q)", cur.Version, cur.Blocks[0].Text)
	}
	if vs, _ := s.Versions(two, p.ID); len(vs) != 2 || vs[0].Version != 2 {
		t.Fatalf("versions after rollback: %+v", vs)
	}
	old, err := s.Version(two, p.ID, 2)
	if err != nil || old.Blocks[0].Text != "two" {
		t.Fatalf("version 2 is still readable: %+v %v", old, err)
	}
}

// TestALinkServesOnlyWhileLive walks a link's states through LinkByToken —
// the System read that admits a viewer: pending serves nothing, approved
// serves, revoked serves nothing again; denied never serves; a malformed or
// unknown token never reaches a row.
func TestALinkServesOnlyWhileLive(t *testing.T) {
	s, _ := pgStore(t)
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, err := s.Create(two, agentA, "Ask", b, n)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.CreateLink(two, p.ID, LinkPending, "worker-a")
	if err != nil || l.State != LinkPending || l.DecidedAt != nil || !TokenShapeOK(l.Token) {
		t.Fatalf("pending link: %+v %v", l, err)
	}
	if _, err := s.LinkByToken(context.Background(), l.Token); !errors.Is(err, ErrLinkNotFound) {
		t.Errorf("a pending link served: %v", err)
	}
	if err := s.SetLinkAsk(two, l.ID, 77); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LinkByAsk(two, 77); err != nil || got.ID != l.ID {
		t.Fatalf("LinkByAsk: %+v %v", got, err)
	}
	if _, err := s.LinkByAsk(asUser(3), 77); !errors.Is(err, ErrLinkNotFound) {
		t.Errorf("user 3 found user 2's link by its ask: %v", err)
	}
	if _, err := s.DecideLink(two, l.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideLink(two, l.ID, false); !errors.Is(err, ErrNotPending) {
		t.Errorf("a link was decided twice: %v", err)
	}
	got, err := s.LinkByToken(context.Background(), l.Token)
	if err != nil || got.ID != l.ID || got.OwnerID != 2 || got.PageID != p.ID {
		t.Fatalf("a live link: %+v %v", got, err)
	}
	if _, err := s.RevokeLink(two, p.ID, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkByToken(context.Background(), l.Token); !errors.Is(err, ErrLinkNotFound) {
		t.Errorf("a revoked link served: %v", err)
	}
	if again, err := s.RevokeLink(two, p.ID, l.ID); err != nil || again.State != LinkRevoked {
		t.Errorf("revoking twice: %+v %v", again, err)
	}
	// Denied.
	d, _ := s.CreateLink(two, p.ID, LinkPending, "worker-a")
	if _, err := s.DecideLink(two, d.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkByToken(context.Background(), d.Token); !errors.Is(err, ErrLinkNotFound) {
		t.Errorf("a denied link served: %v", err)
	}
	e, _ := s.CreateLink(two, p.ID, LinkLive, "you")
	if _, err := s.LinkByToken(context.Background(), e.Token); err != nil {
		t.Fatalf("control: a live link: %v", err)
	}
	for _, bad := range []string{"", "nope", e.Token + "x", e.Token[:42] + "!"} {
		if _, err := s.LinkByToken(context.Background(), bad); !errors.Is(err, ErrLinkNotFound) {
			t.Errorf("LinkByToken(%q): %v", bad, err)
		}
	}
	if _, err := s.LinkByToken(context.Background(), "A"+e.Token[1:]); !errors.Is(err, ErrLinkNotFound) && e.Token[0] != 'A' {
		t.Errorf("a token one character off served: %v", err)
	}
}

// TestResponsesNeedALiveLinkAndStopAtTheCaps: a response through a link
// revoked meanwhile records nothing; the per-link caps refuse with ErrTooMany.
func TestResponsesNeedALiveLinkAndStopAtTheCaps(t *testing.T) {
	s, _ := pgStore(t)
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, _ := s.Create(two, agentA, "Caps", b, n)
	l, _ := s.CreateLink(two, p.ID, LinkLive, "you")
	defer func(a, f int) { MaxResponsesPerLink, MaxUploadsPerLink = a, f }(MaxResponsesPerLink, MaxUploadsPerLink)
	MaxResponsesPerLink, MaxUploadsPerLink = 3, 1
	if _, err := s.AddResponse(two, l, 1, "u1", KindUpload, nil, "file-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResponse(two, l, 1, "u1", KindUpload, nil, "file-2"); !errors.Is(err, ErrTooMany) {
		t.Errorf("a second file past MaxUploadsPerLink: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.AddResponse(two, l, 1, "q1", KindSubmit, []byte(`{"answer":"x"}`), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddResponse(two, l, 1, "q1", KindSubmit, []byte(`{"answer":"x"}`), ""); !errors.Is(err, ErrTooMany) {
		t.Errorf("a fourth response past MaxResponsesPerLink: %v", err)
	}
	rs, err := s.Responses(two, p.ID, 0, 10)
	if err != nil || len(rs) != 3 || rs[0].Kind != KindUpload || rs[0].UploadPublicID != "file-1" || string(rs[1].Data) != `{"answer": "x"}` {
		t.Fatalf("responses: %+v %v", rs, err)
	}
	if after, _ := s.Responses(two, p.ID, rs[1].ID, 10); len(after) != 1 || after[0].ID != rs[2].ID {
		t.Fatalf("after %d: %+v", rs[1].ID, after)
	}
	// Revoked mid-flight: the row says so, whatever the caller holds.
	other, _ := s.CreateLink(two, p.ID, LinkLive, "you")
	if _, err := s.RevokeLink(two, p.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResponse(two, other, 1, "q1", KindSubmit, []byte(`{"answer":"x"}`), ""); !errors.Is(err, ErrLinkNotFound) {
		t.Errorf("a response through a revoked link: %v", err)
	}
}

func TestTheOpenLinkAndPageCaps(t *testing.T) {
	s, _ := pgStore(t)
	two := asUser(2)
	defer func(l, p, v int) { MaxOpenLinksPerPage, MaxPagesPerOwner, MaxVersionsPerPage = l, p, v }(MaxOpenLinksPerPage, MaxPagesPerOwner, MaxVersionsPerPage)
	MaxOpenLinksPerPage, MaxPagesPerOwner, MaxVersionsPerPage = 2, 2, 2
	b, n := blocksOf(t, qBlocks)
	p, _, err := s.Create(two, agentA, "One", b, n)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(two, agentA, "Two", b, n); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(two, agentB, "Three", b, n); !errors.Is(err, ErrTooMany) {
		t.Errorf("a third page past MaxPagesPerOwner: %v", err)
	}
	// Another owner's count is its own.
	if _, _, err := s.Create(asUser(3), agentB, "Theirs", b, n); err != nil {
		t.Errorf("user 3's first page refused by user 2's count: %v", err)
	}
	if _, _, err := s.AddVersion(two, agentA, p.ID, "One", b, n); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddVersion(two, agentA, p.ID, "One", b, n); !errors.Is(err, ErrTooMany) {
		t.Errorf("a third version past MaxVersionsPerPage: %v", err)
	}
	a, _ := s.CreateLink(two, p.ID, LinkPending, "x")
	if _, err := s.CreateLink(two, p.ID, LinkLive, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLink(two, p.ID, LinkLive, "x"); !errors.Is(err, ErrTooMany) {
		t.Errorf("a third open link past MaxOpenLinksPerPage: %v", err)
	}
	if _, err := s.RevokeLink(two, p.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLink(two, p.ID, LinkLive, "x"); err != nil {
		t.Errorf("a revoked link still counts toward the cap: %v", err)
	}
	if _, err := s.CreateLink(two, p.ID, LinkDenied, "x"); err == nil {
		t.Error("a link was created denied")
	}
}

// TestALinksTokenIsNotStored: after CreateLink hands its token back, no column
// of the link's row — read whole, as JSON, by the table's owner — holds the
// token, and the one it is found by is its sha256. The control: the same scan
// finds the hash, so a scan that sees nothing could not pass.
func TestALinksTokenIsNotStored(t *testing.T) {
	s, pool := pgStore(t)
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, err := s.Create(two, agentA, "Secret link", b, n)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []LinkState{LinkPending, LinkLive} {
		l, err := s.CreateLink(two, p.ID, state, "you")
		if err != nil || !TokenShapeOK(l.Token) {
			t.Fatalf("CreateLink(%s): %+v %v", state, l, err)
		}
		var row string
		if err := pool.QueryRow(two, `SELECT row_to_json(l)::text FROM page_links l WHERE id = $1`, l.ID).Scan(&row); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(row, l.Token) {
			t.Errorf("the %s link's row holds its token: %s", state, row)
		}
		if !strings.Contains(row, `"token_sha256":"`+TokenHash(l.Token)+`"`) {
			t.Errorf("control: the row does not hold the token's hash: %s", row)
		}
		// No read hands the token back.
		got, err := s.Link(two, p.ID, l.ID)
		if err != nil || got.Token != "" || got.TokenSHA256 != TokenHash(l.Token) {
			t.Errorf("Link read back %+v %v; want no token, its hash", got, err)
		}
	}
	var cols int
	if err := pool.QueryRow(two, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'page_links' AND column_name = 'token'`).Scan(&cols); err != nil || cols != 0 {
		t.Errorf("page_links has a token column (%d, %v)", cols, err)
	}
}
