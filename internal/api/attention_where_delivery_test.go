package api

import (
	"context"
	"encoding/json"
	"html"
	"net/http/httptest"
	"regexp"
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

func (w whereAnswered) Answered(context.Context, time.Time, int) ([]attention.Ask, error) {
	return w.answered, nil
}

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

// whereDeliveries are whereAnsweredAsks' card source and delivery line, by id.
var whereDeliveries = map[int64][2]string{
	101: {"mango:1 · tmux scratch3:5 @ ws", "Queued for mango:1 · tmux scratch3:5 @ ws — waiting: the session is busy. muster keeps trying."},
	102: {"pear:2 @ ws (not shown in any mirrored window now)", "Queued for pear:2 @ ws (not shown in any mirrored window now): delivering it as a new message in its session."},
	103: {"tmux build:7 @ ws", "Waiting for tmux build:7 @ ws to read it: a mirrored session reads its answer itself (attention_wait); muster does not type into it."},
	104: {"mango:1 · tmux scratch3:5 @ ws", "Delivery failed: the paste was not seen."},
}

var answeredCard = regexp.MustCompile(`(?s)<li id="answered-(\d+)".*?data-attention-source>([^<]*)</span>.*?data-attention-delivery>([^<]*)</p>`)

// TestAnAnswersDeliveryLineNamesTheExactTmuxTarget: the Answered list on the
// queue (GET /attention/items), each card's source and delivery line whole.
func TestAnAnswersDeliveryLineNamesTheExactTmuxTarget(t *testing.T) {
	s := whereServer(t)
	s.ext.Attention = whereAnswered{answered: whereAnsweredAsks()}
	rec := httptest.NewRecorder()
	s.handleAttentionItems(rec, httptest.NewRequest("GET", "/attention/items", nil).WithContext(s.attentionPrincipal(context.Background(), 1)))
	cards := answeredCard.FindAllStringSubmatch(rec.Body.String(), -1)
	if len(cards) != len(whereDeliveries) {
		t.Fatalf("%d answered cards, want %d:\n%s", len(cards), len(whereDeliveries), rec.Body.String())
	}
	for _, m := range cards {
		var id int64
		_ = json.Unmarshal([]byte(m[1]), &id)
		want := whereDeliveries[id]
		if got := html.UnescapeString(m[2]); got != want[0] {
			t.Errorf("answer %d: source %q, want %q", id, got, want[0])
		}
		if got := html.UnescapeString(m[3]); got != want[1] {
			t.Errorf("answer %d: delivery %q, want %q", id, got, want[1])
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
	if err := json.Unmarshal([]byte(s.toolFleetList(s.attentionPrincipal(context.Background(), 1))), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{
		"mirror:" + whereLabelledSID:   {"mango:1 @ ws", "ws", "mango:1 · tmux scratch3:5 @ ws"},
		"mirror:" + whereUnlabelledSID: {"build:7 @ ws", "ws", "tmux build:7 @ ws"},
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
