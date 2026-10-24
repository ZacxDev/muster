package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// CHARACTERIZATION GOLDENS for the runtime-interface refactor: what -list,
// -list -windows, -list -claude and -list -hermes print, recorded BEFORE the
// refactor and read the same after it. A golden that changes is a behaviour
// change. Every fixture is synthetic.

// TODO(runtime-interface): delete these goldens in the final fix pass after
// review round 1; until then they are the proof that before equals after.

var updateCharacterization = flag.Bool("update-characterization", false, "rewrite testdata/characterization/*.golden")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "characterization", name+".golden")
	if *updateCharacterization {
		must(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the golden (record it with -update-characterization BEFORE a refactor, never after): %v", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from its golden %s.\n--- got\n%s\n--- want\n%s", name, path, got, want)
	}
}

func TestCharacterizeTheListings(t *testing.T) {
	for _, c := range []struct {
		name    string
		backend mirrorhost.Backend
	}{{"claude-windows", windowsListBackend{}}, {"hermes-windows", hermesListBackend{}}} {
		withBackend(t, c.backend)
		var b strings.Builder
		for _, args := range [][]string{{"-list"}, {"-list", "-windows"}, {"-list", "-claude"}} {
			rc, out, errs := runWith(t, nil, args...)
			b.WriteString("$ " + strings.Join(args, " ") + "\n")
			b.WriteString(out)
			b.WriteString("stderr: " + errs + "\n")
			b.WriteString("rc " + string(rune('0'+rc)) + "\n")
		}
		golden(t, "list-"+c.name, b.String())
	}

	withBackend(t, hermesListBackend{})
	home := t.TempDir()
	hermesConfig(t, home, binX)
	status := t.TempDir()
	must(t, hermeshost.StatusDir(status).Write(hermeshost.StatusFile{SessionID: listSID, Status: mirror.StatusIdle,
		Since: 1700000000000, WriterPID: 50, WriterStart: "1"}))
	out := listHermes(t, home, map[string]string{envHermesStatusDir: status})
	out = strings.ReplaceAll(strings.ReplaceAll(out, home, "<home>"), status, "<status>")
	if exe, err := os.Executable(); err == nil {
		out = strings.ReplaceAll(out, exe, "<this-binary>")
	}
	golden(t, "list-hermes", out)
}
