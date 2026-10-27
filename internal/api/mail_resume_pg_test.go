package api

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// RESUMING AN ENDED SESSION FOR ITS MAIL (mail_resume.go, migration 0056),
// through the real dispatch against a real Postgres: mail muster would route
// itself, for a session no window shows, has the session's host resume it and
// is then routed there — or, at every gate, goes to the chief exactly as
// before. The host is played (resumeMirror); the host's own half is
// internal/mirror's resume_test.go.
//
// Fixture values are invented and pairwise distinct from every other test's.

const (
	resumeSender = "peer-desk/builder"
	resumeThread = "7311"
	// goneSID is what window @4 holds once routeSID's window is gone.
	goneSID = "5e55a1d0-0000-4000-8000-0000000c0ffe"
)

// resumeMirror is routeMirror plus the resume: whether host tower offers it,
// what it answers, and whether the session's window then comes up.
type resumeMirror struct {
	*routeMirror
	rmu sync.Mutex
	// offer: tower advertises ResumeSpawn.
	offer bool
	// attic, when set, is a second online host that offers resumes and holds
	// no transcript (it refuses not_here); it sorts before tower.
	attic bool
	// remembered, when set, is an OFFLINE host whose last list shows routeSID.
	remembered bool
	refuse     error
	// appear: a resume brings routeSID's window up (after delay).
	appear bool
	delay  time.Duration
	spawns []string // "<host> <sid>"
}

func (m *resumeMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := m.routeMirror.Hosts(ctx)
	m.rmu.Lock()
	defer m.rmu.Unlock()
	hs[0].ResumeSpawn = m.offer
	if m.attic {
		hs = append([]mirror.HostView{{Host: "attic", Online: true, Rendered: true, Interactive: true, StatusReport: true, ResumeSpawn: true}}, hs...)
	}
	if m.remembered {
		ws := mirror.NormalizeWindows([]mirror.Window{{Session: "old", ID: "@2", Index: 0, Active: true, Claude: true, ClaudeSession: routeSID}}, 0)
		hs = append(hs, mirror.HostView{Host: "zinc", Windows: ws, Online: false})
	}
	return hs
}

func (m *resumeMirror) SpawnResume(_ context.Context, host, sid string) (mirror.ResumedWindow, error) {
	m.rmu.Lock()
	m.spawns = append(m.spawns, host+" "+sid)
	refuse, appear, delay := m.refuse, m.appear, m.delay
	m.rmu.Unlock()
	if host == "attic" {
		return mirror.ResumedWindow{}, &mirror.ResumeRefusedError{Kind: mirror.ResumeRefusalNotHere, Why: "no transcript here"}
	}
	if refuse != nil {
		return mirror.ResumedWindow{}, refuse
	}
	if appear {
		go func() {
			time.Sleep(delay)
			m.routeMirror.set(func(r *routeMirror) { r.claudeSession = "" })
		}()
	}
	return mirror.ResumedWindow{Session: "muster-resume", Window: "@9"}, nil
}

func (m *resumeMirror) spawned() []string {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	return append([]string(nil), m.spawns...)
}

func (m *resumeMirror) setR(f func(*resumeMirror)) {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	f(m)
}

type resumeFixture struct {
	*prFixture
	rm *resumeMirror
}

