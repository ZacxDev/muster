package ui

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// TAILWIND SCANS THIS PACKAGE'S TEST FILES, SO A GUARD'S OWN FIXTURE SHIPS CSS.
//
// tailwind.config.js lists `./internal/ui/**/*.go` — test files and comments
// included — so any token in a _test.go file that Tailwind recognises as a class
// candidate is emitted into web/static/app.css and served to every device. It is
// not hypothetical: one draft of the visibility guard's fixture table added 433
// bytes and seven classes that nothing renders, and this PR's own F3 work closed
// a second leak (43,134 → 43,606 → back to 43,173 bytes).
//
// 🔴 THIS IS A RATCHET, NOT A BAN, AND THE DIFFERENCE IS LOAD-BEARING. A blanket
// "no test file may contribute a class" would be RED ON DAY ONE: rebuilding this
// tree with internal/ui/*_test.go deleted is 640 bytes and 11 classes smaller,
// and the IDENTICAL 11-class set is already on trunk. A permanently-red gate is
// worse than no gate — everyone learns to click through it — so what is pinned is
// the SET: exactly these 11, no more. A twelfth fails here with the eviction
// playbook, and a leak that gets fixed fails here too, so the ledger cannot rot.
//
// The attribution is textual, not a Tailwind run: a class in app.css whose token
// appears in NO non-test scanned source is one only a test file can have
// produced. Verified against the real thing — building app.css with and without
// the test files gives exactly the same 11 (and exactly 640 bytes), measured with
// the repo-pinned Tailwind v3.
//
// ⚠ SCOPE, STATED SO NOBODY OVER-READS A GREEN. This reads a BUILD ARTIFACT, so
// it reports on the stylesheet AS LAST BUILT — a leak introduced after the last
// `make css` is invisible here until someone rebuilds. In muster that window is
// closed from the other side: web/static/app.css is COMMITTED (not gitignored as
// it was upstream, where this note read the other way round) and `make css-check`
// rebuilds it and refuses a stale committed copy, so "as last built" and "as in
// the tree" cannot drift apart without a red gate. A missing app.css fails here
// rather than skipping.
// ---------------------------------------------------------------------------

// cssClassTokenChar reports whether c can appear inside a Tailwind class token.
// It is the boundary rule for cssTokenOccurs: `shrink` must not be found inside
// `shrink-0`.
func cssClassTokenChar(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// cssTokenOccurs reports whether tok appears in text as a standalone class token.
// The boundary is applied only at an END THAT IS ITSELF A TOKEN CHARACTER: a
// candidate like `[start:k]` is extracted by Tailwind out of `src[start:k]`, so
// demanding a non-token char before its `[` would miss every occurrence.
func cssTokenOccurs(tok, text string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], tok)
		if j < 0 {
			return false
		}
		j += i
		okBefore := !cssClassTokenChar(tok[0]) || j == 0 || !cssClassTokenChar(text[j-1])
		end := j + len(tok)
		okAfter := !cssClassTokenChar(tok[len(tok)-1]) || end == len(text) || !cssClassTokenChar(text[end])
		if okBefore && okAfter {
			return true
		}
		i = j + 1
	}
}

