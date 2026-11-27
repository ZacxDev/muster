package pages

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// galRef is a gallery item's asset reference, distinct per n (assetID's ids,
// so the pg tests' assets match).
func galRef(n int) string { return AssetScheme + assetID(n) }

// galleryBlocks is a page with one picking gallery (three items, two groups,
// one item without a caption) and one gallery to look at only.
func galleryBlocks(pick string) string {
	return `[
 {"type":"gallery","id":"logo","label":"Pick the logos","items":[
   {"asset":"` + galRef(1) + `","caption":"1a","group":"Round 1"},
   {"asset":"` + galRef(2) + `","caption":"1b","group":"Round 1"},
   {"asset":"` + galRef(3) + `","group":"Round 2"}],"pick":` + pick + `,"layout":"grid"},
 {"type":"gallery","id":"look","items":[{"asset":"` + galRef(4) + `"}],"layout":"rows"}
]`
}

func mustGallery(t *testing.T, pick string) []Block {
	t.Helper()
	b, _, err := Parse([]byte(galleryBlocks(pick)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return b
}

func TestParseAcceptsAGallery(t *testing.T) {
	b := mustGallery(t, `{"multiple":false,"required":true,"submit_label":" Keep these "}`)
	g := b[0]
	if g.Type != BlockGallery || len(g.Items) != 3 || g.Items[0] != (GalleryItem{Asset: galRef(1), Caption: "1a", Group: "Round 1"}) {
		t.Fatalf("parsed %+v", g)
	}
	if g.Pick == nil || g.Pick.PicksSeveral() || !g.Pick.Required || g.Pick.SubmitLabel != "Keep these" {
		t.Errorf("pick %+v: want one, required, the label trimmed", g.Pick)
	}
	// multiple defaults to true; no pick is a gallery to look at only.
	if several := mustGallery(t, `{}`)[0].Pick; !several.PicksSeveral() {
		t.Error("a pick with no multiple does not pick several")
	}
	if b[1].Pick.PicksSeveral() {
		t.Error("a gallery without pick picks")
	}
	// It round-trips as stored: multiple:false is kept, not dropped.
	enc, _ := json.Marshal(g.Pick)
	if string(enc) != `{"multiple":false,"required":true,"submit_label":"Keep these"}` {
		t.Errorf("the pick is stored as %s", enc)
	}
}

func TestParseRefusesABadGallery(t *testing.T) {
	item := func(asset, rest string) string { return `{"asset":"` + asset + `"` + rest + `}` }
	many := make([]string, MaxGalleryItems+1)
	for i := range many {
		many[i] = item(galRef(1), "")
	}
	for _, c := range []struct{ name, blocks, want string }{
		{"no id", `[{"type":"gallery","items":[` + item(galRef(1), "") + `]}]`, "needs an id"},
		{"no items", `[{"type":"gallery","id":"g"}]`, "needs items"},
		{"too many items", `[{"type":"gallery","id":"g","items":[` + strings.Join(many, ",") + `]}]`, fmt.Sprintf("at most %d items", MaxGalleryItems)},
		{"a URL, not an asset", `[{"type":"gallery","id":"g","items":[` + item("https://example.com/a.png", "") + `]}]`, "not an asset reference"},
		{"a short id", `[{"type":"gallery","id":"g","items":[` + item("asset:abc", "") + `]}]`, "not an asset reference"},
		{"a long caption", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), `,"caption":"`+strings.Repeat("é", maxCaptionRunes+1)+`"`) + `]}]`, "caption is longer than 120"},
		{"a caption of two lines", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), `,"caption":"a\nb"`) + `]}]`, "caption holds a control"},
		{"a bidi caption", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), `,"caption":"a‮b"`) + `]}]`, "caption holds a control"},
		{"a long group", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), `,"group":"`+strings.Repeat("g", maxGroupRunes+1)+`"`) + `]}]`, "group is longer"},
		// The SDK swaps every asset:<id> of the page in the markup for its
		// placeholder, so a caption holding one could never be saved.
		{"a caption holding an asset ref", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), `,"caption":"same as `+galRef(2)+`"`) + `]}]`,
			"item 1: the caption holds an asset reference (" + galRef(2) + ")"},
		{"a caption holding its own ref", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), "") + `,` + item(galRef(2), `,"caption":"`+galRef(2)+`"`) + `]}]`,
			"item 2: the caption holds an asset reference"},
		{"a group holding an asset ref", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), `,"group":"see asset:x"`) + `]}]`, "item 1: the group holds an asset reference (asset:x)"},
		{"an unknown item field", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), `,"href":"x"`) + `]}]`, "href"},
		{"an unknown pick field", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), "") + `],"pick":{"max":2}}]`, "max"},
		{"a bad layout", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), "") + `],"layout":"masonry"}]`, "not grid or rows"},
		{"a long submit label", `[{"type":"gallery","id":"g","items":[` + item(galRef(1), "") + `],"pick":{"submit_label":"` + strings.Repeat("s", maxSubmitLabel+1) + `"}}]`, "pick.submit_label"},
		// The component fields a gallery reads from pick are refused at its top.
		{"required at the top", `[{"type":"gallery","id":"g","required":true,"items":[` + item(galRef(1), "") + `]}]`, "does not take required"},
		{"multiple at the top", `[{"type":"gallery","id":"g","multiple":true,"items":[` + item(galRef(1), "") + `]}]`, "does not take multiple"},
		{"submit_label at the top", `[{"type":"gallery","id":"g","submit_label":"Go","items":[` + item(galRef(1), "") + `]}]`, "does not take submit_label"},
		// And another block does not take a gallery's fields.
		{"items on a question", `[{"type":"question","id":"q","label":"x","items":[` + item(galRef(1), "") + `]}]`, "does not take items"},
		{"pick on a choice", `[{"type":"choice","id":"c","label":"x","options":["a"],"pick":{}}]`, "does not take pick"},
		{"layout on markdown", `[{"type":"markdown","text":"x","layout":"grid"}]`, "does not take layout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := Parse([]byte(c.blocks))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse = %v, want ErrInvalid containing %q", err, c.want)
			}
		})
	}
	// Control: the bound itself is accepted.
	ok := strings.Join(many[:MaxGalleryItems], ",")
	if _, _, err := Parse([]byte(`[{"type":"gallery","id":"g","items":[` + ok + `]}]`)); err != nil {
		t.Errorf("a gallery of exactly %d items: %v", MaxGalleryItems, err)
	}
}

