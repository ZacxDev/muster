package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// turnsStore is an agents store whose owner-turn marker, chat rows and session
// claim are fixed values, whatever it is asked — so ownerTurnFor's own checks
// are what is measured — and which records every Set/Clear, in order.
type turnsStore struct {
	agents.Store
	turn   agents.OwnerTurn
	msgs   map[int64]agents.ChatMessage
	holder string

	mu      sync.Mutex
	log     []string // "set <id> <lane>" / "clear <id>"
	setGate chan struct{}
	// failClears: the next this-many ClearOwnerTurn calls fail.
	failClears int
}

func (s *turnsStore) SetOwnerTurn(_ context.Context, _, _, messageID int64, lane string, _ time.Duration) error {
	if s.setGate != nil {
		<-s.setGate // a slow write
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, "set "+strconv.FormatInt(messageID, 10)+" "+lane)
	return nil
}
func (s *turnsStore) ClearOwnerTurn(_ context.Context, _, messageID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, "clear "+strconv.FormatInt(messageID, 10))
	if s.failClears > 0 {
		s.failClears--
		return errors.New("the database blinked")
	}
	return nil
}
func (s *turnsStore) OwnerTurnInFlight(context.Context, int64) (agents.OwnerTurn, error) {
	return s.turn, nil
}
func (s *turnsStore) ChatMessage(_ context.Context, id int64) (agents.ChatMessage, error) {
	m, ok := s.msgs[id]
	if !ok {
		return agents.ChatMessage{}, errors.New("no such row")
	}
	return m, nil
}
func (s *turnsStore) KickoffClaimHolder(context.Context, int64) (string, error) { return s.holder, nil }
func (s *turnsStore) entries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

// lanesGateway is a gateway whose pod reports act (or err) from GET /v1/clis.
type lanesGateway struct {
	Gateway
	act agents.CLIActivity
	err error
}

func (g lanesGateway) CLIActivity(context.Context, agents.Agent) (agents.CLIActivity, error) {
	return g.act, g.err
}

// clis builds a pod's answer: the owner's lane (lane, "" = primary) busy, plus
// others.
func clis(lane string, others ...agents.CLIState) agents.CLIActivity {
	own := agents.CLIState{Known: true, Lane: lane, Primary: lane == "", SessionID: "own", Status: "busy"}
	return agents.CLIActivity{Version: 1, CLIs: append([]agents.CLIState{own}, others...)}
}

