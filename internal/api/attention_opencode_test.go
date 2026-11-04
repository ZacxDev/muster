package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// A WAITING OPENCODE WINDOW'S ANSWER, in the attention queue.

// openCodeAskMirror is fakeMirror's host "ws" listing opencode windows,
// whose host relays answers when answers is set; it records each
// AnswerOpenCodeAsk and answers err.
type openCodeAskMirror struct {
	*fakeMirror
	answers bool
	mu      sync.Mutex
	calls   []string
	err     error
}

func (m *openCodeAskMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := m.fakeMirror.Hosts(ctx)
	for i := range hs {
		hs[i].OpenCodeAnswer = m.answers && hs[i].Online && hs[i].Interactive
	}
	return hs
}

func (m *openCodeAskMirror) AnswerOpenCodeAsk(ctx context.Context, host, session, window, promptID, choice string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, strings.Join([]string{host, session, window, promptID, choice}, " "))
	return m.err
}

func (m *openCodeAskMirror) answered() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// openCodeAnswerable: @1 waits on a one-question form muster can answer, @2
// on a permission request, @3 on a two-question form (no options).
func openCodeAnswerable() []mirror.Window {
	ws := openCodeWaiting()
	ws[0].Pending = &mirror.Pending{Kind: "question", Tool: "question", Summary: "Proceed?", PromptID: "frm_1a2aaa3d1001lEte0g1lyG1pFi", Options: "Yes\nNo"}
	ws[2] = mirror.Window{Session: "main", ID: "@3", Runtime: mirror.RuntimeOpenCode, AgentSession: askOpenCodeSID, Status: mirror.StatusWaiting,
		StatusSince: 1792000003333, Pending: &mirror.Pending{Kind: "question", Tool: "question", Summary: "Which database? (and 1 more question)"}}
	return ws
}

// A question muster can answer pushes its options, as a Claude question
// does; its card offers them — and Allow once / Reject for a permission,
// never "always" — only when the host relays. Otherwise the card SAYS the
// answer cannot be relayed from here; and an ask muster cannot answer at all
// says to answer it in the terminal, whatever the host.
func TestAnOpenCodeAskOffersItsAnswersOnlyWhenItsHostRelays(t *testing.T) {
	for _, answers := range []bool{true, false} {
		m := &openCodeAskMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, windows: openCodeAnswerable()}, answers: answers}
		s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{Mirror: m})
		by := itemsByWindow(s.waitingSessions(hermesUser()))
		q, p, many := by["@1"], by["@2"], by["@3"]
		if q.Kind != attentionKindQuestion || strings.Join(q.choices, ",") != "Yes,No" {
			t.Fatalf("the question: %+v", q)
		}
		if n := attentionNotification(q, false); n.Body != "Proceed?\nOptions: Yes · No" {
			t.Fatalf("the question's push body %q", n.Body)
		}
		qh := attentionItemsHTML([]attentionItem{q}, time.Now())
		ph := attentionItemsHTML([]attentionItem{p}, time.Now())
		mh := attentionItemsHTML([]attentionItem{many}, time.Now())
		buttons := strings.Contains(qh, `data-opencode-choice="Yes"`) && strings.Contains(qh, `data-opencode-choice="No"`) &&
			strings.Contains(ph, `data-opencode-choice="once"`) && strings.Contains(ph, `data-opencode-choice="reject"`)
		if buttons != answers {
			t.Errorf("answers=%v: buttons drawn = %v:\n%s\n%s", answers, buttons, qh, ph)
		}
		if !answers && (!strings.Contains(qh, html_(openCodeNoRelayText)) || !strings.Contains(ph, html_(openCodeNoRelayText)) || strings.Contains(ph, "<form")) {
			t.Errorf("no buttons, and no word that the answer cannot be relayed:\n%s\n%s", qh, ph)
		}
		if strings.Contains(ph, `value="always"`) {
			t.Error("offered always")
		}
		if !strings.Contains(mh, html_(openCodeTerminalText)) || strings.Contains(mh, "<form") {
			t.Errorf("a form muster cannot answer:\n%s", mh)
		}
	}
	// An offline host's ask says so rather than offering what cannot arrive.
	m := &openCodeAskMirror{fakeMirror: &fakeMirror{online: false, offlineSince: time.Now(), interactive: true, statusReport: true, windows: openCodeAnswerable()}, answers: true}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Mirror: m})
	for _, it := range s.waitingSessions(hermesUser()) {
		if h := attentionItemsHTML([]attentionItem{it}, time.Now()); strings.Contains(h, "<form") {
			t.Errorf("an offline host's ask offers an answer:\n%s", h)
		}
	}
}

