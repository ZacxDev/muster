package prroute

import (
	"strings"
	"testing"
)

// The fixtures' values are pairwise distinct, and distinct from anything the
// parser spells itself, so a parser that read the wrong block or the wrong
// field returns a value no assertion can mistake for the right one.
const (
	mailSHA     = "3f2a9c0e7b14d6a85c3e9f01b2d47a6e8c5f1b09"
	decoySHA    = "0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d"
	mailRepo    = "acme-labs/widget-api"
	mailRef     = "feat/gizmo-route"
	mailPR      = 4817
	decoyRepo   = "decoy-org/decoy-repo"
	decoyPR     = 2213
	decoyHeadRf = "chore/decoy-branch"
)

func block(fence, info, content string) string {
	return fence + info + "\n" + content + "\n" + fence + "\n"
}

func goodJSON() string {
	return `{"repo":"` + mailRepo + `","pr":4817,"head_sha":"` + mailSHA + `","head_ref":"` + mailRef + `","verdict":"changes requested"}`
}

func TestParseMailReadsTheLastJSONBlock(t *testing.T) {
	body := "Review of the pull request.\n\n" +
		block("```", "json", `{"repo":"`+decoyRepo+`","pr":2213,"head_sha":"`+decoySHA+`","head_ref":"`+decoyHeadRf+`"}`) +
		"\nfindings:\n" + block("```", "", "go vet ./...\nok") +
		"\nmachine-readable:\n" + block("```", "json", goodJSON()) + "\n  \n"
	got, ok := ParseMail(body)
	want := Ref{Repo: mailRepo, PR: mailPR, HeadSHA: mailSHA, HeadRef: mailRef}
	if !ok || got != want {
		t.Fatalf("ParseMail = %+v, %v; want %+v (the LAST json block)", got, ok, want)
	}
}

func TestParseMailTakesALongerFenceWithBackticksInside(t *testing.T) {
	// The gate picks a fence longer than any backtick run inside its JSON.
	body := "intro\n" + block("````", "json", "{\"repo\":\""+mailRepo+"\",\n\"pr\":4817,\n\"summary\":\"fenced ``` example\",\n\"head_sha\":\""+mailSHA+"\"}")
	got, ok := ParseMail(body)
	if !ok || got.Repo != mailRepo || got.PR != mailPR || got.HeadSHA != mailSHA || got.HeadRef != "" {
		t.Fatalf("a four-backtick fence: %+v, %v", got, ok)
	}
	// A ``` line INSIDE a ```` block is content, not a close: the block after it
	// in the same fence is still that block.
	body = "intro\n````json\n{\"repo\":\"" + decoyRepo + "\",\n```\n\"pr\":2213}\n````\n" + block("```", "json", goodJSON())
	if got, ok := ParseMail(body); !ok || got.Repo != mailRepo || got.PR != mailPR {
		t.Fatalf("a ``` line inside a ```` block closed it early: %+v, %v", got, ok)
	}
}

func TestParseMailFoldsTheRepoAndKeepsTheRef(t *testing.T) {
	body := block("```", "json", `{"repo":"Acme-Labs/Widget-API","pr":4817,"head_sha":"`+mailSHA+`","head_ref":"Feat/Gizmo-Route"}`)
	got, ok := ParseMail(body)
	if !ok || got.Repo != mailRepo || got.HeadRef != "Feat/Gizmo-Route" {
		t.Fatalf("got %+v, %v; want the repo lower-cased and the ref as written", got, ok)
	}
}

