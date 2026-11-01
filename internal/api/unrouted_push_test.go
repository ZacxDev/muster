package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestTheUnroutedAskPushIsCompact: muster's own ask about a plugin message
// nobody acted on carries ~150 characters of muster's sentence before the
// sender, so its notification body says who and what in compact form instead —
// "From: <sender> · Subject: <subject>", then the option — while the ask's
// question (what the page shows) is unchanged. The REAL question (the same
// function the sweep calls), the REAL body, literal. An unverified sender's
// long mark is the page's short note. RED at #313's round-0 head, whose body
// was the boilerplate cut before the subject.
func TestTheUnroutedAskPushIsCompact(t *testing.T) {
	m := plugins.Message{ID: 42, Sender: "Ada Lovelace <ada@example.test>", Subject: "Invoice 1043 is overdue", Body: "Please pay by Friday."}
	q := unroutedQuestionText("Example inbox", m)
	a := attention.Ask{ID: 9, Owner: 1, Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, Question: q,
		Options: []string{unroutedNoAction}}
	it := askItem(a, nil)
	if it.Text != q {
		t.Fatal("the page's question changed")
	}
	const want = "From: Ada Lovelace <ada@example.test> (unverified sender) · Subject: Invoice 1043 is overdue\nOptions: No action needed"
	if got := attentionPushBody(it); got != want {
		t.Fatalf("body = %q\nwant   %q", got, want)
	}
	// A verified sender carries no note; a question that is not muster's
	// format (an agent's ask that happens to say "From:") keeps its own text.
	m.SenderVerified = true
	if got := attentionPushBody(askItem(attention.Ask{ID: 9, Asker: attention.Asker{System: true}, Question: unroutedQuestionText("Example inbox", m)}, nil)); got !=
		"From: Ada Lovelace <ada@example.test> · Subject: Invoice 1043 is overdue" {
		t.Fatalf("verified: %q", got)
	}
	agent := attention.Ask{ID: 10, Asker: attention.Asker{AgentID: 3, Label: "w"}, Question: "From: x\nSubject: y — ok?"}
	if got := attentionPushBody(askItem(agent, nil)); got != "From: x Subject: y — ok?" {
		t.Fatalf("CONTROL: an agent's question was rewritten: %q", got)
	}
}
