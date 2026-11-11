package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askattempt"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE ANSWER-ATTEMPT LOG (ask_attempts.go), through the real routes: every
// exit of every door records exactly one row with its reason; the set of
// reasons the doors produce IS askattempt.Reasons; no row holds the question's
// or the answer's text; a retry re-reads and resets the picker and is
// attempt 2; "Send as a message" dismisses, then queues, behind the step-up.
// ---------------------------------------------------------------------------

// attemptServer is askServer with an in-memory answer-attempt log.
func attemptServer(t *testing.T, interactive bool) (*askMirror, *Server, *httptest.Server, *askattempt.Mem) {
	t.Helper()
	am, s, ts := askServer(t, interactive)
	mem := &askattempt.Mem{}
	s.ext.AskAttempts = mem
	return am, s, ts, mem
}

// setAsk sets fields of am under its lock.
func setAsk(am *askMirror, f func(a *askMirror)) {
	am.amu.Lock()
	f(am)
	am.amu.Unlock()
}

// postAsk posts form to path as the operator (cookie "" is a fresh one).
func postAsk(t *testing.T, s *Server, ts *httptest.Server, path string, form url.Values, cookie string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie == "" {
		cookie = sessionCookieFor(s)
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", ts.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// pathCase is one exit of one door: run takes it, and the ONE row it must
// leave has these fields.
type pathCase struct {
	name                                  string
	reason, door, action, method, runtime string
	run                                   func(t *testing.T) *askattempt.Mem
}

// claudeFailure is a card answer the host refuses with err: the row it
// leaves.
func claudeFailure(err error) func(t *testing.T) *askattempt.Mem {
	return func(t *testing.T) *askattempt.Mem {
		am, s, ts, mem := attemptServer(t, true)
		register(t, ts)
		region(t, s, ts, `data-ask-state="pending"`)
		setAsk(am, func(a *askMirror) { a.typeErr = err })
		if resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusConflict {
			t.Fatalf("a refused typing: %d\n%s", resp.StatusCode, body)
		}
		return mem
	}
}

// heldTyping is a server whose first answer is being typed (held) until the
// test ends.
func heldTyping(t *testing.T) (*askMirror, *Server, *httptest.Server, *askattempt.Mem) {
	am, s, ts, mem := attemptServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	hold := make(chan struct{})
	setAsk(am, func(a *askMirror) { a.hold = hold })
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", "")
	}()
	region(t, s, ts, `data-ask-state="typing"`)
	t.Cleanup(func() { close(hold); <-done })
	return am, s, ts, mem
}

// messageServer is a server whose card failed with err (a message recovery)
// and that can queue for the window.
func messageServer(t *testing.T, err error) (*askMirror, *Server, *httptest.Server, *askattempt.Mem, *memQueue) {
	am, s, ts, mem := attemptServer(t, true)
	q := &memQueue{}
	s.ext.SendQueue = q
	setAsk(am, func(a *askMirror) {
		a.statusReport = true
		a.windows[0].Status, a.windows[0].StatusSince = mirror.StatusWaiting, 1792000001111
		a.typeErr = err
	})
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	if resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat", "a1", "Fish"), "", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("the failing answer: %d\n%s", resp.StatusCode, body)
	}
	return am, s, ts, mem, q
}

// rowsSince is mem's rows after the first n.
func rowsSince(mem *askattempt.Mem, n int) []askattempt.Attempt { return mem.All()[n:] }