func html_(s string) string { return strings.ReplaceAll(s, "'", "&#39;") }

func postOpenCode(t *testing.T, ts *httptest.Server, cookie, origin string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+attentionOpenCodePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", origin)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// 🔴 The answer passes the gates of the Claude ask-answer path, each before
// the plugin is asked; a good one reaches it and comes back to the queue
// saying opencode took it; the plugin's refusals read as what happened.
func TestAnOpenCodeAnswerIsGated(t *testing.T) {
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@2"}, "prompt": {"per_1a2aa6345001HPvxtQP9FD941m"}, "choice": {"reject"}}
	for _, c := range []struct {
		name   string
		stale  bool
		origin string
		window string
		err    error
		status int
		asked  bool
		says   string
	}{
		{"another origin", false, "https://evil.example", "@2", nil, http.StatusForbidden, false, ""},
		{"a stale sign-in", true, "", "@2", nil, http.StatusForbidden, false, ""},
		{"a window that is not listed", false, "", "@9", nil, http.StatusNotFound, false, ""},
		{"fresh, same origin", false, "", "@2", nil, http.StatusSeeOther, true, ""},
		{"the host does not relay", false, "", "@2", mirror.ErrOpenCodeAnswerUnsupported, http.StatusConflict, true, openCodeNoRelayText},
		{"answered in the terminal", false, "", "@2", mirror.ErrOpenCodeAskGone, http.StatusConflict, true, openCodeGoneText},
		{"unknown outcome", false, "", "@2", mirror.ErrOpenCodeAnswerUnknown, http.StatusGatewayTimeout, true, openCodeUnknownText},
		{"opencode refused the value", false, "", "@2", mirror.ErrOpenCodeAnswerRefused, http.StatusUnprocessableEntity, true, openCodeRefusedText},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &openCodeAskMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, windows: openCodeAnswerable()}, answers: true, err: c.err}
			s, ts := hermesServer(t, m)
			cookie := sessionCookieFor(s)
			if c.stale {
				cookie = staleCookieFor(s)
			}
			origin := ts.URL
			if c.origin != "" {
				origin = c.origin
			}
			f := url.Values{}
			for k, v := range form {
				f[k] = v
			}
			f.Set("window", c.window)
			resp, body := postOpenCode(t, ts, cookie, origin, f)
			if resp.StatusCode != c.status || (len(m.answered()) == 1) != c.asked {
				t.Fatalf("%d %q, asked %v", resp.StatusCode, body, m.answered())
			}
			if c.asked {
				if got := m.answered()[0]; got != "ws main @2 per_1a2aa6345001HPvxtQP9FD941m reject" {
					t.Fatalf("asked %q", got)
				}
			}
			if c.status == http.StatusSeeOther {
				if loc := resp.Header.Get("Location"); loc != attentionPath+"?answered=opencode" {
					t.Fatalf("redirect to %q", loc)
				}
				_, page := mirrorGet(t, s, ts, attentionPath+"?answered=opencode")
				if !strings.Contains(page, html_(openCodeAnsweredText)) {
					t.Fatalf("the queue does not say it was answered:\n%s", page)
				}
			}
			if c.says != "" && !strings.Contains(body, html_(c.says)) {
				t.Fatalf("the page does not say %q:\n%s", c.says, body)
			}
		})
	}
}
