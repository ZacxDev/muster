package officership

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/redact"
)

// ---------------------------------------------------------------------------
// The draft gate's CONTENT RULES over GitHub's comparison (ForbiddenFiles,
// removedKustomizeEntries) and the reading of a push's report-status
// (parseReportStatus). Pure functions: checkComparison (draftgate.go) runs
// them, and reads what the kustomization rule needs from the base.
// ---------------------------------------------------------------------------

// removedKindRE are the manifest lines whose REMOVAL a draft officer may never
// propose: deleting one deletes data or a running service's rollout state.
//
// 🔴 BEST EFFORT, AN EARLY REFUSAL — NOT THE WALL. These rules read GitHub's
// diff, so they catch the shapes they name and miss others that delete or
// rename the same resources (a dropped Flux Kustomization or HelmRelease that
// prunes them, a kustomize namespace:/namePrefix:/nameSuffix: transformer, a
// JSON6902 patch of metadata.name, a flow-style resources: list). The
// AUTHORITATIVE wall against deleting or renaming a stateful resource is the
// TARGET repository's own CI: a render diff, gating every officer/* pull
// request, that compares what the base and the head render to. muster's rule
// refuses the plain cases early; it never replaces that gate.
var removedKindRE = []*regexp.Regexp{
	regexp.MustCompile(`["']?\bkind["']?\s*:\s*["']?(PersistentVolumeClaim|PersistentVolume|Canary)\b`),
	regexp.MustCompile(`["']?\bapiVersion["']?\s*:\s*["']?postgresql\.cnpg\.io/`),
}

// protectedKind is the protected-kind declaration line l holds, "" when none.
func protectedKind(l string) string {
	for _, re := range removedKindRE {
		if m := re.FindString(l); m != "" {
			return strings.TrimSpace(m)
		}
	}
	return ""
}

var (
	// rePatchDelete is a kustomize strategic-merge `$patch: delete`.
	rePatchDelete = regexp.MustCompile(`["']?\$patch["']?\s*:\s*["']?delete\b`)
	// reNameLine is a `name:` key on an indented line (metadata.name, and
	// any other name: of the document).
	reNameLine = regexp.MustCompile(`^\s+["']?name["']?\s*:`)
	// reTopKey is a top-level YAML key.
	reTopKey = regexp.MustCompile(`^["']?([A-Za-z_$][\w.$-]*)["']?\s*:`)
	// reListItem is a list item's scalar; reMapItem a list item that is a
	// mapping (`- path: x`), which is not a kustomization resource entry.
	reListItem = regexp.MustCompile(`^\s*-\s+["']?([^"'\s#]+)["']?\s*(?:#.*)?$`)
	reMapItem  = regexp.MustCompile(`^\s*-\s+["']?[A-Za-z_][\w.-]*["']?\s*:(?:\s|$)`)
	// reSecretKind is a `kind: Secret` line.
	reSecretKind = regexp.MustCompile(`^["']?kind["']?\s*:\s*["']?Secret["']?\s*(?:#.*)?$`)
	// reDataKV is an indented key: value line of a Secret's data.
	reDataKV = regexp.MustCompile(`^\s+["']?[^"':#\s][^:#]*["']?\s*:\s*(.*)$`)
)

// patchLine is one line of a file's unified diff: op is '+', '-', ' ' or '@'
// (a hunk header), text the line without its op.
type patchLine struct {
	op   byte
	text string
}

func patchLines(patch string) []patchLine {
	var out []patchLine
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			out = append(out, patchLine{'@', l})
		case l == "":
			out = append(out, patchLine{' ', ""})
		case l[0] == '+' || l[0] == '-' || l[0] == ' ':
			out = append(out, patchLine{l[0], l[1:]})
		case l[0] == '\\': // "\ No newline at end of file"
		default:
			out = append(out, patchLine{' ', l})
		}
	}
	return out
}

