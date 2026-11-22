package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// The asks audit's fixes, pinned by their literal words and markup.

// askPageBody renders ask a's page, at path (its own, or with a ?notice=).
func askPageBody(t *testing.T, s *Server, a attention.Ask, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.renderAskPage(rec, httptest.NewRequest("GET", path, nil).WithContext(s.attentionPrincipal(context.Background(), 1)), a)
	if rec.Code != http.StatusOK {
		t.Fatalf("the ask page: %d", rec.Code)
	}
	return rec.Body.String()
}

// askPageH1 is the page's top bar heading.
func askPageH1(body string) string {
	const open = `<h1 class="min-w-0 flex-1 truncate text-base font-semibold text-fg">`
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	rest := body[i+len(open):]
	return rest[:strings.Index(rest, "</h1>")]
}

// TestAStepUpCardsPageOffersNoBrowserAnswer: the browser can never answer a
// step-up card (errHostDecisionNeedsPhone), so its page draws no options, no
// suggestions, no answer box and no mic — one line says where it is answered —
// and it is headed by the card's own action, never by its internal asker
// label ("step-up is asking"). Dismiss stays: it withdraws the card, and no
// decision runs from it. RED before the asks audit: Confirm and Refuse, the
// box and the mic were drawn, under "step-up is asking".
func TestAStepUpCardsPageOffersNoBrowserAnswer(t *testing.T) {
	a := stepUpFixtureAsk(t, "Big news.")
	a.Owner, a.CreatedAt = 1, time.Now()
	s := whereServer(t, a)
	body := askPageBody(t, s, a, askPagePath(a.ID))
	if got := askPageH1(body); got != "Post to X? · Draft #41" {
		t.Errorf("the heading %q, want %q", got, "Post to X? · Draft #41")
	}
	for _, want := range []string{
		`<p class="break-words text-base text-fg2" data-ask-stepup-phone>Confirm this on your phone.</p>`,
		`data-ask-dismiss>Dismiss without answering</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the step-up page lacks %s:\n%s", want, body)
		}
	}
	for _, never := range []string{`data-ask-option`, `id="ask-answer"`, `id="ask-input"`, `id="ask-send"`, `ask-dictate`, `<button type="button" data-ask-suggestion`,
		"step-up is asking", "step-up asks", `data-ask-source`, ">Confirm</button>", ">Refuse</button>"} {
		if strings.Contains(body, never) {
			t.Errorf("the step-up page still draws %q", never)
		}
	}
	// Settled, it is headed by its state.
	a.Status = attention.Answered
	if got := askPageH1(askPageBody(t, s, a, askPagePath(a.ID))); got != "Answered" {
		t.Errorf("an answered step-up card's heading %q, want Answered", got)
	}
}

// TestAStepUpCardsRefusalsSayWhatToDo: an answer the server refuses because
// only the paired phone may give it, or because a restart lost the request,
// returns to the ask page with its OWN notice — never the generic "an answer
// is non-blank text…" (ErrInvalid, which both wrap) — and the step-up page,
// which has no answer box to show a notice in, shows it on the page.
// RED before the asks audit: both landed on ?notice=ask-invalid.
func TestAStepUpCardsRefusalsSayWhatToDo(t *testing.T) {
	a := stepUpFixtureAsk(t, "Big news.")
	a.Owner, a.CreatedAt = 1, time.Now()
	s := whereServer(t, a)
	for err, want := range map[error][2]string{
		errHostDecisionNeedsPhone: {"ask-needs-phone", "Confirm this on your paired phone."},
		errHostDecisionLost:       {"ask-lost", "This request was lost when muster restarted; the session must ask again."},
	} {
		form := url.Values{"return": {askPagePath(a.ID)}}
		r := httptest.NewRequest("POST", askPagePath(a.ID)+"/answer", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.attentionFormFailed(rec, r, err, a)
		if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != askPagePath(a.ID)+"?notice="+want[0] {
			t.Errorf("%v: %d %q, want 303 to ?notice=%s", err, rec.Code, loc, want[0])
		}
		body := askPageBody(t, s, a, askPagePath(a.ID)+"?notice="+want[0])
		if !strings.Contains(body, `data-ask-notice>`+want[1]+`</p>`) {
			t.Errorf("%v: the step-up page does not say %q:\n%s", err, want[1], body)
		}
	}
	if got := composerNotices[noticeAskInvalid].text; got != "Type an answer or pick an option." {
		t.Errorf("the blank-answer notice %q", got)
	}
}

// TestAStepUpCardIsMustersOwnOnTheFeed: the Asks feed's card for a step-up
// card names muster as who asks — the head and data-asks-who (the "Sent to"
// toast) — never the internal asker label "step-up". RED before the asks
// audit: its head read "step-up".
func TestAStepUpCardIsMustersOwnOnTheFeed(t *testing.T) {
	a := stepUpFixtureAsk(t, "Big news.")
	a.Owner, a.CreatedAt = 1, time.Now()
	it := askItem(a)
	if title, src, isSrc := attentionTitle(it); title != "muster" || src != "" || !isSrc {
		t.Errorf("attentionTitle %q, %q, %v; want muster, \"\", true", title, src, isSrc)
	}
	card := askFeedCardHTML(it, time.Now())
	if !strings.Contains(card, `data-asks-who="muster"`) || strings.Contains(card, ">step-up<") || strings.Contains(card, `"step-up"`) {
		t.Errorf("the feed card names its asker wrong:\n%s", card)
	}
}

// TestTheAskPageAnswerIsDisabledWhileBlank: Answer is drawn aria-disabled (the
// box starts empty; not the disabled attribute, which disabled the mic's ↑ —
// ui.AsksSendDisabledClass), the page's script turns it on while the box holds
// text — or, on a MULTI-SELECT ask, while a box is TICKED (migration 0175) —
// and a blank answer is not posted;
// beside options it is quiet and Dismiss is a text link, without them the
// filled primary and the full-width quiet Dismiss. The badge row no longer
// repeats who asks (the heading says it). RED before the asks audit: Answer
// was never disabled, and a blank answer round-tripped to the server.
func TestTheAskPageAnswerIsDisabledWhileBlank(t *testing.T) {
	now := time.Now()
	opts := attention.Ask{ID: 31, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Squash or merge?",
		Options: []string{"Squash", "Merge"}, Status: attention.Open, CreatedAt: now}
	free := opts
	free.ID, free.Options = 32, nil
	s := whereServer(t, opts, free)
	body := askPageBody(t, s, opts, askPagePath(opts.ID))
	for _, want := range []string{
		`<button type="submit" id="ask-send" class="press min-h-[44px] shrink-0 rounded-lg px-4 text-sm font-semibold text-fg ring-1 ring-inset ring-edge transition hover:bg-s2 aria-disabled:cursor-not-allowed aria-disabled:opacity-50" aria-disabled="true" data-ask-send>Answer</button>`,
		`<button type="submit" class="inline-flex min-h-[44px] items-center self-start rounded-lg px-1 text-sm font-semibold text-fg2 underline transition hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus" data-ask-dismiss>Dismiss without answering</button>`,
		`function gate(b) { var s = document.getElementById('ask-send'); if (!s) return; if (b.value.trim() || gatePick(b.form)) s.removeAttribute('aria-disabled'); else s.setAttribute('aria-disabled', 'true'); }`,
		`if (e.submitter && e.submitter.id === 'ask-send' && !b.value.trim() && !gatePick(f)) { e.preventDefault(); b.focus(); return; }`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the options page lacks %s:\n%s", want, body)
		}
	}
	// gatePick reads the form's CONTROLS (f.elements), not its DOM
	// descendants: a multi-select ask's checkboxes are form-associated with
	// the answer form but are drawn ABOVE it, so a querySelector subtree walk
	// never finds them. (Asserted behaviourally by e2e/tests/ask-page.spec.ts;
	// pinned here so a regression to querySelector is caught without a
	// browser.)
	for _, want := range []string{
		`function gatePick(f) {`,
		`if (!f || !f.elements) return null;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the options page lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `f.querySelector('input[name="option"]:checked')`) {
		t.Errorf("gatePick walks the form's DOM descendants, so it cannot see the form-associated multi-select boxes:\n%s", body)
	}
	if got := askPageH1(body); got != "Worker Seven is asking" {
		t.Errorf("the heading %q", got)
	}
	if strings.Contains(body, "data-ask-source") || strings.Contains(body, "Worker Seven</span> asks") {
		t.Errorf("the badge row still names the asker:\n%s", body)
	}
	body = askPageBody(t, s, free, askPagePath(free.ID))
	for _, want := range []string{
		`<button type="submit" id="ask-send" class="press min-h-[44px] shrink-0 rounded-full bg-accent px-5 text-base font-semibold text-on-accent transition hover:bg-accent/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50" aria-disabled="true" data-ask-send>Answer</button>`,
		`<button type="submit" class="` + flowsQuietButton + ` w-full" data-ask-dismiss>Dismiss without answering</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the free-text page lacks %s:\n%s", want, body)
		}
	}
	// Settled: headed by its state, never "is asking".
	for st, want := range map[attention.Status]string{attention.Answered: "Answered", attention.Cancelled: "Dismissed", attention.Expired: "Expired"} {
		free.Status = st
		if got := askPageH1(askPageBody(t, s, free, askPagePath(free.ID))); got != want {
			t.Errorf("%s: heading %q, want %q", st, got, want)
		}
	}
}

// TestTheFeedsSuggestionsCarryTheAIPill: the feed card's suggested replies
// are marked as an AI's, as the ask page's are, and their ring is ring-edge
// (3:1 in both themes; accent/40 was under it).
func TestTheFeedsSuggestionsCarryTheAIPill(t *testing.T) {
	it := attentionItem{Key: "ask-9", Kind: attentionKindAsk, AskID: 9, Text: "ship?", suggestions: []string{"Ship it"}}
	form := askAnswerFormHTML(it, "9", askFormPlace{chips: true, gated: true})
	if !strings.Contains(form, `data-asks-suggestions><span class="self-center rounded bg-accent/15 px-1.5 py-0.5 text-xs font-semibold text-fg" data-asks-ai-pill>AI</span><button type="button" class="`+ui.AsksCardChipClass+`"`) {
		t.Errorf("the feed's chips carry no AI pill:\n%s", form)
	}
	if !strings.Contains(ui.AsksCardChipClass, " ring-edge ") || strings.Contains(ui.AsksCardChipClass, "accent/40") {
		t.Errorf("the chip ring: %q", ui.AsksCardChipClass)
	}
}

// TestTheSmallerAsksFixes pins the audit's copy and markup fixes that need no
// server: the step-up Details' chevron, an outbound draft's medium named by
// its file alone (its type, size and SHA-256 behind Details), the Log tab's
// loading line, and the queued delivery's sentence.
func TestTheSmallerAsksFixes(t *testing.T) {
	a := stepUpFixtureAsk(t, "Big news.")
	if page := askQuestionHTML(a); !strings.Contains(page, `data-stepup-details-toggle>Details`+ui.AsksCardChevron+`</summary>`) {
		t.Errorf("the step-up Details has no chevron:\n%s", page)
	}
	plugin, venue, kind := int64(3), "x", plugins.TargetPost
	d := plugins.Draft{ID: 14, PluginID: &plugin, Venue: &venue, TargetKind: &kind, Body: "Hi", Status: plugins.DraftPending,
		MediaRefs: []string{"launch/hero-1.webp"}, Media: []plugins.DraftMedia{{Ref: "launch/hero-1.webp", ContentType: "image/webp", Size: 2048, SHA256: strings.Repeat("b", 64)}}}
	card := draftCardHTMLAt(plugins.DraftCard{Draft: d}, designNow)
	// The medium's file name, type, size and SHA-256 are in the card's ONE
	// Details fold (stepUpDetailsHTML), never on its face.
	at := strings.Index(card, `<details class="`+ui.StepUpDetailsClass+`" data-stepup-details>`)
	if at < 0 {
		t.Fatalf("the outbound card has no Details fold:\n%s", card)
	}
	details := card[at:]
	for _, want := range []string{
		`<span data-draft-medium-name>launch/hero-1.webp</span>`,
		`data-draft-medium-facts>image/webp · 2 KB · sha256 ` + strings.Repeat("b", 64) + `</span></li>`,
	} {
		if !strings.Contains(details, want) {
			t.Errorf("the outbound card's Details lack %s:\n%s", want, card)
		}
	}
	for _, want := range []string{
		`data-stepup-details-toggle>Details` + ui.AsksCardChevron + `</summary>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the outbound card lacks %s:\n%s", want, card)
		}
	}
	if lazy := askLogLazyHTML(url.Values{}); !strings.Contains(lazy, `data-asklog-loading hidden>Loading…</p>`) || !strings.Contains(lazy, `<noscript>`) {
		t.Errorf("the Log tab's loading line:\n%s", lazy)
	}
	queued := attention.Ask{Delivery: attention.DeliveryQueued, DeliveryDetail: "the agent is in the middle of a turn"}
	if got, _ := answerDeliveryText(queued, "Worker Seven", false); got != "Will reach Worker Seven when it's back." {
		t.Errorf("the queued delivery %q", got)
	}
	if lazy := askLogLazyHTML(url.Values{}); !strings.Contains(lazy, `<p class="px-1 text-sm text-fg2" role="status" data-asklog-loading hidden>Loading…</p>`) {
		t.Errorf("the Log tab's loading line is not drawn hidden (a page with no script would say Loading… forever):\n%s", lazy)
	}
}

