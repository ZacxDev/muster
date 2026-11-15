package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
)

// emptyStore is a connector store with nothing in it: GetConnector answers
// not-found, as a store with no stored connector of that name does.
type emptyStore struct{ connectors.Store }

func (emptyStore) GetConnector(context.Context, string) (connectors.Connector, error) {
	return connectors.Connector{}, connectors.ErrNotFound
}

// TestInternalHTTPConnectorsAreWiredForAdminsOnly: MUSTER_INTERNAL_HTTP_CONNECTORS
// and its token, read by loadConfig, become a connector an ADMIN's principal
// finds and a member's does not; the boot line names the service and never
// the token.
func TestInternalHTTPConnectorsAreWiredForAdminsOnly(t *testing.T) {
	env := map[string]string{envDatabase: "postgres://unused", envStandalone: "1", envConnectorEncKey: connectorKey(1),
		envConnectPort: "18191", envAgentConnectURL: "http://muster.example.test:18191/",
		envInternalHTTPConnectors:               `[{"name":"app-driver","base_url":"http://app-driver.example.test:8080"}]`,
		"MUSTER_INTERNAL_HTTP_TOKEN_APP_DRIVER": "fixture-wiring-token-99"}
	cfg, err := loadConfig(func(n string) string { return env[n] })
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("a coherent internal_http configuration is refused: %v", err)
	}
	if len(cfg.InternalHTTP) != 1 {
		t.Fatalf("%d internal_http connectors parsed, want 1", len(cfg.InternalHTTP))
	}
	var logs bytes.Buffer
	store := withInternalHTTP(emptyStore{}, cfg.InternalHTTP, log.New(&logs, "", 0))
	admin := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	if _, ok := connectors.InternalHTTPOf(admin, store, "app-driver"); !ok {
		t.Fatal("an admin's principal does not find app-driver")
	}
	if _, ok := connectors.InternalHTTPOf(member, store, "app-driver"); ok {
		t.Fatal("a member's principal finds app-driver")
	}
	if _, err := store.GetConnector(member, "app-driver"); err != connectors.ErrNotFound {
		t.Fatalf("a member's GetConnector(app-driver) = %v, want not found", err)
	}
	line := logs.String()
	if !strings.Contains(line, "http://app-driver.example.test:8080") || strings.Contains(line, "fixture-wiring-token-99") {
		t.Fatalf("boot line %q: want the service named and no token", line)
	}
}