// TestOwnerTurnForChecksEveryCondition: ownerTurnFor releases only on a marker
// naming the chief's OWNER, pointing at the chief's own user row, with no
// muster-started turn holding the claim, the call over MCP, and every OTHER
// CLI in the pod idle by its own account — each failure its own sentence.
// Agent, user and row ids are pairwise distinct; lane names too.
func TestOwnerTurnForChecksEveryCondition(t *testing.T) {
	s := everyExtensionOn(t)
	chief := agents.Agent{ID: 31, Name: "chief", OwnerID: 7}
	msgs := map[int64]agents.ChatMessage{
		501: {ID: 501, AgentID: 31, Role: "user", Content: "dispatch it"},
		502: {ID: 502, AgentID: 32, Role: "user", Content: "another agent's"},
		503: {ID: 503, AgentID: 31, Role: "assistant", Content: "a reply"},
	}
	own := agents.OwnerTurn{By: 7, MessageID: 501, Lane: "lane-owner"}
	idle := func(lane string) agents.CLIState {
		return agents.CLIState{Known: true, Lane: lane, SessionID: lane, Status: "idle"}
	}
	busy := func(c agents.CLIState) agents.CLIState { c.Status = "busy"; return c }
	for _, c := range []struct {
		name    string
		turn    agents.OwnerTurn
		holder  string
		act     agents.CLIActivity
		actErr  error
		noLanes bool
		inProc  bool
		want    string
	}{
		{"no marker (a turn the owner did not type, or one ccd refused)", agents.OwnerTurn{}, "", clis("lane-owner"), nil, false, false, chiefReleaseNoOwnerTurn},
		{"a marker typed by someone else", agents.OwnerTurn{By: 8, MessageID: 501, Lane: "lane-owner"}, "", clis("lane-owner"), nil, false, false, chiefReleaseNotOwnersTurn},
		{"a marker naming another agent's row", agents.OwnerTurn{By: 7, MessageID: 502, Lane: "lane-owner"}, "", clis("lane-owner"), nil, false, false, chiefReleaseUnverifiable},
		{"a marker naming an assistant row", agents.OwnerTurn{By: 7, MessageID: 503, Lane: "lane-owner"}, "", clis("lane-owner"), nil, false, false, chiefReleaseUnverifiable},
		{"a muster-started turn holds the claim", own, "plugin-delivery/x", clis("lane-owner"), nil, false, false, chiefReleaseOutsideTurn},
		{"another lane busy (a zombie or self-woken turn)", own, "", clis("lane-owner", busy(idle("lane-zombie"))), nil, false, false, chiefReleaseOtherCLIBusy},
		{"the primary busy (a machine ask)", own, "", clis("lane-owner", agents.CLIState{Known: true, Primary: true, SessionID: "p", Status: "busy"}), nil, false, false, chiefReleaseOtherCLIBusy},
		{"an unknown CLI with a background shell", own, "", clis("lane-owner", agents.CLIState{SessionID: "bg", Status: "shell"}), nil, false, false, chiefReleaseOtherCLIBusy},
		{"a live CLI with no session record", own, "", clis("lane-owner", agents.CLIState{PID: 77, Status: "unaccounted", Unaccounted: true}), nil, false, false, chiefReleaseUnaccountedCLI},
		{"an unknown CLI waiting on a prompt", own, "", clis("lane-owner", agents.CLIState{SessionID: "bg2", Status: "waiting"}), nil, false, false, chiefReleaseOtherCLIBusy},
		{"the owner's lane not listed", own, "", agents.CLIActivity{Version: 1, CLIs: []agents.CLIState{idle("lane-other")}}, nil, false, false, chiefReleaseCLIsUnknown},
		{"the owner's lane named, but by a CLI ccd does not know", own, "", agents.CLIActivity{Version: 1, CLIs: []agents.CLIState{{Lane: "lane-owner", SessionID: "x", Status: "busy"}}}, nil, false, false, chiefReleaseOtherCLIBusy},
		{"the owner's lane listed idle-unknown only", own, "", agents.CLIActivity{Version: 1, CLIs: []agents.CLIState{{Lane: "lane-owner", SessionID: "x", Status: "idle"}}}, nil, false, false, chiefReleaseCLIsUnknown},
		{"ccd unreachable or too old", own, "", agents.CLIActivity{}, errors.New("404"), false, false, chiefReleaseCLIsUnknown},
		{"a gateway that cannot read the pod", own, "", agents.CLIActivity{}, nil, true, false, chiefReleaseCLIsUnknown},
		{"a gateway-runtime session's in-process call", own, "", clis("lane-owner"), nil, false, true, chiefReleaseNotOverMCP},
		{"only the owner's lane works, others idle (control)", own, "", clis("lane-owner", idle("lane-a"), agents.CLIState{Known: true, Primary: true, SessionID: "p", Status: "idle"}), nil, false, false, ""},
		{"the owner's turn on the primary, a lane idle (control)", agents.OwnerTurn{By: 7, MessageID: 501}, "", clis("", idle("lane-a")), nil, false, false, ""},
	} {
		s.ext.Agents = &turnsStore{turn: c.turn, msgs: msgs, holder: c.holder}
		s.ext.Gateway = lanesGateway{act: c.act, err: c.actErr}
		if c.noLanes {
			s.ext.Gateway = &turnGateway{}
		}
		ctx := context.Background()
		if c.inProc {
			ctx = withInProcessToolCall(ctx)
		}
		got, why := s.ownerTurnFor(ctx, chief)
		if why != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, why, c.want)
		}
		if c.want == "" && got.ID != 501 {
			t.Errorf("%s: resolved message %d, want 501", c.name, got.ID)
		}
	}
	// A store without the marker cannot vouch for anything.
	s.ext.Agents = listAgents{}
	if _, why := s.ownerTurnFor(context.Background(), chief); why != chiefReleaseUnverifiable {
		t.Errorf("a store without the marker: %q, want %q", why, chiefReleaseUnverifiable)
	}
}