// cssDecodeClass decodes one CSS-escaped class name starting at sel[i] (just past
// the '.') and returns it with the index after it. CSS escapes are `\<hex>{1,6}`
// with an optional trailing space (how Tailwind writes `:` `[` `]` and any
// non-ASCII char) or `\<char>`.
func cssDecodeClass(sel string, i int) (string, int) {
	const special = " \t\n\r,.#:>+~()[]{}\\'\"=*|^$/%!;"
	var b strings.Builder
	for i < len(sel) {
		c := sel[i]
		if c == '\\' {
			j, hex := i+1, ""
			for j < len(sel) && len(hex) < 6 && strings.IndexByte("0123456789abcdefABCDEF", sel[j]) >= 0 {
				hex += string(sel[j])
				j++
			}
			if hex != "" {
				n, err := strconv.ParseInt(hex, 16, 32)
				if err != nil {
					return b.String(), j
				}
				b.WriteRune(rune(n))
				if j < len(sel) && sel[j] == ' ' {
					j++
				}
				i = j
				continue
			}
			if j < len(sel) {
				b.WriteByte(sel[j])
				i = j + 1
				continue
			}
			break
		}
		if strings.IndexByte(special, c) >= 0 {
			break
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), i
}

// cssClasses returns every class name that appears in a SELECTOR in src.
//
// 🔴 SELECTORS ONLY — a declaration value is full of dots. `transition:opacity
// .2s` and `line-height:1.375` yield `.2s` / `.375`, and a naive scan reports
// them as classes; measured on this stylesheet, that inflated 605 real classes to
// 647 and produced 15 phantom "test-only" entries. So the scan takes the text
// before each `{`, skips at-rules, and reads only there.
func cssClasses(src string) map[string]bool {
	out := map[string]bool{}
	rest := src
	for {
		k := strings.IndexByte(rest, '{')
		if k < 0 {
			return out
		}
		sel := rest[:k]
		if p := strings.LastIndexAny(sel, "}{"); p >= 0 {
			sel = sel[p+1:]
		}
		rest = rest[k+1:]
		if strings.HasPrefix(strings.TrimSpace(sel), "@") {
			continue
		}
		for i := 0; i < len(sel); {
			// A '.' starts a class unless it is escaped (`\.`, a literal dot inside a
			// class name) or follows a digit (a decimal that survived into selector
			// text). A '.' after a LETTER is a compound selector — `.a.b` — and must
			// still count, which is why this is not the same rule as "preceded by any
			// token character".
			digit := i > 0 && sel[i-1] >= '0' && sel[i-1] <= '9'
			if sel[i] != '.' || digit || (i > 0 && sel[i-1] == '\\') {
				i++
				continue
			}
			name, next := cssDecodeClass(sel, i+1)
			if name == "" {
				i++
				continue
			}
			out[name] = true
			i = next
		}
	}
}

// cssScannedSources returns the contents of every file tailwind.config.js
// scans, split into non-test and test-only.
//
// 🔴 THE GLOBS ARE PARSED OUT OF tailwind.config.js, NOT MIRRORED BY HAND.
// Upstream hand-copied them here with a comment asking the next person to keep
// the two in step — which is the duplication this whole file exists to catch,
// one level up: a content entry added to the config and not here makes this
// guard NARROWER than the build it claims to describe, and the shortfall is
// invisible because the guard still passes. Reading the config removes the
// question. It also makes the API carve's owed entry (a second document source
// outside this package) self-covering: adding it to the config puts it under
// this guard with no second edit.
func cssScannedSources(t *testing.T) (nonTest, tests map[string]string) {
	t.Helper()
	nonTest, tests = map[string]string{}, map[string]string{}
	add := func(path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read scanned source %s: %v", path, err)
		}
		if strings.HasSuffix(path, "_test.go") {
			tests[path] = string(b)
		} else {
			nonTest[path] = string(b)
		}
	}

	globs := tailwindContentGlobs(t)
	// POSITIVE CONTROL for the config parser: an empty or mis-parsed content
	// array yields no globs, which would leave both maps empty and make every
	// attribution below vacuous in the PERMISSIVE direction (no non-test source
	// accounts for anything, so every class reads as test-only).
	if len(globs) < 2 {
		t.Fatalf("parsed %d content glob(s) from tailwind.config.js (%v) — the parser is not reading "+
			"the config, so nothing below describes the real build", len(globs), globs)
	}

	for _, gl := range globs {
		// 🔴 A `!` ENTRY IS AN EXCLUSION, NOT A DIRECTORY. Walking it as a path
		// would fail on a directory literally named "!"; treating it as an
		// inclusion would be worse — this guard would then read the very files
		// Tailwind is configured NOT to scan, and report their classes as
		// attributed. The exclusion this config carries is the whole reason the
		// test-only leak is zero, so mis-reading it would make the zero vacuous.
		if strings.HasPrefix(gl, "!") {
			continue
		}
		root, exts := globRootAndExts(t, gl)
		if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			for _, e := range exts {
				if strings.HasSuffix(path, e) {
					add(path)
					return nil
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("walk %s (from content glob %q): %v", root, gl, err)
		}
	}

	// 🔴 THE TAILWIND *INPUT* IS A NON-TEST SOURCE, THOUGH IT IS NOT A CONTENT
	// GLOB. Classes hand-written in input.css (.htmx-swapping, .card-removed and
	// the other htmx lifecycle hooks) are AUTHORED CSS: they reach the stylesheet
	// through the @layer that declares them, not through Tailwind's scan, so no
	// content glob can ever account for them and the attribution below calls them
	// unattributable.
	//
	// Upstream never saw this, and the reason is worth recording: its
	// attribution happened to succeed because a COMMENT in a component file
	// mentioned `htmx-swapping` in passing. Carving that component out of muster
	// deleted the comment and the guard immediately reported a leak in a class
	// nobody had touched — a green that had been resting on prose.
	add("../../web/css/input.css")

	// 🔴 `tests` IS EXPECTED TO BE EMPTY NOW, AND THAT IS THE POINT. The config
	// excludes _test.go, so the glob walk above cannot reach a test file — which
	// is exactly the property under test. The excluded corpus is loaded
	// separately by excludedTestSources, because the guard still needs to read it
	// to prove the exclusion is doing something.
	if len(nonTest) == 0 {
		t.Fatal("scanned 0 non-test sources — the walk is wired to nothing")
	}
	return nonTest, tests
}

// excludedTestSources returns the _test.go files in this package — the corpus
// Tailwind is configured NOT to scan.
//
// 🔴 IT EXISTS TO MAKE A ZERO INTO A MEASUREMENT. "No test-only class reached
// the stylesheet" is indistinguishable from "there was never a class-shaped
// token in a test file to begin with", and the second would make the guard
// permanently green whatever the config said. Reading the corpus lets the test
// assert there ARE such tokens and that none of them made it through.
func excludedTestSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("found no _test.go files — the exclusion below would be vacuous")
	}
	return out
}