// TestAQueuedAnswerSaysWhyWhenItWillNotClearOnItsOwn: a delivery waiting on
// something muster retries (busy, mid-turn, being replaced, the runtime took
// nothing, an agent still starting) says it will arrive; one waiting on what
// never clears without the owner — the agent stopped, no agent gateway — says
// why and what to do. The details are the very strings deliverAnswer stores
// (its constants and the literal ones its other leaves write). RED before:
// every queued detail read "Will reach … when it's back."
func TestAQueuedAnswerSaysWhyWhenItWillNotClearOnItsOwn(t *testing.T) {
	const who = "Worker Seven"
	for detail, want := range map[string]string{
		"the agent is in the middle of a turn":           "Will reach Worker Seven when it's back.",
		"the agent is busy with another delivery":        "Will reach Worker Seven when it's back.",
		"the agent is being replaced":                    "Will reach Worker Seven when it's back.",
		"the agent's runtime took nothing (busy)":        "Will reach Worker Seven when it's back.",
		"the agent has not had its first turn yet":       "Will reach Worker Seven when it's back.",
		"the agent is not running (provisioning)":        "Will reach Worker Seven when it's back.",
		"the agent is not running (pending)":             "Will reach Worker Seven when it's back.",
		"the agent is not running (stopped)":             "Waiting: Worker Seven is stopped. Start it again and muster delivers the answer.",
		"the agent is not running (error)":               "Waiting: Worker Seven stopped with an error. Restart it and muster delivers the answer.",
		"this muster has no agent gateway":               "Not delivered: this server has no agent gateway, so it cannot send answers to Worker Seven. Whoever runs this server must set one up; until then, give the answer in Worker Seven's session.",
		"the agent is not running (stopped) and more":    "Will reach Worker Seven when it's back.",
		"this muster has no agent gateway, or something": "Will reach Worker Seven when it's back.",
	} {
		a := attention.Ask{Delivery: attention.DeliveryQueued, DeliveryDetail: detail}
		got, retry := answerDeliveryText(a, who, false)
		if got != want || retry {
			t.Errorf("%q: %q (retry %v), want %q", detail, got, retry, want)
		}
	}
}