// TestMarkOwnerTurnMarksOnlyAnAcceptedOwnersTurn: the marker is set only when
// the typer IS the agent's owner AND ccd accepted the text, in the lane ccd
// named; a turn ccd refused (no MarkTurnAccepted) never marks; the clear always
// names the turn's own row.
func TestMarkOwnerTurnMarksOnlyAnAcceptedOwnersTurn(t *testing.T) {
	s := everyExtensionOn(t)
	a := agents.Agent{ID: 31, Name: "chief", OwnerID: 7}
	msg := agents.ChatMessage{ID: 501, AgentID: 31, Role: "user"}

	st := &turnsStore{}
	s.ext.Agents = st
	ctx, clear := s.markOwnerTurn(context.Background(), a, msg, 7)
	if len(st.entries()) != 0 {
		t.Fatalf("marked before ccd accepted: %v", st.entries())
	}
	agents.MarkTurnAccepted(ctx, agents.TurnAcceptance{Lane: "lane-7"})
	agents.MarkTurnAccepted(ctx, agents.TurnAcceptance{Lane: "lane-other"}) // once only
	clear()
	if got := st.entries(); len(got) != 2 || got[0] != "set 501 lane-7" || got[1] != "clear 501" {
		t.Fatalf("accepted owner turn: %v; want [set 501 lane-7, clear 501]", got)
	}

	refused := &turnsStore{}
	s.ext.Agents = refused
	_, clear = s.markOwnerTurn(context.Background(), a, msg, 7) // ccd never accepts
	clear()
	if got := refused.entries(); len(got) != 1 || got[0] != "clear 501" {
		t.Fatalf("a refused turn: %v; want only the clear", got)
	}

	for _, by := range []int64{0, 8} { // nobody typed it; someone else typed it
		other := &turnsStore{}
		s.ext.Agents = other
		ctx, clear := s.markOwnerTurn(context.Background(), a, msg, by)
		agents.MarkTurnAccepted(ctx, agents.TurnAcceptance{Lane: "l"})
		clear()
		if len(other.entries()) != 0 {
			t.Errorf("typed by %d: %v; only the owner's own turn marks", by, other.entries())
		}
	}
}

// TestTheClearAlwaysWinsTheSet: ccd's 103 is handled on a transport goroutine,
// so the set can race the turn's end. A set still WRITING when the turn
// returns is waited for and then cleared (order: set, clear); a set that
// ARRIVES after the turn returned writes nothing (a late 103 on a cancelled or
// timed-out turn).
func TestTheClearAlwaysWinsTheSet(t *testing.T) {
	s := everyExtensionOn(t)
	a := agents.Agent{ID: 31, Name: "chief", OwnerID: 7}
	msg := agents.ChatMessage{ID: 501, AgentID: 31, Role: "user"}

	slow := &turnsStore{setGate: make(chan struct{})}
	s.ext.Agents = slow
	ctx, clear := s.markOwnerTurn(context.Background(), a, msg, 7)
	setDone := make(chan struct{})
	go func() { agents.MarkTurnAccepted(ctx, agents.TurnAcceptance{Lane: "l"}); close(setDone) }()
	time.Sleep(50 * time.Millisecond) // the set is now blocked inside SetOwnerTurn
	clearDone := make(chan struct{})
	go func() { clear(); close(clearDone) }()
	select {
	case <-clearDone:
		t.Fatal("the clear finished while a set was still writing")
	case <-time.After(50 * time.Millisecond):
	}
	close(slow.setGate)
	<-setDone
	<-clearDone
	if got := slow.entries(); len(got) != 2 || got[0] != "set 501 l" || got[1] != "clear 501" {
		t.Fatalf("a set racing the clear: %v; want [set 501 l, clear 501]", got)
	}

	late := &turnsStore{}
	s.ext.Agents = late
	ctx, clear = s.markOwnerTurn(context.Background(), a, msg, 7)
	clear()                                                        // the turn returned (cancelled, timed out)
	agents.MarkTurnAccepted(ctx, agents.TurnAcceptance{Lane: "l"}) // ...and then the 103 arrives
	if got := late.entries(); len(got) != 1 || got[0] != "clear 501" {
		t.Fatalf("a late 103: %v; want only the clear", got)
	}
}

// TestAFailedClearIsRetriedOnceThenSaidLoudly: a clear that fails is retried
// once, on a fresh context; one that fails twice is logged as a marker left
// live. A clear that works the first time is not retried (control).
func TestAFailedClearIsRetriedOnceThenSaidLoudly(t *testing.T) {
	a := agents.Agent{ID: 31, Name: "chief", OwnerID: 7}
	msg := agents.ChatMessage{ID: 501, AgentID: 31, Role: "user"}
	for _, c := range []struct {
		name       string
		fail       int
		wantClears int
		wantLog    bool
	}{
		{"works first time", 0, 1, false},
		{"fails once", 1, 2, false},
		{"fails twice", 2, 2, true},
	} {
		s := everyExtensionOn(t)
		var buf bytes.Buffer
		s.logger = log.New(&buf, "", 0)
		st := &turnsStore{failClears: c.fail}
		s.ext.Agents = st
		_, clear := s.markOwnerTurn(context.Background(), a, msg, 7)
		clear()
		if got := len(st.entries()); got != c.wantClears {
			t.Errorf("%s: %d clear attempt(s), want %d", c.name, got, c.wantClears)
		}
		if logged := strings.Contains(buf.String(), "could NOT clear chief's owner-turn marker (message 501) after a retry"); logged != c.wantLog {
			t.Errorf("%s: logged the stuck marker = %v, want %v (log %q)", c.name, logged, c.wantLog, buf.String())
		}
	}
}