func claudeCases() []pathCase {
	const cc, card, sub, dis, typed, esc, msg = askattempt.RuntimeClaudeCode, askattempt.DoorCard, askattempt.ActionSubmit,
		askattempt.ActionDismiss, askattempt.MethodTyped, askattempt.MethodEscape, askattempt.MethodMessage
	fail := func(name, reason string, err error) pathCase {
		return pathCase{name, reason, card, sub, typed, cc, claudeFailure(err)}
	}
	return []pathCase{
		{"typed", "typed", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, true)
			register(t, ts)
			region(t, s, ts, `data-ask-state="pending"`)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusOK {
				t.Fatalf("typed: %d", resp.StatusCode)
			}
			return mem
		}},
		{"from the phone's shell", "typed", askattempt.DoorPhone, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, true)
			register(t, ts)
			region(t, s, ts, `data-ask-state="pending"`)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat", "door", "phone"), "", ""); resp.StatusCode != http.StatusOK {
				t.Fatalf("typed: %d", resp.StatusCode)
			}
			return mem
		}},
		{"too large", "too-large", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, true)
			f := answerForm(gateTool)
			f.Set("pad", strings.Repeat("x", mirrorAskFormMax+1))
			if resp, _ := answerReq(t, s, ts, f, "", ""); resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("too large: %d", resp.StatusCode)
			}
			return mem
		}},
		{"an unknown window", "not-found", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, true)
			f := answerForm(gateTool)
			f.Set("host", "nope")
			if resp, _ := answerReq(t, s, ts, f, "", ""); resp.StatusCode != http.StatusNotFound {
				t.Fatalf("unknown window: %d", resp.StatusCode)
			}
			return mem
		}},
		{"an unknown card (the registry lost on a restart)", "not-found", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, true)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusNotFound {
				t.Fatalf("unknown card: %d", resp.StatusCode)
			}
			return mem
		}},
		{"a read-only host", "read-only", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, false)
			register(t, ts)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("read-only: %d", resp.StatusCode)
			}
			return mem
		}},
		{"a stale sign-in (step-up)", "step-up", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, true)
			register(t, ts)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), staleCookieFor(s), ""); resp.StatusCode != http.StatusForbidden {
				t.Fatalf("stale: %d", resp.StatusCode)
			}
			return mem
		}},
		{"an invalid form", "invalid", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, true)
			register(t, ts)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Purple", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid: %d", resp.StatusCode)
			}
			return mem
		}},
		{"a closed card", "closed", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			am, s, ts, mem := attemptServer(t, true)
			register(t, ts)
			am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Cat"})
			region(t, s, ts, `data-ask-state="answered-terminal"`)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("closed: %d", resp.StatusCode)
			}
			return mem
		}},
		{"a host that cannot type", "unsupported", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			am, s, ts, mem := attemptServer(t, true)
			setAsk(am, func(a *askMirror) { a.noAnswer = true })
			register(t, ts)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("unsupported: %d", resp.StatusCode)
			}
			return mem
		}},
		{"another answer while one is typed", "in-progress", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := heldTyping(t)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Blue", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("in progress: %d", resp.StatusCode)
			}
			return mem
		}},
		{"the same answers again", "duplicate", card, sub, typed, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := heldTyping(t)
			if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusOK {
				t.Fatalf("duplicate: %d", resp.StatusCode)
			}
			return mem
		}},
		fail("not sent", "not-sent", fmt.Errorf("%w: %w", mirror.ErrAskNotSent, mirror.ErrHostOffline)),
		fail("the hub's step-up", "step-up", fmt.Errorf("%w: %w", mirror.ErrAskNotSent, mirror.ErrStepUpRequired)),
		fail("no picker", "no-picker", fmt.Errorf("%w: gone", mirror.ErrAskNoPicker)),
		fail("unreadable", "unreadable", fmt.Errorf("%w: clipped", mirror.ErrAskUnreadable)),
		fail("touched", "touched", fmt.Errorf("%w: someone", mirror.ErrAskTouched)),
		fail("lost", "lost", mirror.WithAskKeys(fmt.Errorf("%w: key 2", mirror.ErrAskLost), 1)),
		fail("unrecognised", "unrecognised", fmt.Errorf("%w: layout", mirror.ErrAskUnrecognised)),
		fail("busy", "busy", fmt.Errorf("%w: pane", mirror.ErrAskBusy)),
		fail("the host's unsupported", "unsupported", fmt.Errorf("%w: old", mirror.ErrAskUnsupported)),
		fail("timeout", "timeout", mirror.ErrAskTimeout),
		fail("anything else", "error", errors.New("boom")),

		// The dismissal.
		{"dismissed", "dismissed", card, dis, esc, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, true)
			register(t, ts)
			if resp, _ := cancelReq(t, s, ts, gateTool, "", ""); resp.StatusCode != http.StatusOK {
				t.Fatalf("dismissed: %d", resp.StatusCode)
			}
			return mem
		}},
		{"a dismissal while typing", "in-progress", card, dis, esc, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := heldTyping(t)
			if resp, _ := cancelReq(t, s, ts, gateTool, "", ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("dismiss while typing: %d", resp.StatusCode)
			}
			return mem
		}},
		{"a dismissal of a closed card", "closed", card, dis, esc, cc, func(t *testing.T) *askattempt.Mem {
			am, s, ts, mem := attemptServer(t, true)
			register(t, ts)
			am.feed(gateTool, gateQs(t), nil)
			region(t, s, ts, `data-ask-state="declined"`)
			if resp, _ := cancelReq(t, s, ts, gateTool, "", ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("dismiss closed: %d", resp.StatusCode)
			}
			return mem
		}},
		{"a dismissal the host refuses", "busy", card, dis, esc, cc, func(t *testing.T) *askattempt.Mem {
			am, s, ts, mem := attemptServer(t, true)
			setAsk(am, func(a *askMirror) { a.cancelErr = fmt.Errorf("%w: x", mirror.ErrAskBusy) })
			register(t, ts)
			if resp, _ := cancelReq(t, s, ts, gateTool, "", ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("dismiss refused: %d", resp.StatusCode)
			}
			return mem
		}},
		{"a dismissal with no tool", "invalid", card, dis, esc, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem := attemptServer(t, true)
			if resp, _ := cancelReq(t, s, ts, "", "", ""); resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("dismiss no tool: %d", resp.StatusCode)
			}
			return mem
		}},

		// Send as a message.
		{"sent as a message", "queued", card, sub, msg, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem, _ := messageServer(t, fmt.Errorf("%w: gone", mirror.ErrAskNoPicker))
			n := len(mem.All())
			if resp, body := postAsk(t, s, ts, ui.MirrorAskMessagePath, answerForm(gateTool), ""); resp.StatusCode != http.StatusOK {
				t.Fatalf("message: %d\n%s", resp.StatusCode, body)
			}
			return memFrom(mem, n)
		}},
		{"a message on a card that offers none", "not-offered", card, sub, msg, cc, func(t *testing.T) *askattempt.Mem {
			_, s, ts, mem, _ := messageServer(t, fmt.Errorf("%w: someone", mirror.ErrAskTouched))
			n := len(mem.All())
			if resp, _ := postAsk(t, s, ts, ui.MirrorAskMessagePath, answerForm(gateTool), ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("not offered: %d", resp.StatusCode)
			}
			return memFrom(mem, n)
		}},
		{"a message whose picker will not go", "dismiss-failed", card, sub, msg, cc, func(t *testing.T) *askattempt.Mem {
			am, s, ts, mem, _ := messageServer(t, fmt.Errorf("%w: clipped", mirror.ErrAskUnreadable))
			setAsk(am, func(a *askMirror) { a.cancelErr = fmt.Errorf("%w: x", mirror.ErrAskUnreadable) })
			n := len(mem.All())
			if resp, _ := postAsk(t, s, ts, ui.MirrorAskMessagePath, answerForm(gateTool), ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("dismiss failed: %d", resp.StatusCode)
			}
			return memFrom(mem, n)
		}},
		{"a message the queue refuses", "queue-refused", card, sub, msg, cc, func(t *testing.T) *askattempt.Mem {
			am, s, ts, mem, _ := messageServer(t, fmt.Errorf("%w: gone", mirror.ErrAskNoPicker))
			setAsk(am, func(a *askMirror) { a.statusReport = false })
			n := len(mem.All())
			if resp, _ := postAsk(t, s, ts, ui.MirrorAskMessagePath, answerForm(gateTool), ""); resp.StatusCode != http.StatusConflict {
				t.Fatalf("queue refused: %d", resp.StatusCode)
			}
			return memFrom(mem, n)
		}},
	}
}

