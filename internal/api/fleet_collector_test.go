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
// (screened). Every other string field is set to a sentinel and must not
// appear: a field added to fleetFact is forbidden until it is listed here.
var fcMailAllowed = map[string]bool{"Title": true, "Repo": true, "State": true}

func fcFact(sub focus.Subject, title, cwd string) fleetFact {
	f := fleetFact{Subject: sub, State: "active", FirstAt: time.Unix(1_700_000_000, 0), LastAt: time.Unix(1_700_000_100, 0),
		Titles: []focus.TitleAt{{Title: title, At: time.Unix(1_700_000_050, 0)}}, PRs: []string{"acme-labs/wombat#7"}}
	v := reflect.ValueOf(&f).Elem()
	for i := 0; i < v.NumField(); i++ {
		fld := v.Type().Field(i)
		if fld.Type.Kind() == reflect.String && !fcMailAllowed[fld.Name] {
			v.Field(i).SetString(fcSentinel + " " + strings.ToLower(fld.Name))
		}
	}
	f.Title, f.Repo = "pane-"+title[len("Collector-test "):len("Collector-test ")+4], "wombat-repo"
	// Cwd decides the filter, so it must be a real directory; its LEAF is the
	// sentinel, and the whole of it must not appear.
	f.Cwd = cwd
	return f
}

func fcCollected() fleetCollected {
	return fleetCollected{
		Facts: []fleetFact{
			fcFact(focus.MirrorSubject("fc-in"), fcInTitle, fcPrefix+"/zqleaf"),
			fcFact(focus.MirrorSubject("fc-out"), fcOutTitle, "/opt/zqelse"),
			fcFact(focus.MirrorSubject("fc-unk"), fcUnkTitle, ""),
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
	out := fcMail(t, fcCollected())
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
	out, err := encodeFleetReport(fcCollected(), fleetWindow{}, nil, time.Unix(1_700_000_200, 0), nil, overviewMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{fcSentinel + " host", fcSentinel + " name", "zqonlinehost", "zqofflinehost",
		fcOutTitle, fcUnkTitle} {
		if !strings.Contains(out, want) {
			t.Errorf("the tool lacks %q: %s", want, out)
		}
	}
	// Neither door writes the directory or the repo's source: no encoder
	// carries them today.
	for _, never := range []string{"zqleaf", fcSentinel + " reposource"} {
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
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fx := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = fx.Sel.Name
				case *ast.Ident:
					name = fx.Name
				}
				if _, tracked := want[name]; tracked {
					got[name] = append(got[name], f.Name.Name+"."+fn.Name.Name)
				}
				return true
			})
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
			t.Errorf("%s is called from [%s]; the ledger says [%s]", name, g, callers)
		}
	}
}