// TestAGallerysAssetsAreItsVersionsReferences: a gallery's items are
// references the store checks (AssetRefs) and the shell serves.
func TestAGallerysAssetsAreItsVersionsReferences(t *testing.T) {
	b := mustGallery(t, `{}`)
	got := AssetRefs(b)
	want := []string{assetID(1), assetID(2), assetID(3), assetID(4)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("AssetRefs = %v, want %v", got, want)
	}
	if g := GalleryAssets(b); len(g) != 4 || !g[assetID(3)] {
		t.Errorf("GalleryAssets = %v", g)
	}
}

// TestCheckSubmitStoresAGallerysPicks pins the stored shape, literally: the
// picks in the items' order, each with the item's own asset and caption.
func TestCheckSubmitStoresAGallerysPicks(t *testing.T) {
	b := mustGallery(t, `{}`)
	for _, c := range []struct{ data, want string }{
		{`{"picks":[{"index":2,"asset":"` + galRef(3) + `"},{"index":0,"asset":"` + galRef(1) + `","caption":"1a"}]}`,
			`{"picks":[{"index":0,"asset":"` + galRef(1) + `","caption":"1a"},{"index":2,"asset":"` + galRef(3) + `"}]}`},
		// No caption sent: the item's own is stored.
		{`{"picks":[{"index":1,"asset":"` + galRef(2) + `"}]}`, `{"picks":[{"index":1,"asset":"` + galRef(2) + `","caption":"1b"}]}`},
		// Nothing picked (not required): an empty list, saved.
		{`{"picks":[]}`, `{"picks":[]}`},
		{`{}`, `{"picks":[]}`},
	} {
		got, err := CheckSubmit(b, "logo", json.RawMessage(c.data))
		if err != nil {
			t.Errorf("CheckSubmit(%s): %v", c.data, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("CheckSubmit(%s) = %s, want %s", c.data, got, c.want)
		}
	}
}

func TestCheckSubmitRefusesWhatAGalleryDoesNotSend(t *testing.T) {
	several := mustGallery(t, `{}`)
	one := mustGallery(t, `{"multiple":false,"required":true}`)
	p := func(i int, asset string) string {
		return `{"index":` + fmt.Sprint(i) + `,"asset":"` + asset + `"}`
	}
	for _, c := range []struct {
		name      string
		blocks    []Block
		component string
		data      string
		want      string
	}{
		{"a gallery without pick", several, "look", `{"picks":[]}`, "takes no picks"},
		{"not picks", several, "logo", `{"choices":["1a"]}`, "sends {picks"},
		{"no index", several, "logo", `{"picks":[{"asset":"` + galRef(1) + `"}]}`, "not one of the gallery's 3 items"},
		{"an index past the end", several, "logo", `{"picks":[` + p(3, galRef(4)) + `]}`, "not one of the gallery's 3 items"},
		{"a negative index", several, "logo", `{"picks":[` + p(-1, galRef(1)) + `]}`, "not one of"},
		{"another item's asset", several, "logo", `{"picks":[` + p(0, galRef(2)) + `]}`, "item 0's asset"},
		{"another caption", several, "logo", `{"picks":[{"index":0,"asset":"` + galRef(1) + `","caption":"1b"}]}`, "caption is not"},
		{"picked twice", several, "logo", `{"picks":[` + p(0, galRef(1)) + `,` + p(0, galRef(1)) + `]}`, "picked twice"},
		{"an extra key", several, "logo", `{"picks":[],"admin":true}`, "sends {picks"},
		{"an extra key in a pick", several, "logo", `{"picks":[{"index":0,"asset":"` + galRef(1) + `","url":"x"}]}`, "sends {picks"},
		{"two of a single pick", one, "logo", `{"picks":[` + p(0, galRef(1)) + `,` + p(1, galRef(2)) + `]}`, "pick one"},
		{"a required pick left empty", one, "logo", `{"picks":[]}`, "a pick is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := CheckSubmit(c.blocks, c.component, json.RawMessage(c.data))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("CheckSubmit = %v, want ErrInvalid containing %q", err, c.want)
			}
		})
	}
	// Control: one pick of the single gallery is taken.
	if _, err := CheckSubmit(one, "logo", json.RawMessage(`{"picks":[`+p(2, galRef(3))+`]}`)); err != nil {
		t.Errorf("control: one pick of a single gallery: %v", err)
	}
}