// memFrom is a Mem holding mem's rows after the first n (the case's own).
func memFrom(mem *askattempt.Mem, n int) *askattempt.Mem {
	out := &askattempt.Mem{}
	for _, a := range rowsSince(mem, n) {
		_, _ = out.Record(authz.WithPrincipal(context.Background(), authz.Principal{UserID: a.Owner, Role: authz.RoleMember, Via: authz.ViaSession}), a)
	}
	return out
}

// openCodeCardCase is an opencode card's submit whose host answers err.
func openCodeCardCase(name, reason string, err error, form url.Values, prep func(m *ocFormMirror)) pathCase {
	return pathCase{name, reason, askattempt.DoorCard, askattempt.ActionSubmit, askattempt.MethodRelayed, askattempt.RuntimeOpenCode,
		func(t *testing.T) *askattempt.Mem {
			m := newOCFormMirror(false)
			m.err = err
			if prep != nil {
				prep(m)
			}
			s, ts := viewServer(t, m.fakeMirror)
			mem := &askattempt.Mem{}
			s.UseExtensions(Extensions{Mirror: m, Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal(),
				AskAttempts: mem})
			_, _ = answerReq(t, s, ts, form, "", "")
			return mem
		}}
}

func openCodeCases() []pathCase {
	good := answerForm(ocAskForm, "a0", "1", "a1", "0")
	cases := []pathCase{
		openCodeCardCase("relayed", "relayed", nil, good, nil),
		openCodeCardCase("not the ask waiting", "not-waiting", nil, answerForm("frm_other", "a0", "1", "a1", "0"), nil),
		openCodeCardCase("a host that sends no forms", "unanswerable", nil, good, func(m *ocFormMirror) { m.forms = false }),
		openCodeCardCase("an index past the options", "invalid", nil, answerForm(ocAskForm, "a0", "9", "a1", "0"), nil),
		openCodeCardCase("opencode moved on", "gone", fmt.Errorf("%w: x", mirror.ErrOpenCodeAskGone), good, nil),
		openCodeCardCase("unreachable", "unreachable", fmt.Errorf("%w: x", mirror.ErrOpenCodeUnreachable), good, nil),
		openCodeCardCase("answer unknown", "answer-unknown", fmt.Errorf("%w: x", mirror.ErrOpenCodeAnswerUnknown), good, nil),
		openCodeCardCase("answer refused", "answer-refused", fmt.Errorf("%w: x", mirror.ErrOpenCodeAnswerRefused), good, nil),
		openCodeCardCase("the host's step-up", "step-up", mirror.ErrStepUpRequired, good, nil),
		openCodeCardCase("the host's unsupported", "unsupported", mirror.ErrOpenCodeAnswerUnsupported, good, nil),
		openCodeCardCase("anything else", "error", errors.New("boom"), good, nil),
	}
	// The Attention list's own door.
	att := func(name, reason string, err error, cookie func(*Server) string) pathCase {
		return pathCase{name, reason, askattempt.DoorAttention, askattempt.ActionSubmit, askattempt.MethodRelayed, askattempt.RuntimeOpenCode,
			func(t *testing.T) *askattempt.Mem {
				m := &openCodeAskMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, windows: openCodeAnswerable()}, answers: true, err: err}
				s, ts := attentionServer(t, m)
				mem := &askattempt.Mem{}
				s.ext.AskAttempts = mem
				form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@2"}, "prompt": {"per_1a2aa6345001HPvxtQP9FD941m"}, "choice": {"reject"}}
				_, _ = postOpenCode(t, ts, cookie(s), ts.URL, form)
				return mem
			}}
	}
	return append(cases,
		att("relayed from Attention", "relayed", nil, sessionCookieFor),
		att("a stale sign-in at Attention", "step-up", nil, staleCookieFor),
		att("unreachable from Attention", "unreachable", fmt.Errorf("%w: x", mirror.ErrOpenCodeUnreachable), sessionCookieFor),
	)
}