// tailwindContentGlobs reads the quoted entries of tailwind.config.js's
// `content` array, skipping commented-out lines.
//
// ⚠ IT IS A TEXT PARSE, NOT A JS EVALUATION, and its limits are stated rather
// than hidden: an entry built by concatenation or spread would be missed. The
// control in cssScannedSources is what keeps that from being silent — a parse
// that finds fewer than two entries fails the test outright.
func tailwindContentGlobs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../tailwind.config.js")
	if err != nil {
		t.Fatalf("read tailwind.config.js: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "content:")
	if i < 0 {
		t.Fatal("tailwind.config.js has no `content:` key — the build no longer scans anything this " +
			"guard can describe")
	}

	// 🔴 THE ARRAY IS TERMINATED BY A LINE, NOT BY THE NEXT `]` IN THE FILE, AND
	// THAT IS A MEASURED FIX RATHER THAN CAUTION. The first version searched for
	// the next `]` character. The config's comments explain arbitrary-value
	// Tailwind classes and Go slice expressions, so they CONTAIN `]` — the scan
	// stopped inside a comment, returned one glob instead of two, and the
	// positive control in cssScannedSources fired. Loud, and only because that
	// control exists: without it the guard would have silently described half the
	// build. Comment lines are skipped, and the array ends at the first line that
	// closes it.
	var out []string
	closed := false
	lines := strings.Split(src[i:], "\n")
	for n, line := range lines {
		trimmed := strings.TrimSpace(line)
		if n > 0 && strings.HasPrefix(trimmed, "]") {
			closed = true
			break
		}
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		a := strings.Index(trimmed, "'")
		if a < 0 {
			continue
		}
		b := strings.Index(trimmed[a+1:], "'")
		if b < 0 {
			continue
		}
		out = append(out, trimmed[a+1:a+1+b])
	}
	if !closed {
		t.Fatal("tailwind.config.js's content array is unterminated — no line closes it, so this scan " +
			"read to the end of the file and whatever it returned is not the content list")
	}
	return out
}

// globRootAndExts turns a Tailwind content glob into a directory to walk and the
// suffixes to accept, relative to THIS package's directory (tests run there,
// the config paths are repo-relative).
func globRootAndExts(t *testing.T, gl string) (root string, exts []string) {
	t.Helper()
	p := strings.TrimPrefix(gl, "./")
	// Split the fixed prefix from the first wildcard segment.
	segs := strings.Split(p, "/")
	var fixed []string
	var tail string
	for i, s := range segs {
		if strings.ContainsAny(s, "*{") {
			tail = strings.Join(segs[i:], "/")
			break
		}
		fixed = append(fixed, s)
	}
	root = "../../" + strings.Join(fixed, "/")
	if tail == "" {
		// A single named file, e.g. ./internal/api/login.go — walking its own
		// path visits exactly it.
		return root, []string{filepath.Base(p)}
	}
	base := filepath.Base(tail) // "*.go" or "*.{html,js}"
	if k := strings.Index(base, "{"); k >= 0 {
		inner := base[k+1 : strings.Index(base, "}")]
		for _, e := range strings.Split(inner, ",") {
			exts = append(exts, "."+strings.TrimSpace(e))
		}
		return root, exts
	}
	return root, []string{strings.TrimPrefix(base, "*")}
}

// TestTheStylesheetCarriesNoNewTestOnlyClasses is the ratchet.
// TestNoTestOnlyClassReachesTheStylesheet asserts the OUTPUT of the exclusion,
// not the config line that causes it.
//
// 🔴 THIS REPLACED A RATCHET, AND THE REPLACEMENT IS THE POINT. Upstream scanned
// its test files and then maintained a hand-curated LEDGER of the classes that
// leaked as a result — ten entries, each with a note saying why the dead CSS was
// accepted. A ledger like that has to be curated forever, cannot tell an
// accident from a decision, and is a standing invitation to add an eleventh row
// instead of fixing the leak. Excluding _test.go from the scan deletes the whole
// problem: measured on this tree, 38,836 bytes and 7 test-only classes became
// 38,274 bytes and 0, with every css-check control class still present.
//
// So what is asserted now is simply ZERO — no ledger, nothing to curate.
//
// 🔴 AND THE ZERO IS MADE INTO A MEASUREMENT, because "no test file leaked a
// class" reads identically to "no test file contains a class-shaped token" and
// to "this guard is reading the wrong files". Three controls run first: the
// extractor must find hundreds of classes including ones the app really
// renders; most of them must be attributable to a scanned source (or
// cssTokenOccurs is matching nothing and everything looks test-only); and the
// EXCLUDED corpus must itself contain class-shaped tokens that the stylesheet
// does not carry, which is the direct evidence that the exclusion is doing work.
func TestNoTestOnlyClassReachesTheStylesheet(t *testing.T) {
	raw, err := os.ReadFile("../../web/static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v\n"+
			"    It is committed, so this should not happen; if it is genuinely missing, "+
			"`make css` rebuilds it.", err)
	}
	classes := cssClasses(string(raw))

	// CONTROL 1 — the extractor sees a real stylesheet.
	if len(classes) < 200 {
		t.Fatalf("extracted only %d classes from app.css — the extractor is wired to nothing, "+
			"so the empty result below would prove nothing", len(classes))
	}
	for _, live := range []string{"flex", "hidden", "relative"} {
		if !classes[live] {
			t.Errorf("app.css has no .%s rule — either the stylesheet was built with the wrong Tailwind "+
				"(v4 ignores tailwind.config.js and produces a ~20KB file) or the extractor is broken", live)
		}
	}

	nonTest, _ := cssScannedSources(t)
	var testOnly []string
	for c := range classes {
		attributed := false
		for _, src := range nonTest {
			if cssTokenOccurs(c, src) {
				attributed = true
				break
			}
		}
		if !attributed {
			testOnly = append(testOnly, c)
		}
	}
	sort.Strings(testOnly)

	// CONTROL 2 — attribution works at all.
	if len(classes)-len(testOnly) < 100 {
		t.Fatalf("only %d of %d classes were attributed to a scanned source — cssTokenOccurs is "+
			"matching nothing, so the result below is noise", len(classes)-len(testOnly), len(classes))
	}

	// CONTROL 3 — the excluded corpus really does contain class-shaped tokens
	// that are NOT in the stylesheet. Without this, a zero above is consistent
	// with the test files having nothing Tailwind would ever have emitted.
	excluded := excludedTestSources(t)
	suppressed := 0
	for _, probe := range []string{"shrink", "invert", "list-item"} {
		inATest := false
		for _, src := range excluded {
			if cssTokenOccurs(probe, src) {
				inATest = true
				break
			}
		}
		inAScannedSource := false
		for _, src := range nonTest {
			if cssTokenOccurs(probe, src) {
				inAScannedSource = true
				break
			}
		}
		if inATest && !inAScannedSource && !classes[probe] {
			suppressed++
		}
	}
	if suppressed == 0 {
		t.Errorf("none of the probe utilities is present in a _test.go file, absent from every scanned " +
			"source, and absent from app.css — so the zero below is not evidence the exclusion works.\n" +
			"These three were MEASURED in the stylesheet before `!./internal/ui/**/*_test.go` was added " +
			"and absent after. If the test prose that contained them has changed, pick new probes from " +
			"whatever bare utility words the current tests happen to use.")
	}

	for _, c := range testOnly {
		where := "no test file either — it is unattributable"
		for path, src := range excluded {
			if cssTokenOccurs(c, src) {
				where = "written in " + path
				break
			}
		}
		t.Errorf("app.css carries .%s, which no SCANNED source accounts for (%s).\n"+
			"    If it came from a test file, the `!./internal/ui/**/*_test.go` exclusion in "+
			"tailwind.config.js has stopped working — a Tailwind upgrade that drops `!` patterns does "+
			"this SILENTLY, which is why this test reads the output rather than the config line.\n"+
			"    If it came from nowhere, it is hand-written CSS in web/css/input.css that this "+
			"attribution does not cover; add that file to the non-test sources.", c, where)
	}

	t.Logf("%d classes in app.css, %d test-only, %d/3 suppression probes confirmed",
		len(classes), len(testOnly), suppressed)
}

// cssJoin concatenates at RUNTIME. Every fixture below goes through it, for the
// same reason aaVariant exists in auto_approve_header_test.go: this file is
// scanned by Tailwind, so a contiguous `<utility>` / `<variant>:<utility>` /
// `<utility>-<n>` literal in a fixture would be emitted into app.css as dead CSS
// — and this is the guard that would then fail on its own fixture.
func cssJoin(parts ...string) string { return strings.Join(parts, "") }

// TestCSSClassExtractionIsSelectorOnly is the extractor's own negative control:
// it must NOT report the dotted numbers that fill declaration values, and it must
// decode the escapes Tailwind writes. Without this, a phantom entry in the ledger
// above would look exactly like a real leak.
//
// ⚠ The fixture selectors are deliberately NOT real utility names (see cssJoin).
// The extractor cannot tell a utility from any other class, so nothing is lost.
func TestCSSClassExtractionIsSelectorOnly(t *testing.T) {
	cases := []struct {
		name string
		css  string
		want []string
	}{
		{"a decimal in a declaration value", ".zza{transition:opacity .2s;line-height:1.375}", []string{"zza"}},
		{"a compound selector", ".zza.zzb>.zzc{color:red}", []string{"zza", "zzb", "zzc"}},
		{"an at-rule wrapper", "@media (min-width:640px){.zzd{color:red}}", []string{"zzd"}},
		{"an escaped colon", ".zze\\:zzf{color:red}", []string{"zze:zzf"}},
		{"an escaped arbitrary value", ".zzg-\\[qq\\]{color:red}", []string{"zzg-[qq]"}},
		{"a leading-digit class (hex escape + space)", ".\\32 zzh{color:red}", []string{"2zzh"}},
		{"a non-ASCII hex escape", ".zzi-\\2026 j{color:red}", []string{"zzi-…j"}},
		{"an id and a tag, not classes", "#zzx div{color:red}", nil},
	}
	for _, c := range cases {
		got := cssClasses(c.css)
		var names []string
		for n := range got {
			names = append(names, n)
		}
		sort.Strings(names)
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: cssClasses(%q) = %v, want %v", c.name, c.css, names, c.want)
		}
	}
}

// TestCSSTokenOccursRespectsClassBoundaries is cssTokenOccurs' own control. It is
// the predicate that decides whether a class is attributable, and it reports "not
// attributable" by finding NOTHING — so it has to be shown to discriminate.
//
// ⚠ Every token that is a real utility is assembled with cssJoin — see there.
func TestCSSTokenOccursRespectsClassBoundaries(t *testing.T) {
	shrink := cssJoin("shr", "ink")
	maxH80 := cssJoin("max-h", "-80")
	slate := cssJoin("text-slate", "-500")
	slice := cssJoin("[start", ":k]")
	cases := []struct {
		name      string
		tok, text string
		want      bool
	}{
		{"the bare utility", shrink, cssJoin(`Class("`, shrink, `")`), true},
		{"a longer utility is a different class", shrink, cssJoin(`Class("`, shrink, `-0")`), false},
		{"...and so is a longer prefix", shrink, cssJoin(`Class("flex-`, shrink, `")`), false},
		{"a word in prose", "invert", "// prose that says invert.", true},
		{"one of several classes in an attribute", maxH80, cssJoin(`Class("`, maxH80, ` overflow-auto")`), true},
		{"a longer numeric suffix", maxH80, cssJoin(`Class("`, maxH80, `0")`), false},
		{"a slice expression (the token starts with a non-token char)", slice, cssJoin("return src", slice), true},
		{"...and its boundary still holds at the end", slice, cssJoin("return src[start", ":k2]"), false},
		{"an opacity modifier is not a token character", slate, cssJoin(`Class("`, slate, `/70")`), true},
		{"absent", "zznope", "nothing here", false},
	}
	for _, c := range cases {
		if got := cssTokenOccurs(c.tok, c.text); got != c.want {
			t.Errorf("%s: cssTokenOccurs(%q, %q) = %v, want %v", c.name, c.tok, c.text, got, c.want)
		}
	}
}
