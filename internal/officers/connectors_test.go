package officers_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// An officer's `connectors`: what its instances may request besides github.

func TestAnOfficersDeclaredConnectorsAreTheOnlyExtraOnesItMayRequest(t *testing.T) {
	raw := `[{"id":"release:example-web","hq_url":"https://hq.example.com/api/v1","repo":"example-org/web",
	          "connectors":["prod-db-ro", "prod-db-ro", "analytics-ro"]}]`
	list, err := officers.Parse(raw, env(map[string]string{"MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB": "tok"}), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if strings.Join(o.Connectors, ",") != "analytics-ro,prod-db-ro" {
		t.Fatalf("connectors = %v, want sorted and deduplicated", o.Connectors)
	}
	for name, want := range map[string]bool{"github": true, "prod-db-ro": true, "analytics-ro": true, "other-db": false, "": false} {
		if got := officers.MayRequestConnector(o, name); got != want {
			t.Errorf("MayRequestConnector(%q) = %v, want %v", name, got, want)
		}
	}
	// An officer that declares none may request github alone (the rule before).
	none := officers.Officer{ID: "triage:x"}
	if !officers.MayRequestConnector(none, "github") || officers.MayRequestConnector(none, "prod-db-ro") {
		t.Error("an officer without connectors: want github only")
	}
	for _, bad := range []string{`["Prod_DB"]`, `["github"]`, `[""]`} {
		raw := `[{"id":"a:b","hq_url":"https://hq.example.com/api/v1","connectors":` + bad + `}]`
		if _, err := officers.Parse(raw, env(map[string]string{"MUSTER_OFFICER_TOKEN_A_B": "tok"}), "muster-agent-"); err == nil ||
			!strings.Contains(err.Error(), "connector") {
			t.Errorf("connectors %s: %v, want a boot refusal naming the connector", bad, err)
		}
	}
}

func TestTheKickoffTeachesAnOfficersDeclaredConnectors(t *testing.T) {
	o := officers.Officer{ID: "release:example-web", MaxInstances: 1, Repo: "example-org/web", Connectors: []string{"prod-db-ro"}}
	text := officers.KickoffText(o, 1)
	for _, want := range []string{"You may also use these connectors: prod-db-ro.", "postgres_query{connector, sql, params?}",
		"READ-ONLY transaction", "pg_catalog", "the only connector you may use for it"} {
		if !strings.Contains(text, want) {
			t.Errorf("kickoff lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "the only connector you may use.") {
		t.Error("the kickoff still says github is the only connector")
	}
	// Without connectors the text has no such paragraph.
	o.Connectors = nil
	if text := officers.KickoffText(o, 1); strings.Contains(text, "postgres_query") || strings.Contains(text, "for it.") {
		t.Errorf("an officer without connectors is told about them:\n%s", text)
	}
}