// TestEveryAnswerPathRecordsOneRow: THE LEDGER. Each exit of each door —
// every refusal before typing, every outcome the host reports of typing, a
// dismissal's, a message's, an opencode relay's, from the card, the Attention
// list and the phone's shell — leaves exactly ONE row, with its reason, door,
// action, method and runtime. And the reasons those exits produce are EXACTLY
// askattempt.Reasons: a reason with no path here, or a path recording one not
// enumerated, fails it (the set grows or shrinks together).
func TestEveryAnswerPathRecordsOneRow(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range append(claudeCases(), openCodeCases()...) {
		t.Run(c.name, func(t *testing.T) {
			mem := c.run(t)
			rows := mem.All()
			if len(rows) != 1 {
				t.Fatalf("%d rows, want exactly 1: %+v", len(rows), rows)
			}
			a := rows[0]
			got := []string{a.Reason, a.Door, a.Action, a.Method, a.Runtime}
			want := []string{c.reason, c.door, c.action, c.method, c.runtime}
			if !slices.Equal(got, want) {
				t.Fatalf("row (reason, door, action, method, runtime) = %v, want %v", got, want)
			}
			if a.Outcome != askattempt.OutcomeOf(c.reason) || a.Owner != authz.OperatorUserID {
				t.Fatalf("row %+v", a)
			}
			seen[a.Reason] = true
		})
	}
	var got []string
	for r := range seen {
		got = append(got, r)
	}
	sort.Strings(got)
	if want := askattempt.ReasonCodes(); !slices.Equal(got, want) {
		t.Fatalf("the reasons the doors record\n got %v\nwant askattempt.Reasons %v", got, want)
	}
}

