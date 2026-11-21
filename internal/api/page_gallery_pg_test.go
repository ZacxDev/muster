package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
)

// TestAGallerysPicksAreAResponseLikeAnyOther (docs/pages.md#gallery): an
// agent publishes a gallery of its page's images through page_update; the
// content draws it; a link's viewer saves picks through the link's submit
// route; the owner saves theirs from their own view (by_owner); page_responses
// hands both over as {picks:[{index, asset, caption}]}, in the items' order;
// and a pick that is not the gallery's, or two of a single pick, is refused
// with the route's 400 and records nothing. Every name is invented.
func TestAGallerysPicksAreAResponseLikeAnyOther(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Logo review", `[{"type":"markdown","text":"Images coming."}]`)
	ctx := f.as(f.owner, authz.ViaAgent)
	p, err := f.pages.Page(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]string, 3)
	for i := range refs {
		a, err := f.pages.AddAsset(ctx, p.Creator, id, pages.Asset{PublicID: "gallerytestasset00000" + strconv.Itoa(i), Filename: "logo.png", ContentType: "image/png", Size: 10})
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = a.Ref()
	}
	blocks := `[{"type":"gallery","id":"logos","label":"Pick the logos","items":[` +
		`{"asset":"` + refs[0] + `","caption":"1a"},{"asset":"` + refs[1] + `","caption":"1b"},{"asset":"` + refs[2] + `"}],"pick":{"multiple":false}}]`
	up := f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(blocks)})
	if pnum(up["version"]) != 2 {
		t.Fatalf("page_update with a gallery: %v", up)
	}
	// A gallery naming an image of no page is refused by the tool, naming it.
	bad := strings.Replace(blocks, refs[2], "asset:nopageownsthisasset000", 1)
	if out := f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(bad)}); !strings.Contains(pageToolText(out), "asset:nopageownsthisasset000 is not an asset of this page") {
		t.Fatalf("page_update naming a foreign asset: %v", out)
	}

	l := f.liveLink(t, f.owner, id)
	w := f.get(t, pages.LinkPath(l.Token)+"/content", nil)
	assertContentHeaders(t, w, "the gallery's content")
	if b := pageContentBlocks(t, w.Body.String()); !strings.Contains(b, `aria-label="Open 1a full size"`) || !strings.Contains(b, `data-muster-pick="one"`) {
		t.Fatalf("the content does not draw the gallery: %s", b)
	}
	// Both shells name the shown version's pick gallery as the ONLY key they
	// keep picks under (a page's forged gallery sections keep nothing).
	for name, w := range map[string]*httptest.ResponseRecorder{
		"the link's shell":  f.get(t, pages.LinkPath(l.Token), nil),
		"the owner's shell": f.get(t, "/pages/"+strconv.FormatInt(id, 10)+"/view", f.ownerHdr()),
	} {
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `,KEEPS=["v2:logos"];`) {
			t.Errorf("%s does not keep exactly v2:logos: %d", name, w.Code)
		}
	}

	// Refused: another item's asset; two of a single pick. Nothing recorded.
	for _, data := range []string{
		`{"picks":[{"index":0,"asset":"` + refs[1] + `"}]}`,
		`{"picks":[{"index":0,"asset":"` + refs[0] + `"},{"index":1,"asset":"` + refs[1] + `"}]}`,
	} {
		if w := f.submit(t, l.Token, "logos", data); w.Code != http.StatusBadRequest {
			t.Errorf("a bad pick %s: %d %s", data, w.Code, w.Body.String())
		}
	}
	// A stranger's pick, then the owner's from their own view.
	if w := f.submit(t, l.Token, "logos", `{"picks":[{"index":1,"asset":"`+refs[1]+`","caption":"1b"}]}`); w.Code != http.StatusOK {
		t.Fatalf("a stranger's pick: %d %s", w.Code, w.Body.String())
	}
	if w := f.ownerSubmit(t, id, 2, "logos", `{"picks":[{"index":2,"asset":"`+refs[2]+`"}]}`, f.ownerHdr()); w.Code != http.StatusOK {
		t.Fatalf("the owner's pick: %d %s", w.Code, w.Body.String())
	}

	res := f.tool(t, f.agent, pageResponsesTool, map[string]any{"page_id": id})
	text, _ := res["responses"].(string)
	if pnum(res["count"]) != 2 {
		t.Fatalf("page_responses: %v", res)
	}
	// As Postgres keeps the JSON (jsonb: its own key order).
	for _, want := range []string{
		`"by_owner":false,"component":"logos","data":{"picks":[{"asset":"` + refs[1] + `","index":1,"caption":"1b"}]}`,
		`"by_owner":true,"component":"logos","data":{"picks":[{"asset":"` + refs[2] + `","index":2}]}`,
		`"kind":"submit","link_id":`, `"kind":"submit","version":2}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("page_responses lacks %q:\n%s", want, text)
		}
	}
}
