package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
)

// NOTICES ON THE QUEUE (attention_notice.go): where a notice points, resolved
// now; its notification (no buttons for a paired phone); its tap.

// ByAsks is boxNudges' batch read: byAsk for every id it holds.
func (b boxNudges) ByAsks(_ context.Context, ids []int64) (map[int64]chiefnudge.Decision, error) {
	if b.err != nil {
		return nil, b.err
	}
	out := map[int64]chiefnudge.Decision{}
	for _, id := range ids {
		if d, ok := b.byAsk[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

// boxNotice is notice id as the store reads it: muster's own, labelled with
// the place the sweep saw ("ws · old:9"), no options.
func boxNotice(id int64) attention.Ask {
	return attention.Ask{ID: id, Owner: 1, Kind: attention.KindNotice, Asker: attention.Asker{System: true, Label: "ws · old:9"},
		Question: "Waiting on you: merge or wait?", Options: []string{}, Status: attention.Open, CreatedAt: time.Now()}
}

// TestANoticeNamesWhereItsSessionIsNow: a notice about a mirrored session names
// the window running that conversation NOW — "tmux main:2 @ ws" (mirror.Where,
// as every attention surface names a window) and the tmux target
// "main:2" — not the address its decision recorded (@9, "old"); its push has
// no ask payload (no buttons on a paired phone, an old app included) and names
// the place in its title; its tap opens that window's view.
func TestANoticeNamesWhereItsSessionIsNow(t *testing.T) {
	s := boxServer(t, true)
	ctx := s.attentionPrincipal(context.Background(), 1)
	it := s.askItemNow(ctx, boxNotice(boxNudgeMirror))
	if it.Kind != attentionKindNotice || it.Source != "tmux main:2 @ ws" || it.Target != "main:2" || it.host != "ws" || it.AskID != boxNudgeMirror {
		t.Fatalf("the notice item %+v", it)
	}
	n := attentionNotification(it, false)
	if n.Ask != nil || n.AskActions {
		t.Fatalf("a notice's push carries an ask payload (buttons): %+v", n.Ask)
	}
	if n.Title != "tmux main:2 @ ws may be waiting on you" || n.Body != "Waiting on you: merge or wait?" || n.Type != attentionNotificationType {
		t.Fatalf("the notice's push: title %q body %q type %q", n.Title, n.Body, n.Type)
	}
	if got, want := s.pushTargetFor(ctx, 1, it), "/mirror/view?host=ws&session=main&window=%402"; got != want {
		t.Fatalf("its tap opens %q, want %q", got, want)
	}
	// The Asks list's card: the place, the target, its head linking the
	// session, and Dismiss — and no answer box, though dictation is on.
	items := []attentionItem{it}
	s.withAskBoxes(ctx, items, true)
	card := attentionItemsHTML(items, time.Now())
	for _, want := range []string{`data-kind="notice"`, "tmux main:2 @ ws", `data-attention-target>main:2<`, "data-attention-session", "data-attention-dismiss"} {
		if !strings.Contains(card, want) {
			t.Fatalf("the card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "<textarea") || strings.Contains(card, "data-attention-answer") || strings.Contains(card, "-dictate") {
		t.Fatalf("the card draws an answer box or a mic:\n%s", card)
	}
}

// TestANoticeWithNoSessionNowNamesItsLabelAndNoTarget: another owner's decision
// naming the notice's id is ignored, and a session no window holds now names
// the label the sweep saw and no target — so `muster nudge next` skips it —
// and its tap falls back to its own page.
func TestANoticeWithNoSessionNowNamesItsLabelAndNoTarget(t *testing.T) {
	s := boxServer(t, false)
	// Another owner's decision about a mirrored session the owner's host DOES
	// run: followed, it would name that window (ws · main:2).
	const theirsMirror = 91
	s.ext.ChiefNudges.(boxNudges).byAsk[theirsMirror] = chiefnudge.Decision{Owner: 2, AskID: theirsMirror, Kind: chiefnudge.KindMirror,
		Key: ptOtherSID, Host: "ws", TmuxSession: "main", Window: "@2"}
	ctx := s.attentionPrincipal(context.Background(), 1)
	for _, c := range []struct {
		name   string
		id     int64
		source string
	}{
		{"another owner's decision", boxNudgeTheirs, "ws · old:9"},
		{"another owner's decision about a window the owner has", theirsMirror, "ws · old:9"},
		{"no window holds the session", boxNudgeNoWindow, "ws · old:9" + askNotShown},
		{"no decision at all", boxSystemNoDecision, "ws · old:9"},
	} {
		t.Run(c.name, func(t *testing.T) {
			it := s.askItemNow(ctx, boxNotice(c.id))
			if it.Source != c.source || it.Target != "" {
				t.Fatalf("source %q target %q, want %q and none", it.Source, it.Target, c.source)
			}
			if got := s.pushTargetFor(ctx, 1, it); got != askPagePath(c.id) {
				t.Fatalf("its tap opens %q, want its own page", got)
			}
		})
	}
}

// TestANoticeAboutAnOpencodePaneResolvesItsWindow: the session key of an
// opencode pane ("opencode:<id>") resolves to the window showing that opencode
// session, for the notice's place (led by the host's label for the window)
// and target alike.
func TestANoticeAboutAnOpencodePaneResolvesItsWindow(t *testing.T) {
	ws := mirror.NormalizeWindows([]mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Claude: true, ClaudeSession: focusSID},
		{Session: "oc", ID: "@7", Index: 3, Label: "mango:3", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_abc123"},
	}, 0)
	hosts := []mirror.HostView{{Host: "ws", Online: true, Windows: ws}}
	a := boxNotice(90)
	ds := map[int64]chiefnudge.Decision{90: {Owner: 1, AskID: 90, Kind: chiefnudge.KindMirror, Key: "opencode:ses_abc123", Host: "ws", TmuxSession: "oc", Window: "@7"}}
	it := noticeItem(a, ds, hosts)
	if it.Source != "mango:3 · tmux oc:3 @ ws" || it.Target != "oc:3" {
		t.Fatalf("the opencode notice: source %q target %q", it.Source, it.Target)
	}
	// What `muster nudge next` prints (the summary's item): the label too, and
	// the bare tmux target a jump key takes.
	if si := attentionSummaryItemOf(it, "", time.Now()); si.Title != "mango:3 · tmux oc:3 @ ws" || si.Target != "oc:3" || si.Kind != attentionKindNotice {
		t.Fatalf("the notice's summary item: %+v", si)
	}
}