// TestEveryDoorExitNamesItsReason: the structural half of the ledger. In each
// door, every return after the attempt begins is preceded — in its own block
// or one enclosing it — by att.end, att.failed, or the hand-off to the
// opencode door (which names its own), so a NEW exit that forgets is caught
// even before a test drives it. (done records a forgotten one as "error".)
func TestEveryDoorExitNamesItsReason(t *testing.T) {
	doors := map[string][]string{
		"mirror_ask.go":          {"handleMirrorAskAnswer", "handleMirrorAskCancel"},
		"ask_message.go":         {"handleMirrorAskMessage"},
		"mirror_opencode_ask.go": {"handleOpenCodeAskAnswer"},
		"attention_opencode.go":  {"handleAttentionOpenCodeAnswer"},
	}
	n := 0
	for file, fns := range doors {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || !slices.Contains(fns, fn.Name.Name) {
				continue
			}
			n++
			for _, why := range unnamedExits(fn) {
				t.Errorf("%s: %s: %s", file, fn.Name.Name, why)
			}
		}
	}
	if n != 5 {
		t.Fatalf("found %d of the 5 doors", n)
	}
}

// names reports whether stmt names the attempt's reason.
func names(stmt ast.Stmt) bool {
	es, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, _ := sel.X.(*ast.Ident)
	return id != nil && ((id.Name == "att" && (sel.Sel.Name == "end" || sel.Sel.Name == "failed")) ||
		(id.Name == "s" && sel.Sel.Name == "handleOpenCodeAskAnswer"))
}

// unnamedExits is every return of fn, after its attempt begins (an `att :=`,
// or fn's att parameter), that no statement before it — in its block or one
// enclosing it — names a reason for.
func unnamedExits(fn *ast.FuncDecl) []string {
	var out []string
	begun := false
	for _, p := range fn.Type.Params.List {
		for _, n := range p.Names {
			if n.Name == "att" {
				begun = true
			}
		}
	}
	var walk func(list []ast.Stmt, named bool)
	walk = func(list []ast.Stmt, named bool) {
		for _, st := range list {
			if as, ok := st.(*ast.AssignStmt); ok {
				for _, l := range as.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name == "att" {
						begun = true
					}
				}
			}
			if names(st) {
				named = true
			}
			switch x := st.(type) {
			case *ast.ReturnStmt:
				if begun && !named {
					out = append(out, fmt.Sprintf("a return at offset %d names no reason", x.Pos()))
				}
			case *ast.IfStmt:
				walk(x.Body.List, named)
				if x.Else != nil {
					if b, ok := x.Else.(*ast.BlockStmt); ok {
						walk(b.List, named)
					} else {
						walk([]ast.Stmt{x.Else}, named)
					}
				}
			case *ast.SwitchStmt:
				// A switch with a default whose every clause names a reason
				// names one for what follows it.
				all, dflt := true, false
				for _, c := range x.Body.List {
					cc := c.(*ast.CaseClause)
					walk(cc.Body, named)
					dflt = dflt || cc.List == nil
					all = all && slices.ContainsFunc(cc.Body, names)
				}
				named = named || (all && dflt)
			case *ast.ForStmt:
				walk(x.Body.List, named)
			case *ast.RangeStmt:
				walk(x.Body.List, named)
			case *ast.BlockStmt:
				walk(x.List, named)
			}
		}
	}
	walk(fn.Body.List, false)
	return out
}

