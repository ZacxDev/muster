package api

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// TestTheQueueSortsNewestFirstWithAStableTieBreak: newest Since first; equal
// times by Key (whatever order the sources were read in); an item with no time
// last.
func TestTheQueueSortsNewestFirstWithAStableTieBreak(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, in := range [][]attentionItem{
		{{Key: "ask-1", Since: t0}, {Key: "session-b", Since: t0.Add(time.Minute)}, {Key: "draft-9", Since: t0}, {Key: "session-z"}, {Key: "ask-2", Since: t0.Add(2 * time.Minute)}},
		{{Key: "session-z"}, {Key: "draft-9", Since: t0}, {Key: "ask-2", Since: t0.Add(2 * time.Minute)}, {Key: "ask-1", Since: t0}, {Key: "session-b", Since: t0.Add(time.Minute)}},
	} {
		sortAttentionItems(in)
		var got []string
		for _, it := range in {
			got = append(got, it.Key)
		}
		if want := "ask-2 session-b ask-1 draft-9 session-z"; strings.Join(got, " ") != want {
			t.Fatalf("sorted %v, want %s", got, want)
		}
	}
}

// TestAHeldQuestionListsEveryQuestionCompactly: one question is itself; several
// are each one's first line, numbered, one per line.
func TestAHeldQuestionListsEveryQuestionCompactly(t *testing.T) {
	one := askq.Entry{Questions: []askq.Question{{Question: "Which database?\nWe need one."}}}
	if got := heldQuestionText(one); got != "Which database?\nWe need one." {
		t.Fatalf("one question: %q", got)
	}
	many := askq.Entry{Questions: []askq.Question{{Question: "Which database?\nWe need one."}, {Question: "  Migrate\tnow?  "}, {Question: strings.Repeat("x", 200)}}}
	want := "1. Which database?\n2. Migrate now?\n3. " + strings.Repeat("x", heldQuestionLineMax-1) + "…"
	if got := heldQuestionText(many); got != want {
		t.Fatalf("several questions: %q\nwant %q", got, want)
	}
	if got := heldQuestionText(askq.Entry{}); got != sessionWaitingText {
		t.Fatalf("no question: %q", got)
	}
}