// newResumeFixture: thread resumeThread bound to routeSID while its window
// showed it, the window now gone (it holds goneSID); the feature ON for
// resumeSender and prSender, tower offering resumes and the window coming up.
func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	f := &resumeFixture{prFixture: newPRFixture(t)}
	if code, _ := f.bind(t, "", resumeThread, routeSID); code != 201 {
		t.Fatalf("bind: %d", code)
	}
	f.rm = &resumeMirror{routeMirror: f.mirror, offer: true, appear: true, delay: 50 * time.Millisecond}
	f.srv.ext.Mirror = f.rm
	f.mirror.set(func(m *routeMirror) { m.claudeSession = goneSID })
	f.allowResume(t, resumeSender, prSender)
	f.srv.ext.MailResumes = f.plugins
	f.srv.mailResumeWait, f.srv.mailResumePoll = 3*time.Second, 10*time.Millisecond
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM mail_resumes WHERE owner_id = $1`, f.owner)
	})
	return f
}

func (f *resumeFixture) allowResume(t *testing.T, senders ...string) {
	t.Helper()
	var entries []string
	for _, s := range senders {
		entries = append(entries, strconv.FormatInt(f.owner, 10)+":"+plugins.AgentMailPlugin+":"+s)
	}
	l, err := plugins.ParseRelayAllowlist(strings.Join(entries, ","))
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.MailResume = &MailResume{Allow: l}
}

// reply stores one agent-mail reply on the bound thread and hands it on.
func (f *resumeFixture) reply(t *testing.T, external, sender string, verified bool) plugins.Message {
	t.Helper()
	m, created, err := f.plugins.Submit(f.asOwner, f.agentMail.ID, plugins.MessageIn{ExternalID: external, Sender: sender,
		Subject: "re: the gizmo build", Body: "the gizmo build is green on attempt 3", SenderVerified: verified, Thread: resumeThread})
	if err != nil || !created {
		t.Fatalf("submit: %v %v", created, err)
	}
	f.dispatch(t, m)
	return m
}

type resumeRow struct {
	state, host, window, detail string
	spawned                     bool
}

// resumeRows is message m's mail_resumes rows.
func (f *resumeFixture) resumeRows(t *testing.T, m plugins.Message) []resumeRow {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT state, coalesce(host, ''), coalesce(window_id, ''),
		coalesce(detail, ''), spawned_at IS NOT NULL FROM mail_resumes WHERE plugin_message_id = $1`, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []resumeRow
	for rows.Next() {
		var r resumeRow
		if err := rows.Scan(&r.state, &r.host, &r.window, &r.detail, &r.spawned); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestMailForAnEndedThreadSessionResumesItAndIsRoutedThere is the feature: a
// verified, listed sender's reply on a thread whose session no window shows
// has tower resume the session, waits for its window, and is routed there
// through the send queue — the chief takes no turn.
func TestMailForAnEndedThreadSessionResumesItAndIsRoutedThere(t *testing.T) {
	f := newResumeFixture(t)
	m := f.reply(t, "resume-happy-1", resumeSender, true)
	if got := f.rm.spawned(); len(got) != 1 || got[0] != "tower "+routeSID {
		t.Fatalf("spawned %q, want exactly tower %s", got, routeSID)
	}
	rs := f.routesOf(t, m)
	if len(rs) != 1 || rs[0].State != plugins.RouteRouted || rs[0].TargetKey != routeSID {
		t.Fatalf("routes: %+v", rs)
	}
	items := f.queued(t)
	if len(items) != 1 || !strings.Contains(items[0].Body, "Routed to you by "+routedByThread) ||
		!strings.Contains(items[0].Body, "the gizmo build is green on attempt 3") {
		t.Fatalf("queued: %+v", items)
	}
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s), want 0", n)
	}
	if rows := f.resumeRows(t, m); len(rows) != 1 || rows[0] != (resumeRow{state: plugins.ResumeDelivered, host: "tower", window: "@9", spawned: true}) {
		t.Fatalf("mail_resumes: %+v", rows)
	}
}

// TestTheMailResumeIsOffUnlessEveryGateAllowsIt: each gate alone, with every
// other one open, sends the message to the chief exactly as before — no
// resume asked, no route.
func TestTheMailResumeIsOffUnlessEveryGateAllowsIt(t *testing.T) {
	for name, tc := range map[string]struct {
		set      func(t *testing.T, f *resumeFixture)
		sender   string
		verified bool
	}{
		"not configured (the default)": {func(_ *testing.T, f *resumeFixture) { f.srv.ext.MailResume = nil }, resumeSender, true},
		"an empty allowlist":           {func(_ *testing.T, f *resumeFixture) { f.srv.ext.MailResume = &MailResume{} }, resumeSender, true},
		"the kill switch":              {func(_ *testing.T, f *resumeFixture) { f.srv.ext.MailResume.Off = true }, resumeSender, true},
		"a sender not listed":          {func(*testing.T, *resumeFixture) {}, "stranger-desk/builder", true},
		"a listed sender, unverified":  {func(*testing.T, *resumeFixture) {}, resumeSender, false},
		"no host offers resumes":       {func(_ *testing.T, f *resumeFixture) { f.rm.setR(func(m *resumeMirror) { m.offer = false }) }, resumeSender, true},
		"another owner's allowlist": {func(t *testing.T, f *resumeFixture) {
			l, err := plugins.ParseRelayAllowlist(strconv.FormatInt(f.owner+1000, 10) + ":" + plugins.AgentMailPlugin + ":" + resumeSender)
			if err != nil {
				t.Fatal(err)
			}
			f.srv.ext.MailResume = &MailResume{Allow: l}
		}, resumeSender, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newResumeFixture(t)
			tc.set(t, f)
			m := f.reply(t, "resume-gate-"+strings.ReplaceAll(name, " ", "-"), tc.sender, tc.verified)
			if got := f.rm.spawned(); len(got) != 0 {
				t.Fatalf("a resume was asked: %q", got)
			}
			if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 || len(f.resumeRows(t, m)) != 0 {
				t.Fatalf("%d routes, %d chief turns, %d resume rows; want 0, 1, 0", len(rs), f.turns.count(), len(f.resumeRows(t, m)))
			}
		})
	}
}

