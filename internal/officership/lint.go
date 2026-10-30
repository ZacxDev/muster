package officership

import (
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// THE MIGRATION LINT: no destructive (contract) DDL in what an officer merges
// or releases.
//
// Migrations run forward-only at the service's boot, and a code-only revert
// (the automatic rollback) leaves the schema as the release left it. So the
// officer may only EXPAND a schema: a contract step is refused, and a human
// writes it.
//
// Destructive, refused:
//
//   - a DROP, classified by what it is attached to: under ALTER [COLUMN] c,
//     anything but DEFAULT, NOT NULL, IDENTITY, EXPRESSION; as an ALTER TABLE
//     action, anything but a CONSTRAINT the SAME file re-adds by name (the
//     CHECK-widening idiom) — so `DROP COLUMN c`, `DROP c`, `DROP "c"`,
//     `DROP index` (a column named index) are all column drops; as a
//     statement (or inside a DO body), anything but DROP INDEX;
//   - any DROP … CASCADE;
//   - RENAME (a table, a column, a constraint, a value);
//   - ALTER … TYPE / SET DATA TYPE on a column;
//   - SET NOT NULL;
//   - TRUNCATE;
//   - DELETE or UPDATE with no WHERE in the same statement.
//
// 🔴 COMMENTS ARE REMOVED, STRINGS ARE NOT. A `--` or `/* */` comment is
// stripped by a scanner that knows quoting, so `'--'; DROP TABLE x` is still
// seen. String literals and dollar-quoted bodies are SCANNED, so `EXECUTE
// 'DROP TABLE x'` inside a DO block is refused too. That costs a false
// positive on a CHECK whose literal spells a keyword; a false positive is an
// escalation to a human, a false negative is lost data.
// ---------------------------------------------------------------------------

// Finding is one destructive statement in a migration file.
type Finding struct {
	File string
	Rule string
	Text string // the normalised statement, bounded
}

// stripSQLComments returns sql with every comment replaced by a space,
// honouring '…' and "…" quoting, E” escapes and $tag$…$tag$ bodies (whose
// contents are kept).
func stripSQLComments(sql string) string {
	var b strings.Builder
	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case c == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < n && sql[i+1] == '*':
			depth := 0
			for i < n {
				if i+1 < n && sql[i] == '/' && sql[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if i+1 < n && sql[i] == '*' && sql[i+1] == '/' {
					depth--
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				i++
			}
			b.WriteByte(' ')
		case c == '\'' || c == '"':
			j := i + 1
			for j < n {
				if sql[j] == '\\' && c == '\'' && i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && j+1 < n {
					j += 2
					continue
				}
				if sql[j] == c {
					if j+1 < n && sql[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j < n {
				j++
			}
			b.WriteString(sql[i:j])
			i = j
		case c == '$':
			if tag := dollarTag(sql[i:]); tag != "" {
				end := strings.Index(sql[i+len(tag):], tag)
				j := n
				if end >= 0 {
					j = i + len(tag) + end + len(tag)
				}
				b.WriteString(sql[i:j])
				i = j
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

var dollarTagRE = regexp.MustCompile(`^\$[A-Za-z_]?[A-Za-z0-9_]*\$`)

func dollarTag(s string) string { return dollarTagRE.FindString(s) }

var (
	wsRE           = regexp.MustCompile(`\s+`)
	onCommitDropRE = regexp.MustCompile(`\bON\s+COMMIT\s+DROP\b`)
	dropRE         = regexp.MustCompile(`\bDROP\s+("[^"]*"|\w+)(\s+\w+)?`)
	// alterColumnTailRE: the text before a DROP ends in `ALTER [COLUMN] <col>`
	// — the DROP is a column PROPERTY (DEFAULT, NOT NULL, IDENTITY, EXPRESSION).
	alterColumnTailRE = regexp.MustCompile(`\bALTER\s+(?:COLUMN\s+)?(?:"[^"]*"|\w+)\s*$`)
	// alterTableTailRE: the text before a DROP ends in `ALTER TABLE <t>`, or
	// the DROP follows a comma inside an ALTER TABLE — it is a table ACTION.
	alterTableTailRE = regexp.MustCompile(`\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?(?:"[^"]*"|[\w.]+)\s*\*?\s*$`)
	alterTableRE     = regexp.MustCompile(`\bALTER\s+TABLE\b`)
	dropConRE        = regexp.MustCompile(`\bDROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?("[^"]+"|\w+)`)
	addConRE         = regexp.MustCompile(`\bADD\s+CONSTRAINT\s+("[^"]+"|\w+)`)
	cascadeRE        = regexp.MustCompile(`\bDROP\s[^,]*?\bCASCADE\b`)
	renameRE         = regexp.MustCompile(`\bRENAME\b`)
	alterTypeRE      = regexp.MustCompile(`\bALTER\s+(?:COLUMN\s+)?(?:"[^"]+"|\w+)\s+(?:SET\s+DATA\s+)?TYPE\b`)
	setNotNullRE     = regexp.MustCompile(`\bSET\s+NOT\s+NULL\b`)
	truncateRE       = regexp.MustCompile(`\bTRUNCATE\b`)
	deleteRE         = regexp.MustCompile(`\bDELETE\s+FROM\b`)
	updateRE         = regexp.MustCompile(`\bUPDATE\s+(?:ONLY\s+)?(?:"[^"]+"|[\w.]+)\s+(?:AS\s+\w+\s+)?SET\b`)
	whereRE          = regexp.MustCompile(`\bWHERE\b`)
	// columnPropertyDrops are the DROPs under ALTER [COLUMN] c that remove no data.
	columnPropertyDrops = map[string]bool{"DEFAULT": true, "NOT": true, "IDENTITY": true, "EXPRESSION": true}
)

// LintMigration returns every destructive statement in one migration file.
// An empty result is a pass.
func LintMigration(file, sql string) []Finding {
	text := strings.ToUpper(wsRE.ReplaceAllString(stripSQLComments(sql), " "))
	var out []Finding
	add := func(rule, stmt string) {
		out = append(out, Finding{File: file, Rule: rule, Text: truncate(strings.TrimSpace(stmt), 160)})
	}
	// Constraints the file re-adds, by name (quotes kept, case folded above).
	readded := map[string]bool{}
	for _, m := range addConRE.FindAllStringSubmatch(text, -1) {
		readded[m[1]] = true
	}
	for _, stmt := range strings.Split(text, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		// `CREATE TEMP TABLE … ON COMMIT DROP` drops a temp table at commit: not
		// a schema change.
		//
		// 🔴 EACH DROP IS CLASSIFIED BY WHAT IT IS ATTACHED TO (round-1 F4), never
		// by the word after it alone: in Postgres `COLUMN` is optional, so
		// `ALTER TABLE t DROP index` and `ALTER TABLE t DROP "x"` drop columns.
		//   - under ALTER [COLUMN] c: a column property; DEFAULT, NOT NULL,
		//     IDENTITY and EXPRESSION pass;
		//   - an ALTER TABLE action (after `ALTER TABLE t` or a comma inside an
		//     ALTER TABLE): CONSTRAINT is checked for its re-add below, and
		//     ANYTHING else is a column drop;
		//   - anything else (a DROP statement, or one inside a DO body): DROP
		//     INDEX passes, every other object is refused.
		scan := onCommitDropRE.ReplaceAllString(stmt, "ON COMMIT")
		for _, loc := range dropRE.FindAllStringSubmatchIndex(scan, -1) {
			word := scan[loc[2]:loc[3]]
			next := ""
			if loc[4] >= 0 {
				next = strings.TrimSpace(scan[loc[4]:loc[5]])
			}
			before := scan[:loc[0]]
			clause := before
			if i := strings.LastIndexByte(before, ','); i >= 0 {
				clause = before[i+1:]
			}
			switch {
			case alterColumnTailRE.MatchString(clause) && !alterTableTailRE.MatchString(clause):
				if !columnPropertyDrops[word] || (word == "NOT" && next != "NULL") {
					add("DROP "+word+" on a column", stmt)
				}
			case alterTableTailRE.MatchString(clause) || (alterTableRE.MatchString(before) && strings.TrimSpace(clause) == ""):
				if word != "CONSTRAINT" {
					add("DROP COLUMN", stmt)
				}
			default:
				if word != "INDEX" {
					add("DROP "+word, stmt)
				}
			}
		}
		for _, m := range dropConRE.FindAllStringSubmatch(stmt, -1) {
			if !readded[m[1]] {
				add("DROP CONSTRAINT without re-adding "+m[1], stmt)
			}
		}
		if cascadeRE.MatchString(onCommitDropRE.ReplaceAllString(stmt, "ON COMMIT")) {
			add("DROP … CASCADE", stmt)
		}
		if renameRE.MatchString(stmt) {
			add("RENAME", stmt)
		}
		if alterTypeRE.MatchString(stmt) {
			add("ALTER … TYPE", stmt)
		}
		if setNotNullRE.MatchString(stmt) {
			add("SET NOT NULL", stmt)
		}
		if truncateRE.MatchString(stmt) {
			add("TRUNCATE", stmt)
		}
		if deleteRE.MatchString(stmt) && !whereRE.MatchString(stmt) {
			add("DELETE without WHERE", stmt)
		}
		if updateRE.MatchString(stmt) && !whereRE.MatchString(stmt) {
			add("UPDATE without WHERE", stmt)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rule < out[j].Rule })
	return out
}
