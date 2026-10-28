package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/suggest"
)

// SUGGESTED REPLIES TO AN ASK (attention_suggest.go), on a real Postgres,
// through the real attention_ask door, with a fake model.

const (
	sugQuestion = "Ship the migration tonight, or wait until Monday?"
	sugA        = "Wait for Monday, until the backup is verified"
	sugB        = "Ship it tonight, but only after a dry run"
)

// gatedGen is a fake model that answers two grounded suggestions for
// sugQuestion — once release is closed (nil: at once). It counts its calls.
type gatedGen struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
	prompts []string
}

func (g *gatedGen) Generate(ctx context.Context, _ int64, prompt string) (suggest.Generation, error) {
	g.mu.Lock()
	g.calls++
	g.prompts = append(g.prompts, prompt)
	g.mu.Unlock()
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			return suggest.Generation{}, ctx.Err()
		}
	}
	return suggest.Generation{Raw: []suggest.Raw{
		{Text: sugA, Kind: suggest.KindDecide, Cites: []string{"R"}, Quote: "wait until Monday"},
		{Text: sugB, Kind: suggest.KindDecide, Cites: []string{"R"}, Quote: "Ship the migration tonight"},
	}}, nil
}

func (g *gatedGen) n() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// sugFixture is an attention fixture whose operator has a browser AND a paired
// phone, and whose server has a suggestion engine over gen.
type sugFixture struct {
	*attnFixture
	raw         *rawPusher
	web, native string
	gen         *gatedGen
	engine      *suggest.Engine
}

func newSugFixture(t *testing.T, gen *gatedGen, tune func(*suggest.Engine)) *sugFixture {
	t.Helper()
	sf := &sugFixture{raw: &rawPusher{}, web: ep("sug-web"), native: ep("sug-native"), gen: gen}
	sf.engine = &suggest.Engine{Gen: gen}
	if tune != nil {
		tune(sf.engine)
	}
	sf.attnFixture = newAttnFixture(t, func(ext *Extensions, _ *pgxpool.Pool) {
		store := &memPush{}
		claim(t, store, 1, sf.web)
		store.nextID++
		var sub push.Subscription
		sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = sf.native, "k", "a"
		store.devs = append(store.devs, push.Device{ID: store.nextID, OwnerID: 1, Sub: sub, Kind: push.KindAndroid})
		ext.Push = push.NewService(store, sf.raw, push.VAPIDConfig{PublicKey: "k"}, nil)
		ext.Suggest = sf.engine
	})
	return sf
}

// sent is every payload endpoint ep received, decoded into a fresh T each.
func sentTo[T any](t *testing.T, p *rawPusher, ep string) []T {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []T
	for _, b := range p.got[ep] {
		var v T
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("%s: %v (%s)", ep, err, b)
		}
		out = append(out, v)
	}
	return out
}

func (sf *sugFixture) settle() {
	sf.srv.WaitForAskSuggestions()
	sf.srv.WaitForPushes()
}