func TestAGallerysPicksAreDescribedByCaption(t *testing.T) {
	b := mustGallery(t, `{}`)
	data, err := CheckSubmit(b, "logo", json.RawMessage(`{"picks":[`+`{"index":0,"asset":"`+galRef(1)+`"},{"index":2,"asset":"`+galRef(3)+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	a := DescribeAnswer(b, "logo", data)
	if a.Heading != "Pick the logos" || len(a.Lines) != 1 || a.Lines[0] != (AnswerLine{Label: "Picked", Value: "1a, image 3"}) {
		t.Errorf("described %+v", a)
	}
	none := DescribeAnswer(b, "logo", []byte(`{"picks":[]}`))
	if none.Lines[0].Value != "Nothing picked" {
		t.Errorf("an empty save is described %+v", none)
	}
}

// TestAGalleryIsDrawnEscapedWithNamedButtons: every caption, group and label
// is escaped; each thumbnail is a button named "Open <name> full size" and
// each Pick a toggle named "Pick <name>"; a heading is drawn where the group
// changes; the bar is drawn only with pick.
func TestAGalleryIsDrawnEscapedWithNamedButtons(t *testing.T) {
	evil := `<img src=x onerror=alert(1)>`
	v := Version{Title: "x", Version: 4, Blocks: []Block{
		{Type: BlockGallery, ID: "g1", Label: "L " + evil, Pick: &GalleryPick{SubmitLabel: "S " + evil}, Items: []GalleryItem{
			{Asset: galRef(1), Caption: `1a "` + evil, Group: "G " + evil},
			{Asset: galRef(2), Group: "G " + evil},
			{Asset: galRef(3), Caption: "2a", Group: "Two"},
		}},
		{Type: BlockGallery, ID: "g2", Layout: GalleryRows, Items: []GalleryItem{{Asset: galRef(4), Caption: "solo"}}},
	}}
	out := Content(v, RenderOptions{})
	blocks := contentBlocks(t, out)
	if n := strings.Count(out, evil) + strings.Count(blocks, evil); n != 0 {
		t.Errorf("an agent's markup came through unescaped %d time(s)", n)
	}
	esc := `&lt;img src=x onerror=alert(1)&gt;`
	for _, want := range []string{
		`<section class="mp-block mp-gallery" data-muster-component="g1" data-muster-kind="gallery" data-muster-layout="grid" data-muster-items="3" data-muster-pick="several" aria-labelledby="g1-l">`,
		`<p class="mp-label" id="g1-l">L ` + esc + `</p>`,
		`<h3 class="mp-gallery-group">G ` + esc + `</h3><ul class="mp-gallery-grid" role="list">`,
		`<li class="mp-gallery-item" data-muster-item="0" data-muster-asset-id="` + assetID(1) + `" data-muster-caption="1a &#34;` + esc + `" data-muster-name="1a &#34;` + esc + `">`,
		`<button type="button" class="mp-gallery-open" aria-label="Open 1a &#34;` + esc + ` full size"><img src="` + galRef(1) + `" alt="1a &#34;` + esc + `">`,
		`<button type="button" class="mp-gallery-pick mp-secondary" aria-pressed="false" aria-label="Pick 1a &#34;` + esc + `">Pick</button>`,
		// No caption: named by its place, its alt text says where it is.
		`aria-label="Open image 2 full size"><img src="` + galRef(2) + `" alt="Image 2 of 3">`,
		`aria-label="Pick image 2"`,
		// The group changed: a second list under its heading.
		`</ul><h3 class="mp-gallery-group">Two</h3><ul class="mp-gallery-grid" role="list"><li class="mp-gallery-item" data-muster-item="2"`,
		`<button type="button" data-muster-save aria-keyshortcuts="Control+Enter Meta+Enter">S ` + esc + `</button>` + keysHint,
		`<section class="mp-block mp-gallery" data-muster-component="g2" data-muster-kind="gallery" data-muster-layout="rows" data-muster-items="1" aria-label="Gallery">`,
	} {
		if !strings.Contains(blocks, want) {
			t.Errorf("the gallery lacks %q\n%s", want, blocks)
		}
	}
	// One heading per run of a group: "G" once, though two items carry it.
	if n := strings.Count(blocks, `<h3 class="mp-gallery-group">`); n != 2 {
		t.Errorf("%d group headings, want 2", n)
	}
	// Only the picking gallery has Pick buttons and a bar.
	if n := strings.Count(blocks, `class="mp-gallery-pick`); n != 3 {
		t.Errorf("%d Pick buttons, want 3 (the looking gallery has none)", n)
	}
	if n := strings.Count(blocks, `data-muster-save`); n != 1 {
		t.Errorf("%d save bars, want 1", n)
	}
	// The single pick and required are said on the section.
	one := contentBlocks(t, Content(Version{Title: "x", Version: 1, Blocks: []Block{{Type: BlockGallery, ID: "g", Pick: &GalleryPick{Multiple: new(bool), Required: true},
		Items: []GalleryItem{{Asset: galRef(1)}}}}}, RenderOptions{}))
	if !strings.Contains(one, `data-muster-pick="one" data-muster-required aria-label="Gallery">`) {
		t.Errorf("a single required pick is not marked: %s", one)
	}
}

// TestAGallerysScriptIsTheSDKsOwn: a version with a gallery carries the
// gallery's script inside the SDK (the content document still has ONE
// script, and the frame's sandbox and headers are unchanged); a version
// without one carries none of it. The script keeps the SDK's rules.
func TestAGallerysScriptIsTheSDKsOwn(t *testing.T) {
	gv := Version{Title: "x", Version: 7, Blocks: mustGallery(t, `{}`)}
	out := Content(gv, RenderOptions{})
	if n := strings.Count(out, "<script"); n != 1 {
		t.Errorf("the document has %d <script> elements, want only the SDK", n)
	}
	sdk := sdkScript(gv, "", nil)
	for _, want := range []string{
		`var VER=7,`,
		"function galOne(sec){",
		// Save sends once: a tap while one is on its way does nothing.
		`function save(){
  if(busy||!mode)return;`,
		`send("submit",id,{picks:data},function(ok,x){`,
		// The picks are kept in the shell, keyed by version and gallery.
		`key="v"+VER+":"+id`, `send("keep",key,{p:picked.slice(),s:saved?saved.slice():null},function(){});`, `send("recall",key,null,`,
		// The viewer tells the shell, which owns the history entry.
		`onBack=function(){shut(true);};viewer(true);`, `if(!byBack){onBack=null;viewer(false);}`,
	} {
		if !strings.Contains(sdk, want) {
			t.Errorf("the gallery's script lacks %q", want)
		}
	}
	if !strings.Contains(out, galleryCSS) {
		t.Error("a gallery's document lacks its styles")
	}
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "sendBeacon", "WebSocket", "document.cookie", "localStorage", "sessionStorage",
		"history.", "new Promise", ".then(", ".map(", "userActivation"} {
		if strings.Contains(sdk, banned) {
			t.Errorf("the SDK with a gallery uses %s", banned)
		}
	}
	if strings.Count(sdk, "PARENT.postMessage(") != 1 {
		t.Error("the SDK with a gallery posts to its parent something other than hello")
	}
	// Still exactly the SDK's own ways onto the port (send, scroll, viewer),
	// and show() and load() from their own places only.
	if strings.Count(sdk, "R(PM,port,") != 3 || strings.Count(sdk, "show();") != 2 || strings.Count(sdk, "load();") != 1 {
		t.Errorf("the gallery posts on its own, or calls show/load: %d posts, %d show, %d load",
			strings.Count(sdk, "R(PM,port,"), strings.Count(sdk, "show();"), strings.Count(sdk, "load();"))
	}
	// Without a gallery: no gallery script, no gallery styles.
	plain := Version{Title: "x", Version: 1, Blocks: []Block{{Type: BlockMarkdown, Text: "hi"}}}
	if s := sdkScript(plain, "", nil); strings.Contains(s, "galOne") || !strings.Contains(s, "function gal(){}") {
		t.Error("a version without a gallery carries the gallery's script")
	}
	if strings.Contains(Content(plain, RenderOptions{}), ".mp-viewer{") {
		t.Error("a version without a gallery carries the gallery's styles")
	}
	// The frame and its policy are the sandbox's, gallery or not.
	if ContentCSP != "sandbox allow-scripts allow-forms; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
		"img-src data: blob:; media-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'" {
		t.Errorf("the content policy changed: %s", ContentCSP)
	}
	shell := Shell(ShellView{Title: "x", FrameSrc: "/s/T/content", Base: "/s/T", Nonce: "N1"})
	if !strings.Contains(shell, `sandbox="allow-scripts allow-forms" referrerpolicy="no-referrer"></iframe>`) || !strings.Contains(shell, `<script nonce="N1">`) ||
		strings.Count(shell, "<script") != 1 {
		t.Error("the shell's frame sandbox or its one nonce'd script changed")
	}
}

// TestTheShellOwnsTheViewersHistoryEntry pins the shell's half of the
// viewer's Back (gallery.go): "viewer" is read only from the port; an open
// pushes ONE marked entry, a close pops it, an open during that pop waits for
// it — by its popstate, never a timer; a popstate answers "back" on the port
// only when the current entry is no longer the one this load pushed; a marked
// entry current while nothing is open (a reload, a Forward) is taken over by
// the next open. And the picks it keeps for the frame: only a key the server
// named (KEEPS), under this page's path, in this window's sessionStorage,
// bounded per key and per path.
func TestTheShellOwnsTheViewersHistoryEntry(t *testing.T) {
	js := ShellScript("/s/TOKEN", false, false, "v3:logo", "v3:pick2")
	for _, want := range []string{
		`if(m.op==="scroll")onY(m.y);else if(m.op==="viewer")viewer(m.open===true);else relay(m);`,
		`function vMine(){var s=vState();return !!s&&s.musterViewer===1&&s.t===vTok&&vTok!=="";}`,
		`if(vOpen)return;if(vClosing){vWant=true;return;}
  vTok=vId+":"+(++vN);var st={musterViewer:1,t:vTok};
  try{if(vMarked())history.replaceState(st,"");else history.pushState(st,"");vOpen=true;}catch(e){vTok="";}`,
		`vWant=false;if(!vOpen)return;vOpen=false;
 if(!vMine()){vTok="";return;}
 vClosing=true;history.back();
}`,
		`if(vClosing){vClosing=false;vTok="";if(vWant){vWant=false;viewer(true);}return;}
 if(vOpen&&!vMine()){vOpen=false;vTok="";if(port)port.postMessage({muster:1,op:"back"});}`,
		`,port=null,KEEPS=["v3:logo","v3:pick2"];`,
		`typeof d.key!=="string"||KEEPS.indexOf(d.key)<0)
   return reply(false,"muster does not take that");`,
		`var pre="muster-picks:"+location.pathname+"|",sk=pre+d.key;`,
		`if(kv.length>16384)return reply(false,"too much to keep");`,
		`if(KEEPS.indexOf(kk.slice(pre.length))<0)sessionStorage.removeItem(kk);else held++;`,
		`if(held>=16)return reply(false,"too much to keep");`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the shell lacks %q", want)
		}
	}
	// No keeps named: none is taken (KEEPS is empty, never null).
	if !strings.Contains(ShellScript("/s/TOKEN", false, false), `,KEEPS=[];`) {
		t.Error("a shell with no keeps does not refuse every key")
	}
	// The viewer's entry is decided by history.state alone: the old timer
	// fallback (a pop presumed done after a second) is gone.
	if i, j := strings.Index(js, "var vOpen="), strings.Index(js, "function relay("); i < 0 || j < i {
		t.Error("the shell's viewer entry is not where it was")
	} else if strings.Contains(js[i:j], "setTimeout") {
		t.Error("the shell still presumes a pop done on a timer")
	}
	// One push, one replace (the take-over), one place a viewer is read, one "back".
	for s, n := range map[string]int{"history.pushState(": 1, "history.replaceState(": 1, `"viewer"`: 1, `op:"back"`: 1, "viewer(": 3, "history.back()": 1} {
		if got := strings.Count(js, s); got != n {
			t.Errorf("the shell has %d %q, want %d", got, s, n)
		}
	}
	// The keep and recall are answered before any send's checks, and send
	// nothing: no fetch was added.
	if strings.Count(js, "fetch(") != 2 {
		t.Errorf("the shell fetches in %d places, want 2", strings.Count(js, "fetch("))
	}
	// The SDK's half: "back" with no id calls what is open, once.
	sdk := sdkScript(Version{Title: "x", Version: 1}, "", nil)
	if !strings.Contains(sdk, `if(d.op==="back"&&d.id===undefined){var b=onBack;onBack=null;if(b)b();return;}`) ||
		!strings.Contains(sdk, `function viewer(open){if(port)R(PM,port,[{muster:1,op:"viewer",open:open===true}]);}`) {
		t.Error("the SDK's half of the viewer's Back is not as pinned")
	}
}

