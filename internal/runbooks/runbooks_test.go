package runbooks

import (
	"strings"
	"testing"
)

func TestValidateRequiredParams(t *testing.T) {
	spec := Spec{Params: []ParamDef{
		{Name: "repo", Required: true},
		{Name: "version", Required: true, Default: "latest"},
		{Name: "note"},
	}}

	if err := spec.Validate(map[string]string{"repo": "owner/x"}); err != nil {
		t.Fatalf("expected valid (version defaulted, note optional): %v", err)
	}
	err := spec.Validate(map[string]string{"version": "1.2"})
	if err == nil || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("expected missing-repo error, got %v", err)
	}
}

func TestRenderSubstitutesParamsAndDefaults(t *testing.T) {
	rb := Runbook{Spec: Spec{
		Params: []ParamDef{
			{Name: "repo", Required: true},
			{Name: "version", Default: "latest"},
		},
		BodyTemplate: "Bump {{repo}} to {{ version }}. Repo again: {{repo}}.",
	}}

	out, err := rb.Render(map[string]string{"repo": "owner/app"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "Bump owner/app to latest. Repo again: owner/app."
	if out != want {
		t.Fatalf("render mismatch:\n got: %q\nwant: %q", out, want)
	}
}

func TestRenderMissingRequiredErrors(t *testing.T) {
	rb := Runbook{Spec: Spec{
		Params:       []ParamDef{{Name: "repo", Required: true}},
		BodyTemplate: "do {{repo}}",
	}}
	if _, err := rb.Render(nil); err == nil {
		t.Fatal("expected error for missing required param")
	}
}

func TestRenderUndeclaredPlaceholderLeftLiteral(t *testing.T) {
	rb := Runbook{Spec: Spec{BodyTemplate: "literal {{unknown}} stays"}}
	out, err := rb.Render(nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "{{unknown}}") {
		t.Fatalf("expected undeclared placeholder left literal, got %q", out)
	}
}

func TestRenderAppendsStepsAndSuccessCriteria(t *testing.T) {
	rb := Runbook{Spec: Spec{
		BodyTemplate: "Kick off the upgrade.",
		Steps: []Step{
			{Title: "Open a branch"},
			{Title: "Run migrations", Instructions: "use the gated script", RequiresApproval: true},
		},
		SuccessCriteria: "CI is green and the PR is opened.",
	}}
	out, err := rb.Render(nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"## Steps",
		"1. Open a branch",
		"2. Run migrations",
		"checkpoint: call agent_checkpoint",
		"use the gated script",
		"## Done when",
		"CI is green",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered body missing %q:\n%s", want, out)
		}
	}
}