// TestAnOverLongAnswerSaysSo: an answer over attention.MaxAnswerRunes (a long
// dictation) returns to the ask page with its own notice, never "Type an
// answer or pick an option." — which a blank answer still gets. The refusal
// is the store's own (attention.CleanAnswer). RED before: both landed on
// ?notice=ask-invalid.
func TestAnOverLongAnswerSaysSo(t *testing.T) {
	now := time.Now()
	a := attention.Ask{ID: 33, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Notes?", Status: attention.Open, CreatedAt: now}
	s := whereServer(t, a)
	_, long := attention.CleanAnswer(strings.Repeat("a", attention.MaxAnswerRunes+1))
	_, blank := attention.CleanAnswer("   ")
	if long == nil || blank == nil {
		t.Fatalf("control: CleanAnswer took %v / %v", long, blank)
	}
	if got := attentionErrText(long, a); got != "the answer is longer than 8000 characters" {
		t.Errorf("the over-length refusal's own words %q", got)
	}
	for err, want := range map[error][2]string{
		long:  {"ask-too-long", "Not recorded: the answer is longer than 8000 characters. Shorten it and send it again."},
		blank: {"ask-invalid", "Type an answer or pick an option."},
	} {
		form := url.Values{"return": {askPagePath(a.ID)}}
		r := httptest.NewRequest("POST", askPagePath(a.ID)+"/answer", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.attentionFormFailed(rec, r, err, a)
		if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != askPagePath(a.ID)+"?notice="+want[0] {
			t.Errorf("%v: %d %q, want 303 to ?notice=%s", err, rec.Code, loc, want[0])
			continue
		}
		if body := askPageBody(t, s, a, askPagePath(a.ID)+"?notice="+want[0]); !strings.Contains(body, want[1]) {
			t.Errorf("%v: the ask page does not say %q:\n%s", err, want[1], body)
		}
	}
}

// TestASettledAskPageNamesWhoAsked: a settled ask's page is headed by its
// state (Answered, Dismissed, Expired), so its badge row names who asked —
// which an open ask's page leaves to its heading ("… is asking"). RED before:
// a settled page named nobody.
func TestASettledAskPageNamesWhoAsked(t *testing.T) {
	now := time.Now()
	a := attention.Ask{ID: 34, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Ship it?", Status: attention.Open, CreatedAt: now}
	s := whereServer(t, a)
	const who = `<span class="font-semibold text-fg" data-ask-source>Worker Seven</span> asked · `
	if body := askPageBody(t, s, a, askPagePath(a.ID)); strings.Contains(body, "data-ask-source") || askPageH1(body) != "Worker Seven is asking" {
		t.Errorf("the open page: heading %q, or its badge row names the asker again:\n%s", askPageH1(body), body)
	}
	for _, st := range []attention.Status{attention.Answered, attention.Cancelled, attention.Expired} {
		a.Status = st
		body := askPageBody(t, s, a, askPagePath(a.ID))
		if !strings.Contains(body, `data-ask-place>`+strings.TrimSuffix(who, " · ")+`</span><span class="`+ui.AsksSubClass+`"><time `) {
			t.Errorf("%s: the settled page does not name who asked:\n%s", st, body)
		}
	}
	n := a
	n.Kind, n.Status = attention.KindNotice, attention.Cancelled
	if body := askPageBody(t, s, n, askPagePath(n.ID)); !strings.Contains(body, `data-ask-source>Worker Seven</span> sent a notice</span>`) {
		t.Errorf("a dismissed notice's page does not name who sent it:\n%s", body)
	}
}

// TestAStepUpCardOnTheFeedOffersNoAnswer: the feed's card for a step-up card
// draws what its page draws — no Confirm/Refuse, no answer box, no mic — and
// one plain status line (no fill, no border: nothing to tap) in their place,
// ended by Dismiss. The same line on the page is plain too. RED before: the
// feed drew Confirm and Refuse and the answer box, and the page drew the line
// in a bordered pill.
func TestAStepUpCardOnTheFeedOffersNoAnswer(t *testing.T) {
	a := stepUpFixtureAsk(t, "Big news.")
	a.Owner, a.CreatedAt = 1, time.Now()
	card := askFeedCardHTML(askItem(a), time.Now())
	want := `<div class="flex min-w-0 items-center gap-2" data-asks-stepup-row><div class="min-w-0 flex-1"><p class="break-words text-sm text-fg2" data-ask-stepup-phone>Confirm this on your phone.</p></div>` +
		askDismissButton("ask-"+strconv.FormatInt(a.ID, 10)+"-dismiss", "Dismiss without answering") + `</div>`
	if !strings.Contains(card, want) {
		t.Errorf("the feed's step-up card lacks its status row %s:\n%s", want, card)
	}
	for _, never := range []string{"data-attention-option", ">Confirm</button>", ">Refuse</button>", "<textarea", "data-attention-answer", "data-asks-send", "dictate"} {
		if strings.Contains(card, never) {
			t.Errorf("the feed's step-up card still draws %q:\n%s", never, card)
		}
	}
	// Control: an ordinary ask with the same options still draws them.
	plain := attention.Ask{ID: 77, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Ship?",
		Options: []string{hostDecisionConfirm, hostDecisionRefuse}, Status: attention.Open, CreatedAt: time.Now()}
	if c := askFeedCardHTML(askItem(plain), time.Now()); !strings.Contains(c, ">Confirm</button>") || strings.Contains(c, "data-ask-stepup-phone") {
		t.Errorf("an ordinary ask's card lost its options:\n%s", c)
	}
	for _, line := range []string{askStepUpPhoneHTML("text-sm"), askStepUpPhoneHTML("text-base")} {
		for _, pill := range []string{"ring-", "rounded", "bg-", "font-semibold"} {
			if strings.Contains(line, pill) {
				t.Errorf("the step-up line is drawn like a control (%s): %s", pill, line)
			}
		}
	}
}

// TestAnAskWithOptionsCollapsesItsFreeTextRow: on the ask page, an ask with
// options draws its free-text row hidden behind a quiet "Other answer…"
// control (the options are the answer; the form stays in the document, since
// they submit it); an ask without options draws the row open and no such
// control. RED before: the row was always open.
func TestAnAskWithOptionsCollapsesItsFreeTextRow(t *testing.T) {
	now := time.Now()
	opts := attention.Ask{ID: 41, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Squash or merge?",
		Options: []string{"Squash", "Merge"}, Status: attention.Open, CreatedAt: now}
	free := opts
	free.ID, free.Options = 42, nil
	s := whereServer(t, opts, free)
	other := `<button type="button" class="inline-flex min-h-[44px] items-center self-start rounded-lg px-1 text-sm font-semibold text-fg2 underline transition hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus" aria-controls="ask-answer" aria-expanded="false" data-ask-other>Other answer…</button>`
	body := askPageBody(t, s, opts, askPagePath(opts.ID))
	for _, want := range []string{other, `class="flex items-end gap-2" data-attention-answer hidden>`, `form="ask-answer" name="option" value="Squash"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the options page lacks %s:\n%s", want, body)
		}
	}
	body = askPageBody(t, s, free, askPagePath(free.ID))
	if strings.Contains(body, "data-ask-other>") || !strings.Contains(body, `class="flex items-end gap-2" data-attention-answer>`) {
		t.Errorf("the free-text page collapsed its only way to answer:\n%s", body)
	}
}

// TestTheAskPagePostsItsAnswerInPlace: the ask page's answer form is posted in
// place, so a failed post is said in one line under the form (#ask-error, an
// alert) instead of replacing the page, and the typed text stays. Its words
// are pinned whole. The behaviour is the e2e spec's (ask-page.spec: "an
// answer that fails to post").
func TestTheAskPagePostsItsAnswerInPlace(t *testing.T) {
	a := attention.Ask{ID: 51, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Which branch?",
		Status: attention.Open, CreatedAt: time.Now()}
	s := whereServer(t, a)
	body := askPageBody(t, s, a, askPagePath(a.ID))
	for _, want := range []string{
		`</form>
<p id="ask-error" role="alert" class="px-1 pt-2 text-sm text-st-error-fg" hidden data-ask-error></p>`,
		"failed(`Your answer wasn't sent: muster couldn't record it. Try again.`);",
		"failed(`Your answer wasn't sent: muster couldn't be reached. Check your connection and try again.`);",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the ask page lacks %s", want)
		}
	}
}