// TestAnAskIsPushedAtOnceThenAgainWithItsSuggestions: the ask's first push
// goes out BEFORE the model answers (the fake is held), carrying the question
// and the options and no suggestion; once the model answers, the same tag is
// pushed again as an update, the phone's payload v2 carrying both
// suggestions as chips, the browser's the same lines; the set is stored on the
// ask and the ask page draws it. One model call. RED at origin/main: no
// second push, no ask chips, no suggestions column.
func TestAnAskIsPushedAtOnceThenAgainWithItsSuggestions(t *testing.T) {
	gen := &gatedGen{release: make(chan struct{})}
	sf := newSugFixture(t, gen, nil)
	id := sf.ask(t, sf.opTok, sugQuestion, "ship it", "wait for Monday")
	tag := attentionTag(attentionAskKey(id))
	sf.srv.WaitForPushes()
	first := sentTo[push.PayloadV2](t, sf.raw, sf.native)
	if len(first) != 1 || first[0].Tag != tag || first[0].Update || first[0].Ask == nil ||
		first[0].Ask.ID != id || strings.Join(first[0].Ask.Options, "|") != "ship it|wait for Monday" || len(first[0].Ask.Suggestions) != 0 {
		t.Fatalf("the first push (the model still held): %+v", first)
	}
	if first[0].Body != sugQuestion+"\nOptions: ship it · wait for Monday" || first[0].Title != sf.op.Name+" is asking" {
		t.Fatalf("the first push's lines: %q / %q", first[0].Title, first[0].Body)
	}
	close(gen.release)
	sf.settle()
	if gen.n() != 1 || !strings.Contains(gen.prompts[0], sugQuestion) || !strings.Contains(gen.prompts[0], "- wait for Monday") {
		t.Fatalf("model calls %d, prompts %q", gen.n(), gen.prompts)
	}
	all := sentTo[push.PayloadV2](t, sf.raw, sf.native)
	if len(all) != 2 {
		t.Fatalf("the phone got %d pushes, want 2: %+v", len(all), all)
	}
	again := all[1]
	if again.Tag != tag || !again.Update || again.Ask == nil || strings.Join(again.Ask.Suggestions, "|") != sugA+"|"+sugB ||
		strings.Join(again.Ask.Options, "|") != "ship it|wait for Monday" || again.Body != first[0].Body {
		t.Fatalf("the re-push: %+v (ask %+v)", again, again.Ask)
	}
	web := sentTo[map[string]any](t, sf.raw, sf.web)
	if len(web) != 2 || web[1]["tag"] != tag || web[1]["body"] != first[0].Body {
		t.Fatalf("the browser's pushes: %v", web)
	}
	for _, p := range web {
		if _, ok := p["ask"]; ok {
			t.Fatalf("the browser payload carries the native chips: %v", p)
		}
	}
	a, err := sf.attn.Get(asUser(1), id)
	if err != nil || strings.Join(a.Suggestions, "|") != sugA+"|"+sugB {
		t.Fatalf("stored suggestions %q (%v)", a.Suggestions, err)
	}
	page := sf.page(t, askPagePath(id))
	for _, want := range []string{`data-text="` + sugA + `"`, `data-text="` + sugB + `"`, `name="option" value="ship it"`,
		`form="ask-answer"`, `aria-label="AI-suggested replies"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the ask page lacks %q", want)
		}
	}
}

// TestASettledAskIsNeverRepushed: the ask is answered while the model is
// still working; when the model answers, nothing is stored and nothing is
// pushed — the phone's last word on that tag stays the clear. RED with the
// store's open-ask condition removed (see the mutation note in the PR).
func TestASettledAskIsNeverRepushed(t *testing.T) {
	gen := &gatedGen{release: make(chan struct{})}
	sf := newSugFixture(t, gen, nil)
	id := sf.ask(t, sf.opTok, sugQuestion, "ship it", "wait for Monday")
	sf.srv.WaitForPushes()
	if _, err := sf.srv.answerAsk(asUser(1), id, "ship it"); err != nil {
		t.Fatal(err)
	}
	sf.srv.WaitForPushes()
	close(gen.release)
	sf.settle()
	if gen.n() != 1 {
		t.Fatalf("CONTROL: the model was asked %d times, want 1 (the generation must have run)", gen.n())
	}
	var types []string
	for _, p := range sentTo[push.PayloadV2](t, sf.raw, sf.native) {
		types = append(types, p.Type)
	}
	if strings.Join(types, ",") != "attention,resolved" {
		t.Fatalf("the phone got %v, want the ask then its clear and nothing after", types)
	}
	if a, err := sf.attn.Get(asUser(1), id); err != nil || len(a.Suggestions) != 0 {
		t.Fatalf("a settled ask was given suggestions %q (%v)", a.Suggestions, err)
	}
}

// TestARepushThatLostTheRaceWithdrawsItself: the re-push of an ask that was
// settled after its suggestions were stored (the clear may overtake the
// re-push) is followed by the clear again. RED with repushAsk's re-read
// removed.
func TestARepushThatLostTheRaceWithdrawsItself(t *testing.T) {
	sf := newSugFixture(t, &gatedGen{}, func(e *suggest.Engine) { e.Off = true })
	id := sf.ask(t, sf.opTok, sugQuestion)
	sf.settle()
	a, err := sf.srv.answerAsk(asUser(1), id, "later")
	if err != nil {
		t.Fatal(err)
	}
	sf.srv.WaitForPushes()
	a.Suggestions = []string{sugA}
	sf.srv.repushAsk(a) // as if stored a moment before the answer
	sf.srv.WaitForPushes()
	var types []string
	for _, p := range sentTo[push.PayloadV2](t, sf.raw, sf.native) {
		types = append(types, p.Type)
	}
	if strings.Join(types, ",") != "attention,resolved,attention,resolved" {
		t.Fatalf("the phone got %v, want the late re-push withdrawn again", types)
	}
}

// TestNoSuggestionsWhenTheyAreOff: MUSTER_SUGGEST=off (Engine.Off) and an
// owner with no OpenRouter key each mean no model call, no stored set and
// one push; the ask page draws no chips. The control is the first test's
// fixture with neither, which calls once.
func TestNoSuggestionsWhenTheyAreOff(t *testing.T) {
	for name, tune := range map[string]func(*suggest.Engine){
		"MUSTER_SUGGEST=off": func(e *suggest.Engine) { e.Off = true },
		"no OpenRouter key":  func(e *suggest.Engine) { e.Gen = keylessAskGen{e.Gen} },
	} {
		t.Run(name, func(t *testing.T) {
			gen := &gatedGen{}
			sf := newSugFixture(t, gen, tune)
			id := sf.ask(t, sf.opTok, sugQuestion, "ship it")
			sf.settle()
			if gen.n() != 0 {
				t.Fatalf("the model was asked %d times", gen.n())
			}
			if n := len(sentTo[push.PayloadV2](t, sf.raw, sf.native)); n != 1 {
				t.Fatalf("the phone got %d pushes, want 1", n)
			}
			if page := sf.page(t, askPagePath(id)); strings.Contains(page, "data-ask-suggestion data-text") {
				t.Fatal("the ask page draws chips")
			}
		})
	}
}

// keylessAskGen is a generator whose owner has no OpenRouter key.
type keylessAskGen struct{ suggest.Generator }

func (keylessAskGen) CanGenerate(context.Context, int64) error { return suggest.ErrNoKey }

// TestSetSuggestionsMovesOnlyAnOpenAskOnce: the store's own rule. An open ask
// takes a cleaned set once (a blank, an over-long, a multi-line one and one
// that repeats an option are dropped; three kept); a second set is refused;
// a settled ask, another owner's and a missing one move nothing.
func TestSetSuggestionsMovesOnlyAnOpenAskOnce(t *testing.T) {
	f := newAttnFixture(t)
	id := f.ask(t, f.opTok, sugQuestion, "ship it")
	in := []string{" one ", "", strings.Repeat("x", attention.MaxSuggestionRunes+1), "two\nlines", "Ship It", "two", "three", "four"}
	a, ok, err := f.attn.SetSuggestions(asUser(1), id, in)
	if err != nil || !ok || strings.Join(a.Suggestions, "|") != "one|two|three" {
		t.Fatalf("an open ask: ok=%v %q (%v)", ok, a.Suggestions, err)
	}
	if _, ok, err := f.attn.SetSuggestions(asUser(1), id, []string{"again"}); err != nil || ok {
		t.Fatalf("a second set: ok=%v (%v)", ok, err)
	}
	other := f.ask(t, f.opTok, "Another?")
	if _, err := f.srv.cancelAsk(asUser(1), other); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.attn.SetSuggestions(asUser(1), other, []string{"x y"}); err != nil || ok {
		t.Fatalf("a settled ask: ok=%v (%v)", ok, err)
	}
	theirs := f.ask(t, f.workerTok, "A member's?")
	if _, ok, err := f.attn.SetSuggestions(asUser(1), theirs, []string{"x y"}); ok || err == nil {
		t.Fatalf("another owner's ask: ok=%v (%v)", ok, err)
	}
	if b, err := f.attn.Get(asUser(f.member), theirs); err != nil || len(b.Suggestions) != 0 {
		t.Fatalf("another owner's ask was written: %q (%v)", b.Suggestions, err)
	}
}

// TestTheAskPageIsTheAsksOwn: the page an ask's tap lands on — the question,
// its options as buttons of the pinned answer form, its context link, the
// form returning to the page; an answer given there comes back to it showing
// the answer; another owner's ask and a missing one are the queue's 404.
func TestTheAskPageIsTheAsksOwn(t *testing.T) {
	f := newAttnFixture(t)
	id := f.ask(t, f.opTok, "Deploy <the> importer?", "deploy now", "wait")
	sid := strconv.FormatInt(id, 10)
	page := f.page(t, askPagePath(id))
	for _, want := range []string{`data-ask-question>Deploy &lt;the&gt; importer?</p>`, `name="option" value="deploy now"`,
		`<form id="ask-answer" method="POST" action="/attention/asks/` + sid + `/answer"`,
		`name="return" value="/attention/asks/` + sid + `"`, `href="/agents/` + f.op.Name + `"`, `id="app-compose"`,
		`interactive-widget=resizes-content`, `aria-label="Back to Asks"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the ask page lacks %q:\n%s", want, page)
		}
	}
	body, hdr := form(map[string][]string{"option": {"wait"}, "answer": {""}, "return": {askPagePath(id)}})
	for k, v := range f.operator() {
		hdr[k] = v
	}
	w := f.call(t, http.MethodPost, "/attention/asks/"+sid+"/answer", body, hdr)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != askPagePath(id) {
		t.Fatalf("the answer: %d → %q", w.Code, w.Header().Get("Location"))
	}
	after := f.page(t, askPagePath(id))
	if !strings.Contains(after, `data-ask-closed data-status="answered"`) || !strings.Contains(after, "Your answer: wait") {
		t.Fatalf("the answered ask's page:\n%s", after)
	}
	// A refusal because the ask closed meanwhile comes back as
	// ?notice=ask-closed: a closed ask has no answer box (where notices show),
	// so the page says it in the content — and draws no Dismiss.
	closed := f.page(t, askPagePath(id)+"?notice="+noticeAskClosed)
	if !strings.Contains(closed, `data-ask-notice>Not recorded: that question is no longer open`) || strings.Contains(closed, "data-ask-dismiss-form>") {
		t.Fatalf("a closed ask's page with ?notice=ask-closed:\n%s", closed)
	}
	theirs := f.ask(t, f.workerTok, "A member's question?")
	for _, p := range []string{askPagePath(theirs), askPagePath(987654321)} {
		if w := f.call(t, http.MethodGet, p, nil, f.operator()); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "A member's question?") {
			t.Fatalf("GET %s: %d", p, w.Code)
		}
	}
}

// TestAStoredSetIsHiddenWhileSuggestionsAreOff: suggestions stored while they
// were on are not drawn once the switch is off (the page reads the same
// switches the generation does). The control is the same set with the engine
// on.
func TestAStoredSetIsHiddenWhileSuggestionsAreOff(t *testing.T) {
	sf := newSugFixture(t, &gatedGen{}, func(e *suggest.Engine) { e.Off = true })
	id := sf.ask(t, sf.opTok, sugQuestion)
	sf.settle()
	if _, ok, err := sf.attn.SetSuggestions(asUser(1), id, []string{sugA}); err != nil || !ok {
		t.Fatalf("store: ok=%v (%v)", ok, err)
	}
	if page := sf.page(t, askPagePath(id)); strings.Contains(page, `data-text="`+sugA+`"`) {
		t.Fatal("a stored set is drawn while suggestions are off")
	}
	sf.engine.Off = false
	if page := sf.page(t, askPagePath(id)); !strings.Contains(page, `data-text="`+sugA+`"`) {
		t.Fatal("CONTROL: the stored set is not drawn with suggestions on")
	}
}