// TestASessionLiveAnywhereIsNeverResumed: a window shows the session but
// cannot take the route (a read-only host), or a host that went offline still
// lists it — either way it may still run, and a second CLI on one
// conversation is never started.
func TestASessionLiveAnywhereIsNeverResumed(t *testing.T) {
	for name, set := range map[string]func(f *resumeFixture){
		"on a read-only host": func(f *resumeFixture) {
			f.mirror.set(func(m *routeMirror) { m.claudeSession, m.interactive = "", false })
		},
		"on a host that is offline now": func(f *resumeFixture) {
			f.rm.setR(func(m *resumeMirror) { m.remembered = true })
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newResumeFixture(t)
			set(f)
			m := f.reply(t, "resume-live-"+strings.ReplaceAll(name, " ", "-"), resumeSender, true)
			if got := f.rm.spawned(); len(got) != 0 || f.turns.count() != 1 || len(f.resumeRows(t, m)) != 0 {
				t.Fatalf("spawned %q, %d chief turns, %d rows; want none, 1, 0", got, f.turns.count(), len(f.resumeRows(t, m)))
			}
		})
	}
}

// TestAHostsRefusalFallsBackToTheChief: the host refuses (the session runs
// there, unseen): nothing started, the reservation released, the row says
// why, and the chief gets the message.
func TestAHostsRefusalFallsBackToTheChief(t *testing.T) {
	f := newResumeFixture(t)
	f.rm.setR(func(m *resumeMirror) {
		m.refuse = &mirror.ResumeRefusedError{Kind: mirror.ResumeRefusalLive, Why: "it runs here"}
	})
	m := f.reply(t, "resume-refused-1", resumeSender, true)
	rows := f.resumeRows(t, m)
	if len(rows) != 1 || rows[0].state != plugins.ResumeRefused || rows[0].host != "" || rows[0].spawned ||
		!strings.Contains(rows[0].detail, "it runs here") {
		t.Fatalf("mail_resumes: %+v", rows)
	}
	if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 {
		t.Fatalf("%d routes, %d chief turns; want 0, 1", len(rs), f.turns.count())
	}
}

// TestAResumedWindowThatNeverComesUpFallsBackToTheChief: the bounded wait.
func TestAResumedWindowThatNeverComesUpFallsBackToTheChief(t *testing.T) {
	f := newResumeFixture(t)
	f.rm.setR(func(m *resumeMirror) { m.appear = false })
	f.srv.mailResumeWait = 200 * time.Millisecond
	m := f.reply(t, "resume-timeout-1", resumeSender, true)
	rows := f.resumeRows(t, m)
	if len(rows) != 1 || rows[0].state != plugins.ResumeTimedOut || rows[0].host != "tower" || rows[0].window != "@9" {
		t.Fatalf("mail_resumes: %+v", rows)
	}
	if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 {
		t.Fatalf("%d routes, %d chief turns; want 0, 1", len(rs), f.turns.count())
	}
}

// TestAMessageResumesASessionAtMostOnce: dispatched again, the same message
// finds its durable row and asks for no second resume.
func TestAMessageResumesASessionAtMostOnce(t *testing.T) {
	f := newResumeFixture(t)
	f.rm.setR(func(m *resumeMirror) { m.appear = false })
	f.srv.mailResumeWait = 100 * time.Millisecond
	m := f.reply(t, "resume-once-1", resumeSender, true)
	f.dispatch(t, m)
	if got := f.rm.spawned(); len(got) != 1 {
		t.Fatalf("spawned %q, want exactly one", got)
	}
	if rows := f.resumeRows(t, m); len(rows) != 1 {
		t.Fatalf("mail_resumes: %+v", rows)
	}
}

