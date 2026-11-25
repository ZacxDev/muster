package pages

import (
	"strings"
	"testing"
)

// TestTheShellDrawsTheFeedbackPill: a shell with Feedback draws the pill and
// its hidden panel (textarea, Send, close) in the SHELL — never in the frame's
// content — posting to its own Base + /feedback: a link's without credentials,
// the owner's with their session. The microphone is the owner chrome's alone,
// and the frame is never granted it. A shell without Feedback draws none.
func TestTheShellDrawsTheFeedbackPill(t *testing.T) {
	link := Shell(ShellView{Title: "Plan", FrameSrc: "/s/tok-a1/content", Base: "/s/tok-a1", Nonce: "N1", Feedback: &FeedbackChrome{}})
	for _, want := range []string{`data-feedback-pill`, `id="page-feedback"`, `data-feedback hidden`, `<textarea id="page-feedback-text"`,
		`maxlength="4000"`, `data-feedback-send disabled>Send</button>`, `data-feedback-close`, FeedbackLeadLink,
		`POST="/s/tok-a1/feedback"`, `credentials:"omit"`, `referrerPolicy:"no-referrer"`, `.fbp{position:fixed`} {
		if !strings.Contains(link, want) {
			t.Errorf("the link's shell lacks %q", want)
		}
	}
	if strings.Contains(link, `data-dictate="`) || strings.Contains(link, "page-feedback-mic") || strings.Contains(link, FeedbackLeadOwner) {
		t.Error("a link's shell draws the owner's microphone or words")
	}
	// The pill is outside the frame: the content document is the frame's src,
	// and the frame element itself carries no allow= delegation.
	frame := link[strings.Index(link, "<iframe"):]
	frame = frame[:strings.Index(frame, ">")+1]
	if strings.Contains(frame, "allow=") {
		t.Errorf("the content frame is delegated a permission: %s", frame)
	}

	owner := Shell(ShellView{Title: "Plan", FrameSrc: "/pages/7/versions/2/content", Base: "/pages/7/versions/2", Nonce: "N2",
		Owner:    &OwnerChrome{PageID: 7, Version: 2},
		Feedback: &FeedbackChrome{Mic: `<button data-dictate="/transcribe" id="page-feedback-mic">mic-9</button>`, MicNote: `<p id="page-feedback-note">note-9</p>`, MicScript: `/*dictate-js-9*/`}})
	for _, want := range []string{`POST="/pages/7/versions/2/feedback"`, `credentials:"same-origin"`, `referrerPolicy:"same-origin"`,
		`mic-9</button><span class="fbc"`, `note-9`, `/*dictate-js-9*/`, FeedbackLeadOwner} {
		if !strings.Contains(owner, want) {
			t.Errorf("the owner's shell lacks %q", want)
		}
	}
	// The mic sits on the row BENEATH the textarea, with Send.
	if ta, row := strings.Index(owner, "<textarea"), strings.Index(owner, `data-feedback-row`); ta < 0 || row < ta || strings.Index(owner, "mic-9") < row {
		t.Error("the microphone is not on the row beneath the textarea")
	}
	if none := Shell(ShellView{Title: "Plan", FrameSrc: "/s/tok/content", Base: "/s/tok", Nonce: "N3"}); strings.Contains(none, "data-feedback") || strings.Contains(none, ".fbp{") {
		t.Error("a shell without Feedback draws the pill")
	}
}

// TestFeedbackDataRoundTrips: a message is stored as {"text": …} and read
// back, and described for the owner's screen as one line.
func TestFeedbackDataRoundTrips(t *testing.T) {
	if got := FeedbackText(FeedbackData("round-trip-5")); got != "round-trip-5" {
		t.Errorf("FeedbackText(FeedbackData(…)) = %q", got)
	}
	if a := DescribeFeedback(FeedbackData("describe-6")); a.Heading != "Feedback" || len(a.Lines) != 1 || a.Lines[0].Value != "describe-6" {
		t.Errorf("DescribeFeedback: %+v", a)
	}
}
