package mirrorhost

import (
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// A prompt's working directory travels as it is — unless the redactor would
// change it: then it is dropped (unknown), never sent rewritten.
func TestAPromptsWorkingDirectoryIsKeptOrDropped(t *testing.T) {
	ev := cctranscript.Event{Kind: cctranscript.KindPrompt, Text: "hi", Cwd: "/srv/example-work/app"}
	redactEvent(&ev)
	if ev.Cwd != "/srv/example-work/app" {
		t.Fatalf("control: a plain directory became %q", ev.Cwd)
	}
	ev = cctranscript.Event{Kind: cctranscript.KindPrompt, Text: "hi", Cwd: "/srv/ghp_PathSecret0006/app"}
	redactEvent(&ev)
	if ev.Cwd != "" {
		t.Fatalf("a directory holding a token travelled as %q", ev.Cwd)
	}
}
