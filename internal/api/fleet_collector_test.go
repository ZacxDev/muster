package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
)

// 🔴 THE COLLECTOR CARRIES MORE THAN THE MAIL MAY SAY. collectFleet's facts
// hold hosts, the host-qualified name, the stored working directory, the repo's
// source and every pull request; only encodeFleetReport decides what leaves
// muster. These tests pin that seam from the mail side.

const (
	fcPrefix   = "/srv/cw"
	fcInTitle  = "Collector-test in-prefix wombat title"
	fcOutTitle = "Collector-test outside-prefix quokka title"
	fcUnkTitle = "Collector-test unknown-cwd numbat title"
)

// fcSentinel is the value every fleetFact string field the mail may NOT carry
// is set to, followed by the field's name. Short and spaced on purpose: a long
// unbroken run is cut by the secret screen, which would hide a leak for the
// wrong reason (the test also asserts nothing was cut).
const fcSentinel = "zq sentinel"

// fcMailAllowed is the fleetFact fields whose VALUE the mail reply may carry
// (screened); fcFact sets them to real values. Every OTHER field is filled
// with sentinels, recursively, and none may appear: a field added to fleetFact
// is forbidden until it is listed here — and if it is of a kind fcFill cannot
// sentinel, the test FAILS until someone decides deliberately.
var fcMailAllowed = map[string]bool{"Subject": true, "Title": true, "Repo": true, "State": true, "FirstAt": true,
	"LastAt": true, "Titles": true, "TitlesOmitted": true, "PRs": true}

// fcFill puts a sentinel naming path into every string reachable from v:
// a string, each element of a (one-element) slice, each field of a struct,
// the target of a pointer. Any other kind — a number, a bool, a map, a struct
// with unexported fields such as time.Time — cannot carry a sentinel, so it
// fails the test instead of passing unseen.
func fcFill(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(fcSentinel + " " + strings.ToLower(path))
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fcFill(t, s.Index(0), path+"0")
		v.Set(s)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fcFill(t, p.Elem(), path)
		v.Set(p)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fld := v.Type().Field(i)
			if !fld.IsExported() {
				t.Fatalf("fleetFact field %s (%s) holds an unexported field %s: fcFill cannot sentinel it; "+
					"list it in fcMailAllowed or teach fcFill", path, v.Type(), fld.Name)
			}
			fcFill(t, v.Field(i), path+strings.ToLower(fld.Name))
		}
	default:
		t.Fatalf("fleetFact field %s is a %s, which fcFill cannot sentinel: list it in fcMailAllowed or teach fcFill",
			path, v.Kind())
	}
}

func fcFact(t *testing.T, sub focus.Subject, title, cwd string) fleetFact {
	t.Helper()
	f := fleetFact{Subject: sub, State: "active", FirstAt: time.Unix(1_700_000_000, 0), LastAt: time.Unix(1_700_000_100, 0),
		Titles: []focus.TitleAt{{Title: title, At: time.Unix(1_700_000_050, 0)}}, PRs: []string{"acme-labs/wombat#7"}}
	v := reflect.ValueOf(&f).Elem()
	fields := map[string]bool{}
	for i := 0; i < v.NumField(); i++ {
		fld := v.Type().Field(i)
		fields[fld.Name] = true
		if !fcMailAllowed[fld.Name] {
			fcFill(t, v.Field(i), fld.Name)
		}
	}
	for name := range fcMailAllowed {
		if !fields[name] {
			t.Fatalf("fcMailAllowed lists %s, which fleetFact no longer has", name)
		}
	}
	f.Title, f.Repo = "pane-"+title[len("Collector-test "):len("Collector-test ")+4], "wombat-repo"
	// Cwd decides the filter, so it must be a real directory; its LEAF is the
	// sentinel, and the whole of it must not appear.
	f.Cwd = cwd
	return f
}

func fcCollected(t *testing.T) fleetCollected {
	t.Helper()
	return fleetCollected{
		Facts: []fleetFact{
			fcFact(t, focus.MirrorSubject("fc-in"), fcInTitle, fcPrefix+"/zqleaf"),
			fcFact(t, focus.MirrorSubject("fc-out"), fcOutTitle, "/opt/zqelse"),
			fcFact(t, focus.MirrorSubject("fc-unk"), fcUnkTitle, ""),
		},
		Online:  []string{"zqonlinehost"},
		Offline: []fleetOffline{{Host: "zqofflinehost", Since: time.Unix(1_600_000_000, 0)}},
	}
}