// TestTheShellKeepsOnlyTheShownVersionsPickGalleries: KeepKeys names each
// gallery WITH a pick of the version, as galleryJS builds its key, and the
// shell embeds exactly that list; a gallery to look at only keeps nothing.
func TestTheShellKeepsOnlyTheShownVersionsPickGalleries(t *testing.T) {
	v := Version{Title: "x", Version: 7, Blocks: append(mustGallery(t, `{}`), Block{Type: BlockMarkdown, Text: "hi"},
		Block{Type: BlockGallery, ID: "second", Items: []GalleryItem{{Asset: galRef(5)}}, Pick: &GalleryPick{}})}
	got := KeepKeys(v)
	if strings.Join(got, ",") != "v7:logo,v7:second" {
		t.Errorf("KeepKeys = %q, want v7:logo,v7:second", got)
	}
	if !strings.Contains(sdkScript(v, "", nil), `var VER=7,`) || !strings.Contains(galleryJS(v), `key="v"+VER+":"+id`) {
		t.Error("the SDK's keep key is no longer v<version>:<id>")
	}
	shell := Shell(ShellView{Title: "x", FrameSrc: "/s/T/content", Base: "/s/T", Nonce: "N", Keeps: got})
	if !strings.Contains(shell, `,KEEPS=["v7:logo","v7:second"];`) {
		t.Error("the shell does not embed the version's keep keys")
	}
	if k := KeepKeys(Version{Version: 1, Blocks: []Block{{Type: BlockMarkdown, Text: "hi"}}}); k == nil || len(k) != 0 {
		t.Errorf("a version without a pick gallery keeps %q", k)
	}
}

// TestTheViewerEnlargesASmallImage pins the viewer image's box: it fills the
// slide and is letterboxed (object-fit:contain), so a small image is drawn
// up to the viewer's width or height; max-width/max-height alone could only
// shrink it. The e2e test measures the drawn size.
func TestTheViewerEnlargesASmallImage(t *testing.T) {
	if !strings.Contains(galleryCSS, `.mp-viewer-slide img{width:100%;height:100%;object-fit:contain;`) {
		t.Error("the viewer's image does not fill its slide")
	}
	if strings.Contains(galleryCSS, `.mp-viewer-slide img{max-width`) {
		t.Error("the viewer's image is still only shrunk to fit")
	}
}