// ForbiddenFiles is THE content rule of a draft officer's comparison: the
// paths that make it refused and why (none: admitted). A file is refused when
//
//   - its path or its old path matches a forbidden glob;
//   - it is a .gitattributes file, in any directory, added, modified, removed
//     or renamed (one can mark manifests -diff or binary, and GitHub then
//     shows their changes as no diff);
//   - GitHub shows no diff for it (an empty patch): a binary file (GitHub
//     reports changes 0), a large file (changes > 0), a pure rename or copy,
//     a mode change — none can be checked. The one exception is a REMOVED
//     file with changes 0 (a binary or empty file — or a manifest an
//     attribute made look like one), which passes HERE and is judged by
//     checkComparison from its content at the merge base (removedNoDiffWhy);
//     its path rule still applies;
//   - a removed line declares a PersistentVolumeClaim, a PersistentVolume, a
//     CloudNativePG resource or a Flagger Canary (removedKindRE);
//   - an added line is a kustomize `$patch: delete` (whatever it targets: its
//     kind may sit outside the hunk);
//   - a removed `name:` line in a diff that shows a protected kind (renaming a
//     volume, database or canary deletes it and creates another);
//   - an added line has a secret-shaped value (redact.HasSecretLiteralIn,
//     which judges a credential-like KEY as well as a value), or one YAML
//     mapping's credential-like name: has a literal value: (an env entry, a
//     JSON6902 env add, a flow mapping; redact.HasSecretPairIn, judged on
//     each hunk's post-image WITH its context lines, so an unchanged name
//     line counts and a pair is never made across two entries);
//   - an added kind: Secret holds a literal data/stringData value (plainSecret).
//
// ⚠ BEST EFFORT, BY THE DIFF: a hunk shows three lines of context, so a rule
// that needs the document's kind sees it only when the hunk does. The
// kustomization rule (an entry dropped from resources:) needs the base, and is
// checkComparison's. The deletion rules are an early refusal; the wall is the
// target repository's render-diff CI gate (removedKindRE).
func ForbiddenFiles(forbidden []string, files []File) ([]string, string) {
	var hit []string
	var why []string
	add := func(p, reason string) {
		hit = append(hit, p)
		why = append(why, p+": "+reason)
	}
	for _, f := range files {
		for _, p := range []string{f.Path, f.Previous} {
			if p == "" {
				continue
			}
			if path.Base(p) == ".gitattributes" {
				add(p, "a .gitattributes change (an attribute can mark files -diff or binary and so hide a file's diff from the check)")
			}
			for _, pat := range forbidden {
				if officers.MatchForbidden(pat, p) {
					add(p, fmt.Sprintf("a forbidden path (%s)", pat))
					break
				}
			}
		}
		if f.Patch == "" {
			switch {
			case f.Status == "removed" && f.Changes == 0:
			case f.Changes > 0:
				add(f.Path, "GitHub withholds its diff (a large file), so it cannot be checked")
			default:
				add(f.Path, fmt.Sprintf("GitHub shows no diff for it (%s with no patch: a binary file, a pure rename or copy, or a mode change), "+
					"so it cannot be checked", orUnknown(f.Status)))
			}
			continue
		}
		lines := patchLines(f.Patch)
		var added []string
		removedKind, shownKind, removedName, patchDelete := "", "", false, false
		for _, l := range lines {
			switch l.op {
			case '+':
				added = append(added, l.text)
				patchDelete = patchDelete || rePatchDelete.MatchString(l.text)
			case '-':
				if removedKind == "" {
					removedKind = protectedKind(l.text)
				}
				removedName = removedName || reNameLine.MatchString(l.text)
			}
			if shownKind == "" && l.op != '@' {
				shownKind = protectedKind(l.text)
			}
		}
		if removedKind != "" {
			add(f.Path, fmt.Sprintf("it removes a %q line (a volume, database or canary must never be deleted by an officer)", removedKind))
		} else if removedName && shownKind != "" {
			add(f.Path, fmt.Sprintf("it changes a name: in a %q manifest (renaming a volume, database or canary deletes it)", shownKind))
		}
		if patchDelete {
			add(f.Path, "an added line is a kustomize `$patch: delete` (an officer never deletes a resource)")
		}
		// The added lines spliced together are not a whole text (the lines
		// between them are missing, so one env entry's value would pair with
		// another's name): the value and key rules judge them, and the pair
		// rule judges each hunk's post-image (secretPair).
		if redact.HasSecretLiteralIn(f.Path, strings.Join(added, "\n")) || secretPair(f.Path, lines) {
			add(f.Path, "an added line has a credential-shaped value")
		}
		if plainSecret(lines) {
			add(f.Path, "it adds a literal data/stringData value to a kind: Secret (an unencrypted Secret; encrypt it with SOPS)")
		}
	}
	if len(hit) == 0 {
		return nil, ""
	}
	return sortedUnique(hit), strings.Join(why, "; ")
}

// secretPair reports whether the post-image of a file's diff (its context and
// added lines) holds a credential env pair one of whose lines is added
// (redact.HasSecretPairIn), each hunk judged on its own.
func secretPair(p string, lines []patchLine) bool {
	var post []string
	var changed []bool
	flush := func() bool {
		hit := redact.HasSecretPairIn(p, post, changed)
		post, changed = post[:0], changed[:0]
		return hit
	}
	for _, l := range lines {
		switch l.op {
		case '@':
			if flush() {
				return true
			}
		case '+', ' ':
			post, changed = append(post, l.text), append(changed, l.op == '+')
		}
	}
	return flush()
}

func orUnknown(s string) string {
	if s == "" {
		return "a file"
	}
	return strconv.Quote(s)
}