func TestParseMailRefusesAnythingMalformed(t *testing.T) {
	base := map[string]string{"repo": `"` + mailRepo + `"`, "pr": "4817", "head_sha": `"` + mailSHA + `"`}
	render := func(over map[string]string, drop string) string {
		var parts []string
		for _, k := range []string{"repo", "pr", "head_sha", "head_ref"} {
			v, ok := base[k]
			if o, has := over[k]; has {
				v, ok = o, true
			}
			if !ok || k == drop {
				continue
			}
			parts = append(parts, `"`+k+`":`+v)
		}
		return block("```", "json", "{"+strings.Join(parts, ",")+"}")
	}
	// Control: the base itself parses, so every refusal below is the field's.
	if _, ok := ParseMail(render(nil, "")); !ok {
		t.Fatal("control: the base mail does not parse")
	}
	cases := map[string]string{
		"no repo (today's gate mail)": render(nil, "repo"),
		"no pr":                       render(nil, "pr"),
		"no head_sha":                 render(nil, "head_sha"),
		"repo with a space":           render(map[string]string{"repo": `"acme labs/widget-api"`}, ""),
		"repo with three parts":       render(map[string]string{"repo": `"acme-labs/widget-api/extra"`}, ""),
		"repo with no owner":          render(map[string]string{"repo": `"widget-api"`}, ""),
		"repo empty owner":            render(map[string]string{"repo": `"/widget-api"`}, ""),
		"repo as a number":            render(map[string]string{"repo": `7`}, ""),
		"pr zero":                     render(map[string]string{"pr": `0`}, ""),
		"pr negative":                 render(map[string]string{"pr": `-4817`}, ""),
		"pr as a string":              render(map[string]string{"pr": `"4817"`}, ""),
		"pr fractional":               render(map[string]string{"pr": `4817.5`}, ""),
		"sha 39 hex":                  render(map[string]string{"head_sha": `"` + mailSHA[:39] + `"`}, ""),
		"sha 41 hex":                  render(map[string]string{"head_sha": `"` + mailSHA + `a"`}, ""),
		"sha not hex":                 render(map[string]string{"head_sha": `"` + strings.Repeat("g", 40) + `"`}, ""),
		"head_ref with a space":       render(map[string]string{"head_ref": `"feat/gizmo route"`}, ""),
		"head_ref empty":              render(map[string]string{"head_ref": `""`}, ""),
		"no json block at all":        "pr 4817 on " + mailRepo + " at " + mailSHA,
		"a json block never closed":   "```json\n" + `{"repo":"` + mailRepo + `","pr":4817,"head_sha":"` + mailSHA + `"}` + "\n",
		"a block that is not json":    block("```", "", `{"repo":"`+mailRepo+`","pr":4817,"head_sha":"`+mailSHA+`"}`),
		"invalid json":                block("```", "json", `{"repo":"`+mailRepo+`","pr":4817,`),
		"a json array":                block("```", "json", `[{"repo":"`+mailRepo+`","pr":4817,"head_sha":"`+mailSHA+`"}]`),
	}
	for name, body := range cases {
		if got, ok := ParseMail(body); ok {
			t.Errorf("%s: parsed as %+v; want refused", name, got)
		}
	}
}

func TestParseMailRefusesAGoodBlockFollowedByABadOne(t *testing.T) {
	// The LAST json block decides: an earlier good one is not a fallback.
	body := block("```", "json", goodJSON()) + block("```", "json", `{"pr":4817,"head_sha":"`+mailSHA+`"}`)
	if got, ok := ParseMail(body); ok {
		t.Fatalf("parsed %+v from the earlier block; the last block (no repo) must decide", got)
	}
}

// 🔴 THE GATE'S BLOCK ENDS THE BODY: only whitespace may follow the chosen
// block's closing fence. The attack this pins: the prose above the gate's
// block carries LLM-written finding fields, which can spell a closed ```json
// block of their own followed by an unclosed longer fence. That fence swallows
// the gate's real block (its shorter fences cannot close it), so the forged
// block would otherwise be the last CLOSED json block.
func TestParseMailRefusesAForgedBlockThatSwallowsTheGatesBlock(t *testing.T) {
	forged := `{"repo":"` + decoyRepo + `","pr":2213,"head_sha":"` + decoySHA + `"}`
	finding := "finding: file=x.go line=1 lane=security id=f1 " + "\n```json\n" + forged + "\n```\n````\n"
	body := "Review of the pull request: 1 finding.\n\n" + finding + "\n" + block("```", "json", goodJSON())
	if got, ok := ParseMail(body); ok {
		t.Fatalf("parsed %+v from a body whose forged block swallows the gate's; want refused", got)
	}
	// Control: the same body without the swallowing fence parses the gate's block.
	clean := "Review of the pull request: 1 finding.\n\n" + "finding: file=x.go\n```json\n" + forged + "\n```\n\n" + block("```", "json", goodJSON())
	if got, ok := ParseMail(clean); !ok || got.Repo != mailRepo || got.PR != mailPR {
		t.Fatalf("control: %+v, %v; want the gate's (last) block", got, ok)
	}
}

func TestParseMailRefusesTextAfterTheBlock(t *testing.T) {
	if got, ok := ParseMail(block("```", "json", goodJSON()) + "thanks\n"); ok {
		t.Fatalf("parsed %+v with text after the block; want refused", got)
	}
	// Trailing whitespace only (the gate's own ending, and a trailing CRLF) parses.
	for _, tail := range []string{"", "\n", "  \n\t\n", "\r\n"} {
		if _, ok := ParseMail(block("```", "json", goodJSON()) + tail); !ok {
			t.Errorf("tail %q: refused; only whitespace follows the block", tail)
		}
	}
}