// noTextQuestions is a question set whose every text is a distinctive string:
// none may reach a row.
const noTextMarker = "quokka7731"

func noTextInput() json.RawMessage {
	return json.RawMessage(`{"questions":[{"question":"Which ` + noTextMarker + ` way?","header":"H` + noTextMarker + `","options":[` +
		`{"label":"A` + noTextMarker + `","description":"d` + noTextMarker + `"},{"label":"B` + noTextMarker + `","description":"d"}],"multiSelect":false}]}`)
}

// TestNoRowHoldsTheQuestionOrTheAnswer: a question whose header, text,
// options and descriptions all carry a marker, answered with an answer of
// one's own carrying it too — typed, failed (with a pane that shows none of
// it), retried, dismissed and sent as a message: no field of any row holds
// the marker. The control: the pane the host sends DOES show it, and only the
// snapshot holds it — the documented exception.
func TestNoRowHoldsTheQuestionOrTheAnswer(t *testing.T) {
	am, s, ts, mem := attemptServer(t, true)
	q := &memQueue{}
	s.ext.SendQueue = q
	setAsk(am, func(a *askMirror) {
		a.statusReport = true
		a.windows[0].Status, a.windows[0].StatusSince = mirror.StatusWaiting, 1792000001111
	})
	const tool = "toolu_notext"
	if code := openAsk(t, ts, "mmt_ws", "sess-1", tool, noTextInput()); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	region(t, s, ts, `data-ask-tool="`+tool+`"`)
	own := answerForm(tool, "a0", askq.OtherValue, "o0", "Mine "+noTextMarker)
	pick := answerForm(tool, "a0", "A"+noTextMarker)
	setAsk(am, func(a *askMirror) { a.typeErr = fmt.Errorf("%w: gone", mirror.ErrAskNoPicker) })
	_, _ = answerReq(t, s, ts, own, "", "")
	_, _ = answerReq(t, s, ts, pick, "", "")
	if resp, body := postAsk(t, s, ts, ui.MirrorAskMessagePath, answerForm(tool), ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("message: %d\n%s", resp.StatusCode, body)
	}
	if len(q.items) != 1 || !strings.Contains(q.items[0].Body, noTextMarker) {
		t.Fatalf("control: the message queued is %+v (it must carry the answer)", q.items)
	}
	_, _ = cancelReq(t, s, ts, tool, "", "")
	rows := mem.All()
	if len(rows) != 4 {
		t.Fatalf("%d rows, want 4", len(rows))
	}
	for _, a := range rows {
		if b, _ := json.Marshal(a); strings.Contains(string(b), noTextMarker) {
			t.Fatalf("a row holds the question's or answer's text: %s", b)
		}
	}
	if !rows[0].Shape.Other || rows[1].Shape.Other {
		t.Fatalf("the shape's Other: %v %v", rows[0].Shape, rows[1].Shape)
	}
	// The exception: a pane that shows the question is kept, in the snapshot
	// only.
	setAsk(am, func(a *askMirror) {
		a.typeErr = mirror.WithAskScreen(fmt.Errorf("%w: someone", mirror.ErrAskTouched), "❯ 1. A"+noTextMarker)
	})
	const tool2 = "toolu_notext2"
	if code := openAsk(t, ts, "mmt_ws", "sess-1", tool2, noTextInput()); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	region(t, s, ts, `data-ask-tool="`+tool2+`"`)
	_, _ = answerReq(t, s, ts, answerForm(tool2, "a0", "A"+noTextMarker), "", "")
	last := mem.All()[len(mem.All())-1]
	if !strings.Contains(last.Snapshot, noTextMarker) {
		t.Fatalf("control: the snapshot %q lost the pane", last.Snapshot)
	}
	last.Snapshot = ""
	if b, _ := json.Marshal(last); strings.Contains(string(b), noTextMarker) {
		t.Fatalf("outside the snapshot, a row holds the text: %s", b)
	}
}