func fcMail(t *testing.T, c fleetCollected) string {
	t.Helper()
	id := "req-fc"
	out, err := encodeFleetReport(c, fleetWindow{Since: "S", Until: "U", Zone: "UTC"}, &id, time.Unix(1_700_000_200, 0),
		&fleetMail{paths: plugins.FleetReportPaths{fcPrefix}}, plugins.MaxDraftBytes)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestTheMailEncoderCarriesNoCollectorOnlyField: from facts whose every
// non-mail field is a sentinel, the mail reply describes exactly the
// in-prefix session (positive control: its title and pull request are there),
// with the contract's exact shape, and no sentinel, host, directory or
// out-of-prefix/unknown-directory session anywhere.
func TestTheMailEncoderCarriesNoCollectorOnlyField(t *testing.T) {
	out := fcMail(t, fcCollected(t))
	// Positive control: the in-prefix session IS described, so the absences
	// below are not those of an empty reply.
	if !strings.Contains(out, fcInTitle) || !strings.Contains(out, "acme-labs/wombat#7") || !strings.Contains(out, `"name":"pane-in-p"`) {
		t.Fatalf("the in-prefix session is not described: %s", out)
	}
	// Nothing was cut by the secret screen, so no absence below is the
	// screen's doing rather than the encoder's.
	if strings.Contains(out, "secret-shaped") || strings.Contains(out, "[removed]") {
		t.Fatalf("the secret screen cut something; the fixture must not look secret-shaped: %s", out)
	}
	for _, never := range []string{fcSentinel, "zq", fcPrefix, "/opt/", fcOutTitle, fcUnkTitle, "fc-out", "fc-unk"} {
		if strings.Contains(out, never) {
			t.Errorf("the mail carries %q: %s", never, out)
		}
	}
	// 🔴 THE SHAPE IS THE CONTRACT'S, EXACTLY: no key beyond these, at either
	// level. A collector field that reached the encoder as a new key fails here
	// even if its value were not a sentinel.
	var raw struct {
		Coverage map[string]json.RawMessage   `json:"coverage"`
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &top); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]json.RawMessage) string {
		var k []string
		for n := range m {
			k = append(k, n)
		}
		sort.Strings(k)
		return strings.Join(k, ",")
	}
	if got := keys(top); got != "coverage,generated_at,request_id,sessions,type,v,window" {
		t.Errorf("top-level keys %s", got)
	}
	if got := keys(raw.Coverage); got != "gaps,hosts,omitted,sessions" {
		t.Errorf("coverage keys %s", got)
	}
	if len(raw.Sessions) != 1 {
		t.Fatalf("sessions %d; want exactly the in-prefix one: %s", len(raw.Sessions), out)
	}
	if got := keys(raw.Sessions[0]); got != "first_seen,focus,host,id,kind,last_seen,name,prs,repo,state" {
		t.Errorf("session keys %s", got)
	}
	if string(raw.Sessions[0]["host"]) != `""` || string(raw.Coverage["hosts"]) != `[]` {
		t.Errorf("host %s, coverage.hosts %s; want both empty", raw.Sessions[0]["host"], raw.Coverage["hosts"])
	}
	// The unknown-directory session is counted, the outside one is not mentioned.
	if !strings.Contains(out, `"1 session(s) whose working directory muster does not know were left out"`) ||
		!strings.Contains(out, `"1 mirror host(s) offline: whether their sessions are still running is unknown"`) {
		t.Errorf("gaps: %s", out)
	}
}

// The control for the test above: the chief's tool, from the same facts, DOES
// carry the host and the qualified name — so the sentinels are reachable by an
// encoder, and their absence from the mail is the mail encoder's doing.
func TestTheToolEncoderShowsWhatTheMailWithholds(t *testing.T) {
	out, err := encodeFleetReport(fcCollected(t), fleetWindow{}, nil, time.Unix(1_700_000_200, 0), nil, overviewMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{fcSentinel + " host", fcSentinel + " name", "zqonlinehost", "zqofflinehost",
		fcOutTitle, fcUnkTitle} {
		if !strings.Contains(out, want) {
			t.Errorf("the tool lacks %q: %s", want, out)
		}
	}
	// Neither door writes the directory: no encoder carries it today.
	for _, never := range []string{"zqleaf"} {
		if strings.Contains(out, never) {
			t.Errorf("the tool carries %q: %s", never, out)
		}
	}
}

// 🔴 THE LEDGER: who calls what across the collector/encoder and stall seams,
// in every non-test Go file of the module. It fails when a set GROWS (a second
// window-history reader for a report; an encoder bypassing buildFleetReport; a
// second stall classifier) or SHRINKS (a consumer that stopped using the shared
// path). Adding the check-in builder is an intended change here.
func TestTheCollectorAndStallLedger(t *testing.T) {
	want := map[string]string{
		"WindowHistory":     "api.collectFleet",
		"collectFleet":      "api.buildFleetReport",
		"encodeFleetReport": "api.buildFleetReport",
		"stallInputs":       "api.nudgeCandidates",
		"nudgeCandidates":   "api.sweepOwnerNudges",
	}
	got := map[string][]string{}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "vendor" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		// 🔴 ANY REFERENCE, NOT ONLY A DIRECT CALL: a method value
		// (`cf := s.collectFleet; cf(...)`), a function passed as an argument
		// and a method expression all name the function without calling it
		// there, and each is a second user. Every identifier spelling a tracked
		// name counts — a SelectorExpr's Sel is itself an Ident, so `s.x` and a
		// bare `x` are both seen once. The declaring name (FuncDecl.Name) is not
		// walked, so a definition is not a reference.
		record := func(root ast.Node, owner string) {
			ast.Inspect(root, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					if _, tracked := want[id.Name]; tracked {
						got[id.Name] = append(got[id.Name], f.Name.Name+"."+owner)
					}
				}
				return true
			})
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					record(d.Body, d.Name.Name)
				}
			case *ast.GenDecl:
				// A package-level `var x = func() { … }` (or any initializer)
				// is code too; its owner is the declared names.
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					var names []string
					for _, n := range vs.Names {
						names = append(names, n.Name)
					}
					for _, v := range vs.Values {
						record(v, "var:"+strings.Join(names, "+"))
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: the walk saw the module, not an empty directory.
	if files < 100 {
		t.Fatalf("the walk parsed %d files; it is not reading the module", files)
	}
	for name, callers := range want {
		sort.Strings(got[name])
		if g := strings.Join(got[name], ","); g != callers {
			t.Errorf("%s is referenced from [%s]; the ledger says [%s]", name, g, callers)
		}
	}
}
