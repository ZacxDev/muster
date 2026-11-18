package mirror_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

const mixedFormID = "frm_1a2aa3ec2001MixedFixtureFrm"

// mixedFormFrame is a form.created frame (the captured frames' shape) in the
// fixture's form session: q0 a single-select that takes only its options,
// q1 a question with no options that takes only free text.
func mixedFormFrame(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"id": "evt_fixturemixed", "type": "form.created", "location": map[string]any{"directory": "/srv/fixture"},
		"data": map[string]any{"form": map[string]any{"id": mixedFormID, "sessionID": opencodetest.FixtureFormSession, "title": "Questions",
			"metadata": map[string]any{"kind": "question"},
			"fields": []any{
				map[string]any{"key": "q0", "title": "Branch", "description": "Which branch?", "type": "string",
					"options": []any{map[string]any{"value": "main", "label": "main"}, map[string]any{"value": "next", "label": "next"}}},
				map[string]any{"key": "q1", "title": "Why", "description": "Say why, in your words", "type": "string", "custom": true},
			}}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAMixedFormIsAnsweredWholeEndToEnd: with a REAL muster-mirror client and
// the fake opencode server, a form whose first question takes only its
// options and whose second takes only free text is read whole and answered:
// the option's VALUE, and the person's own words as typed — newlines
// included — to exactly that form in that session.
func TestAMixedFormIsAnsweredWholeEndToEnd(t *testing.T) {
	r, srv, window := formRig(t, true)
	srv.EmitFrame(mixedFormFrame(t))
	eventually(t, "the question on the hub", func() bool {
		w, _ := window()
		return w.Status == mirror.StatusWaiting && w.Pending != nil && w.Pending.PromptID == mixedFormID
	})
	ctx, cancel := context.WithTimeout(asUser(1), 10*time.Second)
	defer cancel()
	f, err := r.svc.OpenCodeForm(ctx, "on", "work", "@1", mixedFormID)
	if err != nil || len(f.Questions) != 2 || f.Questions[0].Custom || !f.Questions[1].Custom || len(f.Questions[1].Options) != 0 {
		t.Fatalf("the form: %+v (%v)", f, err)
	}
	if err := r.svc.AnswerOpenCodeForm(ctx, "on", "work", "@1", f,
		[]mirror.OpenCodePick{{Key: "q0", Picks: []int{1}}, {Key: "q1", Other: "it builds\nand it ships"}}); err != nil {
		t.Fatal(err)
	}
	rs := srv.Replies()
	want := "/api/session/" + opencodetest.FixtureFormSession + "/form/" + mixedFormID + "/reply"
	if len(rs) != 1 || rs[0].Path != want || rs[0].Body != `{"answer":{"q0":"next","q1":"it builds\nand it ships"}}` {
		t.Fatalf("the server got %+v", rs)
	}
}