// TestTheSnapshotIsTheHostsPaneRedacted: the pane the host sends with a
// refusal reaches the row redacted.
func TestTheSnapshotIsTheHostsPaneRedacted(t *testing.T) {
	secret := "sk_live_" + "FAKE0000example"
	am, s, ts, mem := attemptServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	setAsk(am, func(a *askMirror) {
		a.typeErr = mirror.WithAskScreen(fmt.Errorf("%w: x", mirror.ErrAskUnrecognised), "export KEY="+secret+"\n❯ 1. Red")
	})
	_, _ = answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", "")
	rows := mem.All()
	if len(rows) != 1 || strings.Contains(rows[0].Snapshot, secret) || !strings.Contains(rows[0].Snapshot, "❯ 1. Red") {
		t.Fatalf("snapshot %+v", rows)
	}
}

// TestARetryResetsThePickerAndIsAttemptTwo: a recoverable failure (lost, with
// nothing recorded to continue) offers Try again; the retry hands the host
// Reset — re-read the picker, put it back to its first option — and is
// recorded as attempt 2 of the card. An unrecoverable one (touched) offers no
// Try again.
func TestARetryResetsThePickerAndIsAttemptTwo(t *testing.T) {
	am, s, ts, mem := attemptServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	setAsk(am, func(a *askMirror) { a.typeErr = fmt.Errorf("%w: key 1", mirror.ErrAskLost) })
	resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", "")
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "Try again") || !strings.Contains(body, `href="`+answersCardPath(gateTool)+`"`) {
		t.Fatalf("the failed card: %d\n%s", resp.StatusCode, body)
	}
	setAsk(am, func(a *askMirror) { a.typeErr = nil })
	if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the retry: %d", resp.StatusCode)
	}
	am.amu.Lock()
	priors := append([]*askq.Progress(nil), am.priors...)
	am.amu.Unlock()
	if len(priors) != 2 || priors[0] != nil || !reflect.DeepEqual(priors[1], &askq.Progress{Reset: true}) {
		t.Fatalf("the host was handed %+v", priors)
	}
	rows := mem.All()
	if len(rows) != 2 || rows[0].Number != 1 || rows[1].Number != 2 || rows[1].Reason != "typed" || rows[0].Reason != "lost" {
		t.Fatalf("rows %+v", rows)
	}
	// Unrecoverable: no Try again.
	am2, s2, ts2, _ := attemptServer(t, true)
	register(t, ts2)
	region(t, s2, ts2, `data-ask-state="pending"`)
	setAsk(am2, func(a *askMirror) { a.typeErr = fmt.Errorf("%w: someone", mirror.ErrAskTouched) })
	if _, body := answerReq(t, s2, ts2, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); strings.Contains(body, "Try again") || strings.Contains(body, "Send as a message") {
		t.Fatalf("a touched card offers a recovery:\n%s", body)
	}
}

