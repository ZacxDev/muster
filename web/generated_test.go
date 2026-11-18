package web

import (
	"io"
	"io/fs"
	"strings"
	"testing"
)

// A generated file is served by Files beside the embed, and hashed like any
// embedded file.
func TestAGeneratedFileIsServedAndHashed(t *testing.T) {
	p := Register("/static/js/zz-generated-test.js", []byte("var zz = 1;"))
	if p != "/static/js/zz-generated-test.js" {
		t.Fatalf("Register returned %q", p)
	}
	b, err := fs.ReadFile(Files, "static/js/zz-generated-test.js")
	if err != nil || string(b) != "var zz = 1;" {
		t.Fatalf("ReadFile: %q, %v", b, err)
	}
	// Through Open (what http.FileServer uses): it must seek.
	f, err := Files.Open("static/js/zz-generated-test.js")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.(io.Seeker).Seek(4, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	rest, _ := io.ReadAll(f)
	if string(rest) != "zz = 1;" {
		t.Fatalf("after seek: %q", rest)
	}
	if st, _ := f.Stat(); st.Size() != 11 || st.Name() != "zz-generated-test.js" {
		t.Fatalf("stat: %v %v", st.Size(), st.Name())
	}
	// sha256("var zz = 1;") — pinned, not asked of the cache.
	if got := Asset(p); got != p+"?v=8c713ffd79aa" {
		t.Errorf("Asset = %q", got)
	}
	// The embed is still there behind it.
	if _, err := fs.Stat(Files, "static/app.css"); err != nil {
		t.Errorf("the embed is not reachable through Files: %v", err)
	}
	if !strings.Contains(strings.Join(Generated(), " "), p) {
		t.Errorf("Generated() = %v, missing %s", Generated(), p)
	}
}

// Each refusal is reached on its own: a path outside /static/js/, one not a
// .js, one not clean, and a second registration. (The embed-collision arm
// cannot be reached: the embed carries no static/js/ today.)
func TestRegisterRefusesAmbiguousPaths(t *testing.T) {
	Register("/static/js/zz-twice.js", []byte("1"))
	for name, path := range map[string]string{
		"outside /static/js/": "/static/app2.js",
		"not .js":             "/static/js/x.css",
		"unclean":             "/static/js/../app.css",
		"registered twice":    "/static/js/zz-twice.js",
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: Register(%q) did not panic", name, path)
				}
			}()
			Register(path, []byte("x"))
		}()
	}
}
