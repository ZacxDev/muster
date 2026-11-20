package api

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

// TestATeamPagesAssetsAreDrawnForEveryMemberWhoOpensIt: a team agent's page
// whose version shows an image (one of its author assets, 0164) is drawn
// with that image in every member's own view — the content document draws
// the <img>, and the asset route serves its bytes — because a team's page's
// assets follow the page (0165). A member of team B only and a member of no
// team get neither (404). CONTROL: the team itself, through the same store
// read the routes make, finds the asset.
func TestATeamPagesAssetsAreDrawnForEveryMemberWhoOpensIt(t *testing.T) {
	f := newTeamPageFixture(t)
	objects := &uploadstest.Objects{}
	sg, err := uploads.NewSigner("team-page-asset-signing-key-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.Uploads = &uploads.Service{Store: uploads.NewPG(f.pool), Objects: objects, Signer: sg, PublicURL: "https://muster.example.test",
		MaxBytes: 25 << 20, LinkTTL: time.Hour}
	id := f.teamPage(t, "Quarterly chart")
	sid := strconv.FormatInt(id, 10)
	team := f.asTeam(f.teamA)
	p, err := f.pages.Page(team, id)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := uploads.NewPublicID()
	if err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\n-team-chart-bytes")
	if err := objects.Put(context.Background(), uploads.PageObjectKeyFor(id, pid), bytes.NewReader(png), int64(len(png)), "image/png"); err != nil {
		t.Fatal(err)
	}
	a, err := f.pages.AddAsset(team, p.Creator, id, pages.Asset{PublicID: pid, Filename: "chart.png", ContentType: "image/png", Size: int64(len(png))})
	if err != nil {
		t.Fatal(err)
	}
	b, n, err := pages.Parse([]byte(`[{"type":"markdown","text":"![chart](` + a.Ref() + `)"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, v, err := f.pages.AddVersion(team, p.Creator, id, "Quarterly chart v3", b, n); err != nil || v.Version != 3 {
		t.Fatalf("the version showing the asset: %+v %v", v, err)
	}
	// CONTROL: the team reads the asset the way the routes do.
	if _, err := f.pages.VersionAsset(team, id, 3, pid); err != nil {
		t.Fatalf("control: the team reads its page's asset: %v", err)
	}
	img := `<img src="` + a.Ref() + `" alt="chart">`
	content, asset := "/pages/"+sid+"/versions/3/content", "/pages/"+sid+"/versions/3/assets/"+pid
	for who, uid := range map[string]int64{"creator": f.creator, "viewer": f.viewer, "member of both": f.both} {
		code, body := f.req(t, uid, http.MethodGet, content, "", nil)
		if code != http.StatusOK {
			t.Errorf("%s's view of the team's page: %d; want 200", who, code)
		} else if blocks := pageContentBlocks(t, body); !strings.Contains(blocks, img) {
			t.Errorf("%s's view of the team's page draws no image; want %s in:\n%s", who, img, blocks)
		}
		if code, body := f.req(t, uid, http.MethodGet, asset, "", nil); code != http.StatusOK || body != string(png) {
			t.Errorf("%s reading the team page's image: %d %q; want 200 and its bytes", who, code, body)
		}
	}
	for who, uid := range map[string]int64{"member of B only": f.onlyB, "member of no team": f.outsider} {
		if code, body := f.req(t, uid, http.MethodGet, content, "", nil); code != http.StatusNotFound || strings.Contains(body, a.PublicID) {
			t.Errorf("%s opening team A's page's content: %d, names the asset %v; want 404", who, code, strings.Contains(body, a.PublicID))
		}
		if code, _ := f.req(t, uid, http.MethodGet, asset, "", nil); code != http.StatusNotFound {
			t.Errorf("%s reading team A's page's image: %d; want 404", who, code)
		}
	}
}
