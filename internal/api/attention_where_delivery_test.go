package api

import (
	"context"
	"encoding/json"
	"html"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
)

// WHERE AN ANSWER'S DELIVERY SENDS THE OPERATOR, AND WHERE THE CHIEF SEES A
// SESSION: an answered ask's card and its delivery line ("Queued for X…",
// "Waiting for X to read it…") name the asking session's window by its exact
// tmux target (askSource), as an open ask does — or say no window shows it;
// and the chief's fleet_list carries the same `where` for each window, so an
// ask the chief writes can name it. Fixture: whereServer (attention_where_test.go).

// whereAnswered is an attention store whose recent answers are answered.
type whereAnswered struct {
	openAsks
	answered []attention.Ask
}

// Closed is the Done tab's read: the same answers, one page.
func (w whereAnswered) Closed(context.Context, attention.ClosedPage) ([]attention.Ask, error) {
	return w.answered, nil
}

// Unconfirmed is none: the fixture's unconfirmed answer is listed in order.
func (w whereAnswered) Unconfirmed(context.Context) ([]attention.Ask, error) { return nil, nil }

func whereAnsweredAsks() []attention.Ask {
	at := time.Now().Add(-time.Minute)
	ans := func(id int64, sid, label string, d attention.Delivery, detail string) attention.Ask {
		return attention.Ask{ID: id, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: sid, Label: label},
			Question: "Which branch?", Status: attention.Answered, Answer: "main", CreatedAt: at, ResolvedAt: at,
			Delivery: d, DeliveryDetail: detail}
	}
	return []attention.Ask{
		ans(101, whereLabelledSID, "mango:1 @ ws", attention.DeliveryQueued, "the session is busy"),
		ans(102, whereGoneSID, "pear:2 @ ws", attention.DeliveryQueued, ""),
		ans(103, whereUnlabelledSID, "build:7 @ ws", attention.DeliveryPull, ""),
		ans(104, whereLabelledSID, "mango:1 @ ws", attention.DeliveryUnconfirmed, "the paste was not seen"),
	}
}

// whereDeliveries are whereAnsweredAsks' card source (the place under its
// title, in words — or the title itself when the place IS its name) and
// delivery line, by id. The line names the session as the title does, never
// by its tmux target: that is the card's ⓘ (whereTargets).
var whereDeliveries = map[int64][2]string{
	101: {"on ws", "Will reach mango when it's back."},
	102: {"pear:2 @ ws (not shown in any mirrored window now)", "Queued for pear:2 @ ws (not shown in any mirrored window now): delivering it as a new message in its session."},
	103: {"on ws", "Waiting for build to read it: a mirrored session reads its answer itself (attention_wait); muster does not type into it."},
	104: {"on ws", "Delivery failed: the paste was not seen."},
}

// whereTitles are the Answered cards' titles: the window's own name (its
// label, else its tmux session, without tmux's window index), as an Open
// card's (attentionTitle).
var whereTitles = map[int64]string{101: "mango", 102: "pear:2 @ ws (not shown in any mirrored window now)", 103: "build", 104: "mango"}

// whereTargets are the exact tmux targets the Answered cards' ⓘ hold, by id
// — where a queued or waiting delivery sends the owner; "" for the ask no
// window shows now (no ⓘ).
var whereTargets = map[int64]string{101: "tmux scratch3:5 @ ws", 102: "", 103: "tmux build:7 @ ws", 104: "tmux scratch3:5 @ ws"}

var (
	answeredCardTitle  = regexp.MustCompile(`data-attention-title([^>]*)>([^<]*)</span>`)
	answeredCardSource = regexp.MustCompile(`<span class="[^"]*" data-attention-source>([^<]*)</span>`)
	answeredCardLine   = regexp.MustCompile(`data-attention-delivery>([^<]*)</p>`)
)