// TestADraftsNotificationNamesWhoAndWhereOnly: "Reply draft: <name>
// (<plugin>)" — the WHOLE body pinned — with the name only for a sender the
// plugin verified; never a word of the message or the draft.
func TestADraftsNotificationNamesWhoAndWhereOnly(t *testing.T) {
	for _, c := range []struct {
		name  string
		m     plugins.Message
		label string
		want  string
	}{
		{"verified, display name", plugins.Message{ID: 7, Sender: "Justin <justin@example.test>", SenderVerified: true, Body: "the secret plan"}, "Discord DMs", "Reply draft: Justin (Discord DMs)"},
		{"verified, bare handle", plugins.Message{ID: 7, Sender: "justin#0420", SenderVerified: true, Body: "hi"}, "Discord DMs", "Reply draft: justin#0420 (Discord DMs)"},
		{"verified, agent-mail address", plugins.Message{ID: 7, Sender: "example-owner/agent-infra", SenderVerified: true, Body: "hi"}, "Agent mail", "Reply draft: example-owner/agent-infra (Agent mail)"},
		{"UNVERIFIED agent-mail address: not shown", plugins.Message{ID: 7, Sender: "example-owner/agent-infra", Body: "hi"}, "Agent mail", "Reply draft: unverified sender (Agent mail)"},
		{"verified, quoted name", plugins.Message{ID: 7, Sender: `"Ada L" <ada@example.test>`, SenderVerified: true}, "Mail", "Reply draft: Ada L (Mail)"},
		{"UNVERIFIED: the claimed name is not shown", plugins.Message{ID: 7, Sender: "Your Bank <alerts@example.test>", Body: "reset now"}, "Mail", "Reply draft: unverified sender (Mail)"},
		{"control characters in a name", plugins.Message{ID: 7, Sender: "Ev\x1b[2Jil\u202E", SenderVerified: true}, "Mail", "Reply draft: Ev [2Jil (Mail)"},
		{"no plugin label", plugins.Message{ID: 7, Sender: "Justin", SenderVerified: true}, "", "Reply draft: Justin"},
		{"no message", plugins.Message{}, "Mail", DraftPushBody},
	} {
		if got := draftPushBody(c.m, c.label); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestADraftsAttentionLineMarksAnUnverifiedSender pins the WHOLE line a draft's
// attention item shows — to the operator and, through attention_list, to the
// chief: an unverified sender is followed by "(unverified sender)", the push's
// wording; a verified one is not.
func TestADraftsAttentionLineMarksAnUnverifiedSender(t *testing.T) {
	for _, c := range []struct {
		name string
		m    plugins.Message
		want string
	}{
		{"verified", plugins.Message{ID: 7, Sender: "Ada <ada@example.test>", SenderVerified: true, Body: "lunch?"}, "Ada <ada@example.test>: lunch?"},
		{"unverified", plugins.Message{ID: 7, Sender: "Your Bank <alerts@example.test>", Body: "reset now"}, "Your Bank <alerts@example.test> (unverified sender): reset now"},
		{"unverified, no sender", plugins.Message{ID: 7, Subject: "s"}, "unknown sender (unverified sender): s"},
	} {
		if got := draftInboundText(c.m); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestEveryAttentionPushBodyCarriesItsSubstance: operator decision "Full text
// always" — for ASKS AND HELD QUESTIONS only (it replaced "Summary only — WHO
// and WHERE, never WHAT" for them). An ask's body is its question, on one line,
// then its options; a held question's is its question(s) and one question's
// choices; a secret-shaped token is redacted. A WAITING SESSION stays summary
// only (operator decision): "Waiting: <tool> · <window> @ <host>", or without
// the tool — never the command summary, never Claude's last words. Whole bodies
// pinned. RED at origin/main for the asks; the session rows are RED at the
// first #313 head (which pushed the summary and the last words).
func TestEveryAttentionPushBodyCarriesItsSubstance(t *testing.T) {
	for _, c := range []struct {
		name string
		it   attentionItem
		want string
	}{
		{"an ask with options", attentionItem{Kind: attentionKindAsk, Source: "deploy-bot", Text: "Deploy the importer\n\nto production   now?",
			Options: []string{"deploy now", "wait until Monday"}}, "Deploy the importer to production now?\nOptions: deploy now · wait until Monday"},
		{"an ask without", attentionItem{Kind: attentionKindAsk, Source: "chief", Text: "Rotate the staging key?"}, "Rotate the staging key?"},
		{"an ask quoting a token", attentionItem{Kind: attentionKindAsk, Source: "ci", Text: "Use ghp_0123abcd9 for the mirror?"}, "Use «redacted» for the mirror?"},
		{"a held question", attentionItem{Kind: attentionKindQuestion, Source: "main:1 @ ws", Text: "1. Which branch?\n2. Push it?"}, "1. Which branch?\n2. Push it?"},
		{"a held question's choices", attentionItem{Kind: attentionKindQuestion, Source: "main:1 @ ws", Text: "Which branch?", choices: []string{"main", "dev"}},
			"Which branch?\nOptions: main · dev"},
		{"a waiting session", attentionItem{Kind: attentionKindSession, Source: "main:1 @ ws", Text: "Bash: curl -H 'Authorization: x' https://x.test", tool: "Bash",
			Detail: "Tests pass.\nPublishing now."}, "Waiting: Bash · main:1 @ ws"},
		{"an MCP tool", attentionItem{Kind: attentionKindSession, Source: "w:2 @ lap", Text: "MCP muster/attention_ask", tool: "MCP muster/attention_ask"}, "Waiting: MCP muster/attention_ask · w:2 @ lap"},
		{"a host that names no tool", attentionItem{Kind: attentionKindSession, Source: "main:1 @ ws", Text: sessionWaitingText}, "Waiting: main:1 @ ws"},
		{"a markdown window title", attentionItem{Kind: attentionKindSession, Source: "**Fix** [auth](https://example.com/a) for C# @ host-b", tool: "Edit"}, "Waiting: Edit · Fix auth for C# @ host-b"},
	} {
		if got := attentionPushBody(c.it); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestTheAskPushTitleNamesTheAsker: WHO is in an ask's title — "<asker> is
// asking"; a waiting session keeps its fixed title (its WHO is in its body).
// RED at origin/main for the asks ("An agent needs your input", the same for
// every asker).
func TestTheAskPushTitleNamesTheAsker(t *testing.T) {
	for _, c := range []struct {
		it   attentionItem
		want string
	}{
		{attentionItem{Kind: attentionKindAsk, Source: "chief"}, "chief is asking"},
		{attentionItem{Kind: attentionKindAsk, Source: "muster"}, "muster is asking"},
		{attentionItem{Kind: attentionKindQuestion, Source: "main:1 @ ws"}, "main:1 @ ws is asking"},
		{attentionItem{Kind: attentionKindSession, Source: "main:1 @ ws"}, "A session is waiting on you"},
	} {
		if got := attentionPushTitle(c.it); got != c.want {
			t.Errorf("%s: title %q, want %q", c.it.Kind, got, c.want)
		}
	}
}

// TestALongAskBodyIsCutOnACharacterAndKeepsItsOptions: a question longer than
// the body is cut — on a character boundary (a skin-toned emoji straddling the
// cut goes whole), with a visible "…" — to leave room for the options line,
// and the whole body is attentionPushBodyMax characters. The control is a
// question that fits: untouched, no ellipsis. RED at origin/main.
func TestALongAskBodyIsCutOnACharacterAndKeepsItsOptions(t *testing.T) {
	opts := "Options: yes · no"
	room := attentionPushBodyMax - utf8.RuneCountInString(opts) - 1
	// The cut (room-1 runes, then "…") falls INSIDE the 👍🏽 cluster.
	pad := strings.Repeat("q", room-2)
	it := attentionItem{Kind: attentionKindAsk, Source: "chief", Text: pad + "\U0001F44D\U0001F3FD" + strings.Repeat(" more", 100), Options: []string{"yes", "no"}}
	got := attentionPushBody(it)
	want := pad + "…\n" + opts
	if got != want {
		t.Fatalf("body = %q…%q, want %q…%q", got[:10], got[len(got)-30:], want[:10], want[len(want)-30:])
	}
	if n := utf8.RuneCountInString(got); n > attentionPushBodyMax {
		t.Fatalf("the body is %d characters, over %d", n, attentionPushBodyMax)
	}
	short := attentionItem{Kind: attentionKindAsk, Source: "chief", Text: "Ship it?", Options: []string{"yes", "no"}}
	if got := attentionPushBody(short); got != "Ship it?\n"+opts {
		t.Fatalf("CONTROL: a short question was changed: %q", got)
	}
}

// TestTheAskBodySurvivesTheAppsCut: the Android app as shipped cuts a body at
// 200 characters (muster-android PayloadV2.MAX_BODY), so the body is at most
// that long — the phone shows exactly the lines the browser does — and a long
// question is cut to leave the options line whole within it. Measured at two
// lengths: a question far over (1000 runes) and one just over the room.
// RED at the first #313 head, whose 400-character body lost its options to the
// app's cut.
func TestTheAskBodySurvivesTheAppsCut(t *testing.T) {
	const appMaxBody = 200 // muster-android PayloadV2.MAX_BODY
	for _, n := range []int{1000, appMaxBody - 20} {
		it := attentionItem{Kind: attentionKindAsk, Source: "chief", Text: strings.Repeat("q", n), Options: []string{"ship it", "wait for Monday"}}
		body := attentionPushBody(it)
		if r := utf8.RuneCountInString(body); r > appMaxBody {
			t.Errorf("a %d-rune question: the body is %d runes, over the app's %d", n, r, appMaxBody)
		}
		if !strings.HasSuffix(push.CutClusters(body, appMaxBody), "\nOptions: ship it · wait for Monday") {
			t.Errorf("a %d-rune question: the app's cut loses the options line: %q", n, body[len(body)-40:])
		}
	}
}

// TestASecretShapedChipIsNotSentToThePhone: an option (or suggestion) a secret
// scan would redact is left out of payload v2's chips and buttons — a chip is
// sent whole or not at all, and a redacted one would answer with the wrong text
// — while the body's options line shows it redacted. The control is the clean
// option beside it, which stays. RED at the first #313 head (chips raw).
func TestASecretShapedChipIsNotSentToThePhone(t *testing.T) {
	it := attentionItem{Key: "ask-5", Kind: attentionKindAsk, AskID: 5, Source: "ci", Text: "Which token?",
		Options: []string{"use ghp_0123abcd9", "neither"}, suggestions: []string{"take sk-live-abcdef123456", "ask ops first"}}
	n := attentionNotification(it, false)
	v := push.Payload{Type: n.Type, Title: n.Title, Body: n.Body, Ask: n.Ask}.V2()
	if strings.Join(v.Ask.Options, "|") != "neither" || strings.Join(v.Ask.Suggestions, "|") != "ask ops first" {
		t.Fatalf("chips = %q / %q", v.Ask.Options, v.Ask.Suggestions)
	}
	if strings.Contains(n.Body, "ghp_") || !strings.Contains(n.Body, "use «redacted»") {
		t.Fatalf("the body's options line: %q", n.Body)
	}
}

// TestWhoseWaitingWindowsTheQueueBelieves: an online host only when it reports
// status; a dropped host only within the grace — the rule the view and the
// push diff share.
func TestWhoseWaitingWindowsTheQueueBelieves(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		h    mirror.HostView
		want bool
	}{
		{"online, reports status", mirror.HostView{Online: true, StatusReport: true}, true},
		{"online, no status", mirror.HostView{Online: true}, false},
		{"dropped just now", mirror.HostView{OfflineSince: now.Add(-time.Second)}, true},
		{"dropped at the grace", mirror.HostView{OfflineSince: now.Add(-attentionOfflineGrace)}, false},
		{"offline, never seen drop", mirror.HostView{}, false},
	} {
		if got := attentionHostBelieved(c.h, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestOnlyTheOperatorsSendsAnswerAsksInSession is the SEAM LEDGER: the exact
// set of calls that may answer an ask in session, pinned by caller AND by the
// paste TARGET each mirrored one names. Each is a door the OPERATOR writes
// through: the rendered view's Send (its own target — mirrorAddressed answers
// only mirror.PasteClaude, so a Terminal send to the active pane does not),
// the send queue's add and its Retry of a failed message (PasteClaude: the
// queue pastes into the Claude pane, as sendQueueSend's own paste pins below),
// an agent's chat form and socket. A
// new caller — a routed message, the chief's hand-back turn, a machine-token
// message, anything an agent sends — or a queue that pasted elsewhere fails
// here; so does a door that stopped answering.
func TestOnlyTheOperatorsSendsAnswerAsksInSession(t *testing.T) {
	type site struct{ caller, target string }
	calls := map[string][]site{}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list sources: %v (%d)", err, len(files))
	}
	fset := token.NewFileSet()
	expr := func(e ast.Expr) string {
		var b strings.Builder
		_ = printer.Fprint(&b, fset, e)
		return b.String()
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "askerAddressed", "askerAddressedBy", "AnswerInSession":
					calls[sel.Sel.Name] = append(calls[sel.Sel.Name], site{fn.Name.Name, ""})
				case "mirrorAddressed":
					calls[sel.Sel.Name] = append(calls[sel.Sel.Name], site{fn.Name.Name, expr(call.Args[3])})
				case "mirrorPaste":
					if fn.Name.Name == "sendQueueDeliver" {
						calls["queue paste"] = append(calls["queue paste"], site{fn.Name.Name, expr(call.Args[6])})
					}
				}
				return true
			})
		}
	}
	for name, want := range map[string][]site{
		"AnswerInSession":  {{"askerAddressedBy", ""}},
		"askerAddressedBy": {{"askerAddressed", ""}, {"mirrorAddressed", ""}},
		"askerAddressed":   {{"handleAgentSend", ""}, {"handleAgentWS", ""}},
		"mirrorAddressed":  {{"mirrorDeliver", "p.target"}, {"sendQueueAdd", "mirror.PasteClaude"}, {"sendQueueRetry", "mirror.PasteClaude"}},
		"queue paste":      {{"sendQueueDeliver", "mirror.PasteClaude"}},
	} {
		got := calls[name]
		slices.SortFunc(got, func(a, b site) int { return strings.Compare(a.caller, b.caller) })
		if !slices.Equal(got, want) {
			t.Errorf("%s is called as %v, want exactly %v", name, got, want)
		}
	}
}