// TestSendAsAMessageDismissesThenQueuesBehindTheStepUp: a card whose picker
// is gone offers "Send as a message"; on a stale sign-in it is refused (403 +
// X-Muster-Step-Up), nothing dismissed or queued; on a fresh one the picker
// is dismissed FIRST (unreadable: it may still be there), then the answer
// muster was typing is queued for the window as "Answer to “Header”: …", and
// the row is method message, door card.
func TestSendAsAMessageDismissesThenQueuesBehindTheStepUp(t *testing.T) {
	am, s, ts, mem, q := messageServer(t, fmt.Errorf("%w: clipped", mirror.ErrAskUnreadable))
	_, card := mirrorGet(t, s, ts, asksPath)
	if !strings.Contains(card, "Send as a message") || !strings.Contains(card, `action="`+ui.MirrorAskMessagePath+`"`) {
		t.Fatalf("the failed card offers no Send as a message:\n%s", card)
	}
	n := len(mem.All())
	resp, _ := postAsk(t, s, ts, ui.MirrorAskMessagePath, answerForm(gateTool), staleCookieFor(s))
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Muster-Step-Up") == "" || len(am.cancelsSoFar()) != 0 || len(q.items) != 0 {
		t.Fatalf("stale: %d, %d dismissals, %d queued", resp.StatusCode, len(am.cancelsSoFar()), len(q.items))
	}
	if r := rowsSince(mem, n); len(r) != 1 || r[0].Reason != "step-up" || r[0].Method != askattempt.MethodMessage {
		t.Fatalf("the stale row: %+v", r)
	}
	resp, body := postAsk(t, s, ts, ui.MirrorAskMessagePath, answerForm(gateTool), "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Sent as a message") || strings.Contains(body, `action="`+ui.MirrorAskMessagePath+`"`) {
		t.Fatalf("fresh (sent, and not offered again): %d\n%s", resp.StatusCode, body)
	}
	if _, card := mirrorGet(t, s, ts, asksPath); strings.Contains(card, `action="`+ui.MirrorAskMessagePath+`"`) || !strings.Contains(card, "Sent as a message") {
		t.Fatalf("the next poll offers it again:\n%s", card)
	}
	if got := am.cancelsSoFar(); len(got) != 1 || got[0] != "ws main @1" {
		t.Fatalf("dismissals %v", got)
	}
	want := "Answer to “Color”: Red\nAnswer to “Pets”: Cat, Fish"
	if len(q.items) != 1 || q.items[0].Body != want || q.items[0].Target != (sendqueue.Target{Host: "ws", Session: "main", Window: "@1"}) {
		t.Fatalf("queued %+v, want %q", q.items, want)
	}
	if r := rowsSince(mem, n); len(r) != 2 || r[1].Reason != "queued" || r[1].Method != askattempt.MethodMessage || r[1].Door != askattempt.DoorCard || r[1].Number != 3 {
		t.Fatalf("the message's row: %+v", r)
	}
}

// TestTheMessageNamesAQuestionWithoutAHeaderByItsFirstWords.
func TestTheMessageNamesAQuestionWithoutAHeaderByItsFirstWords(t *testing.T) {
	qs := []askq.Question{{Question: "Should we ship the migration tonight or wait until the morning window opens?"}, {Header: "Unanswered", Question: "x"}}
	got := askMessageText(qs, map[string]string{qs[0].Question: "Wait"})
	if want := "Answer to “Should we ship the migration tonight or wait …”: Wait"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestThePruneRunsFromTheAttentionSweepHourly: a row past the TTL goes; a
// second sweep within the hour does not prune again.
func TestThePruneRunsFromTheAttentionSweepHourly(t *testing.T) {
	_, s, _, mem := attemptServer(t, true)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	old := askattempt.Attempt{At: time.Now().Add(-askattempt.RowTTL - time.Hour), Runtime: askattempt.RuntimeClaudeCode, Door: askattempt.DoorCard,
		Action: askattempt.ActionSubmit, Method: askattempt.MethodTyped, Reason: "typed"}
	if _, err := mem.Record(ctx, old); err != nil {
		t.Fatal(err)
	}
	s.pruneAskAttempts(context.Background())
	if n := len(mem.All()); n != 0 {
		t.Fatalf("%d rows after the prune", n)
	}
	if _, err := mem.Record(ctx, old); err != nil {
		t.Fatal(err)
	}
	s.pruneAskAttempts(context.Background())
	if n := len(mem.All()); n != 1 {
		t.Fatalf("a second prune within the hour ran: %d rows", n)
	}
}