// TestAnAnswersDeliveryLineNamesTheExactTmuxTarget: the Answered tab's feed
// (GET /attention/items?part=answered, its live refresh), each card's source
// and delivery line whole.
func TestAnAnswersDeliveryLineNamesTheExactTmuxTarget(t *testing.T) {
	s := whereServer(t)
	s.ext.Attention = whereAnswered{answered: whereAnsweredAsks()}
	rec := httptest.NewRecorder()
	s.handleAttentionItems(rec, httptest.NewRequest("GET", "/attention/items?part=answered", nil).WithContext(s.attentionPrincipal(context.Background(), 1)))
	chunks := strings.Split(rec.Body.String(), `<li id="answered-`)[1:]
	if len(chunks) != len(whereDeliveries) {
		t.Fatalf("%d answered cards, want %d:\n%s", len(chunks), len(whereDeliveries), rec.Body.String())
	}
	for _, c := range chunks {
		id, _ := strconv.ParseInt(c[:strings.Index(c, `"`)], 10, 64)
		want := whereDeliveries[id]
		title, line := answeredCardTitle.FindStringSubmatch(c), answeredCardLine.FindStringSubmatch(c)
		if title == nil || line == nil {
			t.Fatalf("answer %d: no title or delivery line:\n%s", id, c)
		}
		if got := html.UnescapeString(title[2]); got != whereTitles[id] {
			t.Errorf("answer %d: title %q, want %q", id, got, whereTitles[id])
		}
		src := ""
		if strings.Contains(title[1], "data-attention-source") {
			src = title[2]
		} else if m := answeredCardSource.FindStringSubmatch(c); m != nil {
			src = m[1]
		}
		if got := html.UnescapeString(src); got != want[0] {
			t.Errorf("answer %d: source %q, want %q", id, got, want[0])
		}
		if got := html.UnescapeString(line[1]); got != want[1] {
			t.Errorf("answer %d: delivery %q, want %q", id, got, want[1])
		}
		// The exact target is reachable: in the card's ⓘ, copied whole.
		if target := whereTargets[id]; target == "" {
			if strings.Contains(c, "data-info-code") {
				t.Errorf("answer %d: an ⓘ on an ask no window shows", id)
			}
		} else if !strings.Contains(c, `data-info-code="">`+target+`</code>`) || !strings.Contains(c, `data-info-copy=""`) {
			t.Errorf("answer %d: the ⓘ does not hold %q", id, target)
		}
	}
}

var askPageDelivery = regexp.MustCompile(`data-ask-delivery>([^<]*)</p>`)

// TestAnAnsweredAskPagesDeliveryLineNamesTheExactTmuxTarget: the same line on
// the ask's own page once it is answered.
func TestAnAnsweredAskPagesDeliveryLineNamesTheExactTmuxTarget(t *testing.T) {
	s := whereServer(t)
	for _, a := range whereAnsweredAsks() {
		rec := httptest.NewRecorder()
		s.renderAskPage(rec, httptest.NewRequest("GET", askPagePath(a.ID), nil).WithContext(s.attentionPrincipal(context.Background(), 1)), a)
		m := askPageDelivery.FindStringSubmatch(rec.Body.String())
		if m == nil {
			t.Fatalf("ask %d: no delivery line:\n%s", a.ID, rec.Body.String())
		}
		if got, want := html.UnescapeString(m[1]), whereDeliveries[a.ID][1]; got != want {
			t.Errorf("ask %d: delivery %q, want %q", a.ID, got, want)
		}
	}
}

// TestTheChiefsFleetListCarriesEachWindowsWhere: fleet_list, as the chief
// reads it, gives each mirrored window's where (and keeps its name and host).
func TestTheChiefsFleetListCarriesEachWindowsWhere(t *testing.T) {
	s := whereServer(t)
	var got struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(s.toolFleetList(s.attentionPrincipal(context.Background(), 1), "")), &got); err != nil {
		t.Fatal(err)
	}
	// name is the session's stable name (internal/sessionname): with no
	// human one, its CODE name (codeName: the codename or tmux address, index
	// and all — an agent's name for a session stays stable as siblings come
	// and go) — no longer with " @ <host>", which is `host`.
	want := map[string][3]string{
		"mirror:" + whereLabelledSID:   {"mango:1", "ws", "mango:1 · tmux scratch3:5 @ ws"},
		"mirror:" + whereUnlabelledSID: {"build:7", "ws", "tmux build:7 @ ws"},
	}
	if len(got.Sessions) != len(want) {
		t.Fatalf("%d rows, want %d: %+v", len(got.Sessions), len(want), got.Sessions)
	}
	for _, row := range got.Sessions {
		id, _ := row["id"].(string)
		w, ok := want[id]
		if !ok {
			t.Fatalf("unexpected row %+v", row)
		}
		if row["name"] != w[0] || row["host"] != w[1] || row["where"] != w[2] {
			t.Errorf("%s: name %v host %v where %v, want %q %q %q", id, row["name"], row["host"], row["where"], w[0], w[1], w[2])
		}
	}
}
