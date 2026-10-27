package mirrorhost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// noWindowLister is a Backend with no window lister: resumeLive's process-table
// check is then the only one that can see a live session.
type noWindowLister struct{}

func (noWindowLister) List(context.Context) ([]string, error) { return nil, nil }
func (noWindowLister) Attach(context.Context, string, bool, uint16, uint16) (Terminal, error) {
	return nil, nil
}

// TestAnUnreadableProcessTableIsNotANotLiveSession: whether the session runs
// here cannot be read when the proc root cannot — and unknown is refused, never
// read as "not live" (a second CLI on one conversation).
func TestAnUnreadableProcessTableIsNotANotLiveSession(t *testing.T) {
	loc := ClaudeLocator{Home: t.TempDir(), Proc: filepath.Join(t.TempDir(), "no-proc-here")}
	live, err := resumeLive(context.Background(), loc, noWindowLister{}, "0b5e55ed-0000-4000-8000-00000000c0de")
	if err == nil {
		t.Fatalf("resumeLive = %v, nil; want an error for an unreadable process table", live)
	}
	// Control: a readable (empty) proc root with no session file is "not live".
	loc.Proc = t.TempDir()
	if live, err := resumeLive(context.Background(), loc, noWindowLister{}, "0b5e55ed-0000-4000-8000-00000000c0de"); live || err != nil {
		t.Fatalf("control: resumeLive = %v, %v; want false, nil", live, err)
	}
}

// The resume's own path checks, reached directly: over the wire the slug
// match and the directory's existence each refuse first, so a fixture there
// cannot tell whether these hold.

// TestOnlyACleanAbsolutePathIsAResumeDirectory: cleanAbsPath.
func TestOnlyACleanAbsolutePathIsAResumeDirectory(t *testing.T) {
	for p, want := range map[string]bool{
		"/srv/work/widget-api":      true,
		"srv/work/widget-api":       false, // relative
		"/srv/work/../widget-api":   false, // not clean
		"/srv/work/widget-api/":     false, // not clean
		"/srv/work/widget\n-api":    false, // a control character
		"/srv/work/widget\x1b[2J":   false,
		"":                          false,
		"/srv/work/widget api (v2)": true, // spaces are a path's own business
	} {
		if got := cleanAbsPath(p); got != want {
			t.Errorf("cleanAbsPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestTheTranscriptsCwdIsTheFirstRowThatMatchesItsProject: transcriptCwd skips
// rows with no cwd, a malformed one, a relative one and one of another
// project, and takes the first that is this transcript's own.
func TestTheTranscriptsCwdIsTheFirstRowThatMatchesItsProject(t *testing.T) {
	own := "/srv/work/widget-api"
	path := filepath.Join(t.TempDir(), "s.jsonl")
	rows := `{"type":"summary"}` + "\n" +
		`not json at all` + "\n" +
		`{"cwd":"srv/work/widget-api"}` + "\n" +
		`{"cwd":"/srv/work/other"}` + "\n" +
		`{"cwd":"/srv/work/widget-api"}` + "\n" +
		`{"cwd":"/srv/work/widget/api"}` + "\n"
	if err := os.WriteFile(path, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := transcriptCwd(path, ProjectSlug(own))
	if err != nil || got != own {
		t.Fatalf("transcriptCwd = %q, %v; want %q", got, err, own)
	}
	// Only a relative spelling of the project: none.
	if err := os.WriteFile(path, []byte(`{"cwd":"srv/work/widget-api"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := transcriptCwd(path, ProjectSlug("srv/work/widget-api")); err == nil {
		t.Fatalf("a relative cwd was taken: %q", got)
	}
}