// plainSecret reports whether a diff that shows `kind: Secret` adds a literal
// value inside its top-level data: or stringData: block — any value but a
// SOPS-encrypted ENC[…] one. The block is known from the diff's own lines
// (a hunk starts not knowing it: an added value whose block the hunk does not
// show is not judged here; redact's key rule still is).
func plainSecret(lines []patchLine) bool {
	secret := false
	for _, l := range lines {
		if l.op != '-' && l.op != '@' && reSecretKind.MatchString(l.text) {
			secret = true
		}
	}
	if !secret {
		return false
	}
	section := ""
	for _, l := range lines {
		switch {
		case l.op == '@':
			section = ""
			continue
		case l.op == '-':
			continue
		}
		if m := reTopKey.FindStringSubmatch(l.text); m != nil && !strings.HasPrefix(l.text, " ") && !strings.HasPrefix(l.text, "\t") {
			section = m[1]
			if l.op == '+' && (section == "data" || section == "stringData") {
				if _, v, _ := strings.Cut(l.text, ":"); literalData(v) && strings.Contains(v, ":") {
					return true // an inline {key: value} block
				}
			}
			continue
		}
		if l.op != '+' || (section != "data" && section != "stringData") {
			continue
		}
		if m := reDataKV.FindStringSubmatch(l.text); m != nil {
			if literalData(m[1]) {
				return true
			}
		} else if strings.TrimSpace(l.text) != "" && !strings.HasPrefix(strings.TrimSpace(l.text), "#") &&
			!strings.HasPrefix(strings.TrimSpace(l.text), "ENC[") {
			return true // a block scalar's line
		}
	}
	return false
}

// literalData reports whether v (a Secret data value, comments included) is a
// literal: anything non-empty that is not a SOPS ENC[…] value.
func literalData(v string) bool {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	v = strings.Trim(v, `"'`)
	return v != "" && !strings.HasPrefix(v, "ENC[")
}

// isKustomization reports whether p is a kustomization file.
func isKustomization(p string) bool {
	switch path.Base(p) {
	case "kustomization.yaml", "kustomization.yml", "Kustomization":
		return true
	}
	return false
}

// removedKustomizeEntries are the repository paths a kustomization file's diff
// DROPS from its resources:, bases: or components: lists (an entry whose
// section the hunk does not show counts as one: fail closed), resolved against
// the file's directory. An entry the diff adds back is not dropped. A remote
// entry or an absolute path is returned as it is written (not a file muster
// can read: refused).
func removedKustomizeEntries(f File) []string {
	if !isKustomization(f.Path) && !isKustomization(f.Previous) {
		return nil
	}
	lines := patchLines(f.Patch)
	addedBack := map[string]bool{}
	for _, l := range lines {
		if l.op == '+' {
			if m := reListItem.FindStringSubmatch(l.text); m != nil {
				addedBack[m[1]] = true
			}
		}
	}
	var out []string
	section := ""
	for _, l := range lines {
		if l.op == '@' {
			section = ""
			continue
		}
		if m := reTopKey.FindStringSubmatch(l.text); m != nil && !strings.HasPrefix(l.text, " ") && !strings.HasPrefix(l.text, "\t") {
			section = m[1]
			continue
		}
		if l.op != '-' || reMapItem.MatchString(l.text) {
			continue
		}
		m := reListItem.FindStringSubmatch(l.text)
		if m == nil || addedBack[m[1]] {
			continue
		}
		switch section {
		case "", "resources", "bases", "components":
		default:
			continue
		}
		entry := m[1]
		if strings.Contains(entry, "://") || strings.HasPrefix(entry, "/") || strings.HasPrefix(entry, "github.com/") {
			out = append(out, entry)
			continue
		}
		out = append(out, path.Join(path.Dir(f.Path), entry))
	}
	return out
}

// parseReportStatus reads a receive-pack response body (report-status, plain
// or multiplexed on side band 1) into ref -> applied ("ok" true, "ng" false).
// A ref it does not name — no report, a truncated body — is absent: unknown.
func parseReportStatus(body []byte) map[string]bool {
	out := map[string]bool{}
	for _, line := range reportStatusLines(body) {
		if ref, ok := strings.CutPrefix(line, "ok "); ok {
			out[ref] = true
		} else if rest, ok := strings.CutPrefix(line, "ng "); ok {
			ref, _, _ := strings.Cut(rest, " ")
			out[ref] = false
		}
	}
	return out
}

// reportStatusLines is a receive-pack response body's report-status lines
// (plain, or multiplexed on side band 1), newline trimmed.
func reportStatusLines(body []byte) []string {
	var out []string
	pkts := readPkts(body)
	if len(pkts) > 0 && len(pkts[0]) > 0 && pkts[0][0] >= 1 && pkts[0][0] <= 3 {
		var inner []byte
		for _, p := range pkts {
			if len(p) > 0 && p[0] == 1 {
				inner = append(inner, p[1:]...)
			}
		}
		pkts = readPkts(inner)
	}
	for _, p := range pkts {
		out = append(out, strings.TrimSuffix(string(p), "\n"))
	}
	return out
}

// readPkts is b's pkt-line payloads up to the first flush-pkt (or the first
// malformed or truncated packet).
func readPkts(b []byte) [][]byte {
	var out [][]byte
	for len(b) >= 4 {
		n, err := strconv.ParseUint(string(b[:4]), 16, 16)
		if err != nil || n == 0 || n < 4 || int(n) > len(b) {
			break
		}
		out = append(out, b[4:n])
		b = b[n:]
	}
	return out
}

// sortedUnique is in sorted, without repeats.
func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	s := append([]string(nil), in...)
	sort.Strings(s)
	out := s[:1]
	for _, x := range s[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}
