package prroute

import "testing"

// Signal.Valid is the one shape check a hook's report passes before it is
// stored (internal/api's POST /api/pr-sessions, internal/plugins' store).

func TestSignalValid(t *testing.T) {
	good := []Signal{
		{Kind: KindPRCreate, Repo: "acme-labs/widget-api", PR: 4817},
		{Kind: KindPRCreate, Repo: "acme-labs/widget-api", PR: 4817, HeadRef: "feat/gizmo-route"},
		{Kind: KindPush, Repo: "acme-labs/widget-api", HeadRef: "feat/gizmo-route"},
	}
	for _, s := range good {
		if !s.Valid() {
			t.Errorf("%+v refused", s)
		}
	}
	bad := map[string]Signal{
		"create, no pr":       {Kind: KindPRCreate, Repo: "acme-labs/widget-api"},
		"create, negative pr": {Kind: KindPRCreate, Repo: "acme-labs/widget-api", PR: -2},
		"push, no branch":     {Kind: KindPush, Repo: "acme-labs/widget-api"},
		"push with a pr":      {Kind: KindPush, Repo: "acme-labs/widget-api", PR: 4817, HeadRef: "feat/x"},
		"unknown kind":        {Kind: "git-fetch", Repo: "acme-labs/widget-api", PR: 4817},
		"repo with no owner":  {Kind: KindPRCreate, Repo: "widget-api", PR: 4817},
		"repo with a space":   {Kind: KindPRCreate, Repo: "acme labs/widget-api", PR: 4817},
		"branch with ..":      {Kind: KindPush, Repo: "acme-labs/widget-api", HeadRef: "feat/../main"},
		"branch leading dash": {Kind: KindPush, Repo: "acme-labs/widget-api", HeadRef: "-f"},
		"branch trailing /":   {Kind: KindPush, Repo: "acme-labs/widget-api", HeadRef: "feat/"},
		"branch with a space": {Kind: KindPRCreate, Repo: "acme-labs/widget-api", PR: 4817, HeadRef: "feat x"},
	}
	for name, s := range bad {
		if s.Valid() {
			t.Errorf("%s: %+v accepted", name, s)
		}
	}
}