// TestTheResumeRateLimitsHold: per session (MailResumePerSessionPerHour) and
// per host (MailResumePerHostPerHour), counted from the durable rows.
func TestTheResumeRateLimitsHold(t *testing.T) {
	t.Run("per session", func(t *testing.T) {
		f := newResumeFixture(t)
		f.rm.setR(func(m *resumeMirror) { m.appear = false })
		f.srv.mailResumeWait = 50 * time.Millisecond
		for i := 0; i < MailResumePerSessionPerHour+1; i++ {
			f.reply(t, "resume-rate-s-"+strconv.Itoa(i), resumeSender, true)
		}
		if got := len(f.rm.spawned()); got != MailResumePerSessionPerHour {
			t.Fatalf("%d resumes, want %d", got, MailResumePerSessionPerHour)
		}
		if n := f.turns.count(); n != MailResumePerSessionPerHour+1 {
			t.Fatalf("%d chief turns, want every message's", n)
		}
	})
	t.Run("per host", func(t *testing.T) {
		f := newResumeFixture(t)
		for i := 0; i < MailResumePerHostPerHour; i++ {
			dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO mail_resumes (owner_id, plugin_message_id, claude_session, sender, via, host, spawned_at, state)
				VALUES ($1, $2, $3, 'peer-desk/other', 'thread', 'tower', now() - interval '10 minutes', 'timed_out')`,
				f.owner, 900000+i, "5e55a1d0-0000-4000-8000-00000000d0"+strconv.Itoa(10+i))
		}
		m := f.reply(t, "resume-rate-h-1", resumeSender, true)
		if got := f.rm.spawned(); len(got) != 0 {
			t.Fatalf("a resume was asked of a host over its limit: %q", got)
		}
		if rows := f.resumeRows(t, m); len(rows) != 1 || rows[0].state != plugins.ResumeRefused || f.turns.count() != 1 {
			t.Fatalf("mail_resumes %+v, %d chief turns", rows, f.turns.count())
		}
	})
}

// TestAThreadResumeAsksEachHostUntilOneHoldsTheTranscript: with no host
// recorded, every online host that offers resumes is asked in order; a
// not_here moves on, and the host that started the window is the one recorded.
func TestAThreadResumeAsksEachHostUntilOneHoldsTheTranscript(t *testing.T) {
	f := newResumeFixture(t)
	f.rm.setR(func(m *resumeMirror) { m.attic = true })
	m := f.reply(t, "resume-hosts-1", resumeSender, true)
	if got := f.rm.spawned(); len(got) != 2 || got[0] != "attic "+routeSID || got[1] != "tower "+routeSID {
		t.Fatalf("spawned %q, want attic then tower", got)
	}
	if rows := f.resumeRows(t, m); len(rows) != 1 || rows[0].state != plugins.ResumeDelivered || rows[0].host != "tower" {
		t.Fatalf("mail_resumes: %+v", rows)
	}
}

// TestAReviewForAnEndedAuthorResumesItOnlyOnItsRecordedHost: a pull-request
// review resumes its author on the host its hook recorded — and on no other
// host when that one cannot (the author block then reaches the chief).
func TestAReviewForAnEndedAuthorResumesItOnlyOnItsRecordedHost(t *testing.T) {
	t.Run("recorded on tower", func(t *testing.T) {
		f := newResumeFixture(t)
		f.authored(t, f.asOwner, routeSID, prCreated())
		m := f.review(t, "resume-pr-1", prSender, true, reviewBody(fullFields()))
		if got := f.rm.spawned(); len(got) != 1 || got[0] != "tower "+routeSID {
			t.Fatalf("spawned %q", got)
		}
		if rs := f.routesOf(t, m); len(rs) != 1 || rs[0].TargetKey != routeSID || f.turns.count() != 0 {
			t.Fatalf("routes %+v, %d chief turns", rs, f.turns.count())
		}
		if rows := f.resumeRows(t, m); len(rows) != 1 || rows[0].state != plugins.ResumeDelivered {
			t.Fatalf("mail_resumes: %+v", rows)
		}
	})
	t.Run("recorded on a host that is not here", func(t *testing.T) {
		f := newResumeFixture(t)
		f.rm.setR(func(m *resumeMirror) { m.attic = true })
		if _, err := f.plugins.RecordPRSession(f.asOwner, routeSID, "laptop", prCreated()); err != nil {
			t.Fatal(err)
		}
		f.review(t, "resume-pr-2", prSender, true, reviewBody(fullFields()))
		if got := f.rm.spawned(); len(got) != 0 {
			t.Fatalf("a host the author was not recorded on was asked: %q", got)
		}
		if turn := f.lastTurn(); f.turns.count() != 1 || !strings.Contains(turn, routeSID) {
			t.Fatalf("%d chief turns; the turn does not name the author: %q", f.turns.count(), turn)
		}
	})
}
