package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPrivilegeRequestsRender(t *testing.T) {
	var b bytes.Buffer
	err := RenderPrivilegeRequests(&b, []PrivilegeRequestView{
		{ID: 1, AgentName: "swift-claw", Profile: "k8s-read", Reason: "need pod logs", CreatedAt: time.Unix(1_700_000_000, 0)},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	for _, want := range []string{"Privilege requests", "swift-claw", "k8s-read", "need pod logs"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestPrivilegeRequestsEmptyRendersNothing(t *testing.T) {
	var b bytes.Buffer
	if err := RenderPrivilegeRequests(&b, nil); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.TrimSpace(b.String()) != "" {
		t.Errorf("empty list should render nothing, got %q", b.String())
	}
}

func TestPrivilegeRequestHasApproveDeny(t *testing.T) {
	var b bytes.Buffer
	_ = RenderPrivilegeRequests(&b, []PrivilegeRequestView{{ID: 9, AgentName: "claw", Profile: "k8s-read"}})
	out := b.String()
	for _, w := range []string{
		`hx-post="/ui/privilege-requests/9/approve"`,
		`hx-post="/ui/privilege-requests/9/deny"`,
		"Approve", "Deny",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("request card missing %q\n%s", w, out)
		}
	}
}

func TestProfilesRender(t *testing.T) {
	var b bytes.Buffer
	if err := RenderProfiles(&b, []ProfileView{{ID: 2, Name: "k8s-read", Description: "read", Summary: "cluster RBAC"}}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	for _, w := range []string{
		"Privilege profiles", "k8s-read", "cluster RBAC",
		`hx-delete="/privileges/2"`, // the registry list + delete remain
		"Create via the Operator",   // creation moved to the operator (chat)
	} {
		if !strings.Contains(out, w) {
			t.Errorf("profiles partial missing %q\n%s", w, out)
		}
	}
	// The manual authoring form is gone (Task 7) — no create form / spec field.
	for _, gone := range []string{"+ New profile", `hx-post="/privileges"`, `name="spec"`} {
		if strings.Contains(out, gone) {
			t.Errorf("create form should be removed, but found %q\n%s", gone, out)
		}
	}
}

func TestAgentGrantsRender(t *testing.T) {
	var b bytes.Buffer
	err := RenderAgentGrants(&b, 5,
		[]GrantView{{ProfileID: 2, ProfileName: "k8s-read"}},
		[]ProfileOption{{ID: 3, Name: "k8s-admin"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	for _, w := range []string{
		"k8s-read",                       // granted chip
		`hx-delete="/agents/5/grants/2"`, // revoke
		`hx-post="/agents/5/grants"`,     // grant form
		"k8s-admin",                      // grantable option
	} {
		if !strings.Contains(out, w) {
			t.Errorf("agent grants partial missing %q\n%s", w, out)
		}
	}
}
