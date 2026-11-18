package web

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// GENERATED STATIC FILES: bytes a Go package builds once, at init, and that are
// then served under /static/ exactly like the embedded files — the same
// content-hashed URL (Asset), the same immutable caching (internal/api
// staticHandler), the same service-worker precache.
//
// 🔴 WHY THE PAGES' SCRIPTS ARE HERE AND NOT FILES IN static/. They are written
// in Go (internal/ui) because they are BUILT from Go: tab registries, storage
// keys, DOM ids and attribute names spelled once in Go and pasted into the
// script, so the server's markup and the browser's code cannot disagree. A .js
// file under static/ would need every one of those values copied in as a
// literal — a second copy of each, which is the drift those concatenations
// exist to prevent. So the script stays where its inputs are, and only its
// DELIVERY moves: out of every document, into one URL per script that the
// browser keeps for a year.
//
// 🔴 A GENERATED FILE MUST BE THE SAME FOR EVERY REQUEST. It is registered once
// per process and served to every user under a URL that names its bytes, so
// nothing per-user or per-request may be in it: those values travel on the
// <script> tag (data-* attributes) that references it. internal/ui's
// static-script tests pin that the bytes do not move with the viewer.

// Files is what /static/ serves: the embedded tree (Static) with the generated
// files laid over it. Its root holds the static/ directory, as Static's does.
var Files fs.FS = overlay{}

var generated = struct {
	sync.RWMutex
	files map[string][]byte // "static/js/app.js" → bytes
}{files: map[string][]byte{}}

// Register adds a generated file at urlPath ("/static/js/<name>.js") and
// returns urlPath, so a package-level var can both register and name it.
//
// It panics — at init, so a build that does it never starts — on a path
// outside /static/js/, on a path registered twice, and on a path the embed
// already carries: each would serve one of two different files under one URL.
func Register(urlPath string, b []byte) string {
	rest, ok := strings.CutPrefix(urlPath, "/static/js/")
	if !ok || rest == "" || path.Clean(urlPath) != urlPath || !strings.HasSuffix(rest, ".js") {
		panic(fmt.Sprintf("web.Register: %q is not a /static/js/<name>.js path", urlPath))
	}
	name := strings.TrimPrefix(urlPath, "/")
	if _, err := fs.Stat(Static, name); err == nil {
		panic(fmt.Sprintf("web.Register: %q is already an embedded file", urlPath))
	}
	generated.Lock()
	defer generated.Unlock()
	if _, dup := generated.files[name]; dup {
		panic(fmt.Sprintf("web.Register: %q registered twice", urlPath))
	}
	generated.files[name] = append([]byte(nil), b...)
	assets.forget(urlPath) // a hash asked for before it existed was ""
	return urlPath
}

// Generated lists every generated file's URL path, sorted.
func Generated() []string {
	generated.RLock()
	defer generated.RUnlock()
	out := make([]string, 0, len(generated.files))
	for name := range generated.files {
		out = append(out, "/"+name)
	}
	sort.Strings(out)
	return out
}

func generatedFile(name string) ([]byte, bool) {
	generated.RLock()
	defer generated.RUnlock()
	b, ok := generated.files[name]
	return b, ok
}

// overlay is Files: a generated file by its name, otherwise Static.
type overlay struct{}

func (overlay) Open(name string) (fs.File, error) {
	if b, ok := generatedFile(name); ok {
		return &memFile{name: path.Base(name), Reader: bytes.NewReader(b), size: int64(len(b))}, nil
	}
	return Static.Open(name)
}

func (o overlay) ReadFile(name string) ([]byte, error) {
	if b, ok := generatedFile(name); ok {
		return append([]byte(nil), b...), nil
	}
	return Static.ReadFile(name)
}

// memFile is an open generated file. It seeks, because http.FileServer seeks
// (to sniff and to serve ranges).
type memFile struct {
	*bytes.Reader
	name string
	size int64
}

func (f *memFile) Stat() (fs.FileInfo, error) { return memInfo{f.name, f.size}, nil }
func (f *memFile) Close() error               { return nil }

var _ io.ReadSeeker = (*memFile)(nil)

type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0o444 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }
