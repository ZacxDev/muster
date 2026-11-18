package officership

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// The draft gate's fourth round: the env credential pair is judged only
// inside one YAML item as the hunk shows it WITH its context lines, never
// across items (an added secret-ref entry next to a changed harmless value is
// not a pair); and the pair is found in the realistic shapes outside a
// dash-led block item (a JSON6902 env add, a flow list, a commented quoted
// name). Every value is synthetic.
// ---------------------------------------------------------------------------

const r4Deploy = "clusters/prod/apps/web/deploy.yaml"

// r4OneHunk adds a secretKeyRef env entry (lines added) right above an
// existing harmless entry whose literal value changes.
const r4OneHunk = "@@ -18,4 +18,7 @@ spec:\n" +
	"           env:\n" +
	"+            - name: API_TOKEN\n" +
	"+              valueFrom:\n" +
	"+                secretKeyRef: {name: web-secrets, key: token}\n" +
	"             - name: LOG_LEVEL\n" +
	"-              value: info\n" +
	"+              value: debug\n" +
	"           ports:"

// r4TwoHunks is the same change split over two hunks of one file.
const r4TwoHunks = "@@ -18,3 +18,6 @@ spec:\n" +
	"           env:\n" +
	"+            - name: API_TOKEN\n" +
	"+              valueFrom:\n" +
	"+                secretKeyRef: {name: web-secrets, key: token}\n" +
	"             - name: DB_HOST\n" +
	"               value: db.example.com\n" +
	"@@ -30,4 +33,4 @@ spec:\n" +
	"             - name: CACHE_SIZE\n" +
	"               value: \"512\"\n" +
	"             - name: LOG_LEVEL\n" +
	"-              value: info\n" +
	"+              value: debug\n"

// r4LiteralPair is the real thing: the added entry's own value is a literal.
const r4LiteralPair = "@@ -18,4 +18,6 @@ spec:\n" +
	"           env:\n" +
	"+            - name: API_TOKEN\n" +
	"+              value: plainword\n" +
	"             - name: LOG_LEVEL\n" +
	"-              value: info\n" +
	"+              value: debug\n" +
	"           ports:"

// TestAPairIsNeverMadeAcrossEnvItems (🟡1): the added lines alone, without
// their context, put `value: debug` under the new API_TOKEN entry. The pair is
// judged in the hunk's own items, so the change is admitted; the literal pair
// is still refused (control).
func TestAPairIsNeverMadeAcrossEnvItems(t *testing.T) {
	for name, c := range map[string]struct {
		patch string
		ok    bool
	}{
		"one hunk":                 {patch: r4OneHunk, ok: true},
		"two hunks":                {patch: r4TwoHunks, ok: true},
		"a literal pair (control)": {patch: r4LiteralPair},
	} {
		f := File{Path: r4Deploy, Status: "modified", Changes: 4, Patch: c.patch}
		if paths, why := ForbiddenFiles(nil, []File{f}); c.ok != (len(paths) == 0) {
			t.Errorf("%s: ForbiddenFiles = %v %q, want ok=%v", name, paths, why, c.ok)
		}
		r := newDraftRig()
		r.withFiles(f)
		err := r.create(goodCreate())
		if c.ok != (err == nil) || (!c.ok && !isConflict(err)) {
			t.Errorf("%s: create err = %v, want ok=%v (a 409 when refused)", name, err, c.ok)
		}
	}
	// On a push to the open pull request the false pair used to CLOSE it.
	for name, c := range map[string]struct {
		patch  string
		closed int
	}{
		"one hunk":                 {patch: r4OneHunk},
		"a literal pair (control)": {patch: r4LiteralPair, closed: 1},
	} {
		r := pushRig()
		cmp := r.infra.compares["trunk..."+oidB]
		cmp.Files = []File{{Path: r4Deploy, Status: "modified", Changes: 4, Patch: c.patch}}
		r.infra.compares["trunk..."+oidB] = cmp
		r.mustPush(t, dInfra, "refs/heads/"+dBranch)(context.Background(), applied("refs/heads/"+dBranch))
		if len(r.infra.closed) != c.closed {
			t.Errorf("push %s: closed %v, want %d closed", name, r.infra.closed, c.closed)
		}
	}
}

// TestAnEnvPairOutsideABlockItemIsRefused (🟡2): the env credential pair in
// a kustomize JSON6902 patch (a nested map or a flow map under value:), a
// flow list, and a quoted name followed by a comment is refused; the same
// shapes with a secret reference or a harmless name are admitted (controls).
func TestAnEnvPairOutsideABlockItemIsRefused(t *testing.T) {
	add := func(lines ...string) File {
		p := "@@ -0,0 +1," + "9 @@\n"
		for _, l := range lines {
			p += "+" + l + "\n"
		}
		return File{Path: "clusters/prod/apps/web/env-patch.yaml", Status: "added", Changes: len(lines), Patch: p}
	}
	op := []string{"- op: add", "  path: /spec/template/spec/containers/0/env/-"}
	for name, c := range map[string]struct {
		f  File
		ok bool
	}{
		"json6902 nested map":     {f: add(append(op, "  value:", "    name: DB_PASSWORD", "    value: plainword")...)},
		"json6902 value first":    {f: add(append(op, "  value:", "    value: plainword", "    name: DB_PASSWORD")...)},
		"json6902 flow map":       {f: add(append(op, "  value: {name: DB_PASSWORD, value: plainword}")...)},
		"a flow list":             {f: add("env: [{name: X_TOKEN, value: \"plainword\"}]")},
		"a flow list, 2 items":    {f: add("env: [{name: DB_HOST, value: db}, {name: X_TOKEN, value: plainword}]")},
		"a commented quoted name": {f: add("- name: \"DB_PASSWORD\" # the database", "  value: plainword")},
		"json6902 valueFrom (control)": {f: add(append(op, "  value:", "    name: DB_PASSWORD", "    valueFrom:",
			"      secretKeyRef: {name: db, key: password}")...), ok: true},
		"json6902 harmless (control)":        {f: add(append(op, "  value:", "    name: DB_HOST", "    value: plainword")...), ok: true},
		"a flow list across items (control)": {f: add("env: [{name: X_TOKEN, valueFrom: x}, {name: DB_HOST, value: plainword}]"), ok: true},
	} {
		if paths, why := ForbiddenFiles(nil, []File{c.f}); c.ok != (len(paths) == 0) {
			t.Errorf("%s: ForbiddenFiles = %v %q, want ok=%v", name, paths, why, c.ok)
		}
	}
}
