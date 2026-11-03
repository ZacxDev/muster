package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/hub"
	"github.com/ZacxDev/muster/internal/clankmailplugin"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// The owner's ceremony through the CLI against a REAL hub: keygen twice,
// identity claim, identity grant — and then the hub lists exactly the chain a
// peer's plugin verifies (VerifyChain), as @owner/chief.
func TestIdentityCeremonyMakesAChainAPeerVerifies(t *testing.T) {
	store, err := hub.OpenSQLite(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h, err := hub.New(hub.Config{Name: "hub.example", Store: store})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	dir := t.TempDir()
	pk, ck := filepath.Join(dir, "principal.key"), filepath.Join(dir, "chief.key")
	e := env(map[string]string{"HUB_URL": srv.URL, "HUB_NAME": "hub.example", "CHIEF_KEY_FILE": ck})
	var out, errb bytes.Buffer
	for _, args := range [][]string{
		{"keygen", "--out", pk}, {"keygen", "--out", ck},
		{"identity", "claim", "--key", pk, "--name", "acme"},
		{"identity", "grant", "--key", pk},
	} {
		if rc := run(args, e, &out, &errb); rc != 0 {
			t.Fatalf("%v: rc %d: %s", args, rc, errb.String())
		}
	}
	if !strings.Contains(out.String(), `granted "chief"`) {
		t.Fatalf("output: %s", out.String())
	}
	chief, err := clankmailplugin.ReadSeedFile(ck)
	if err != nil {
		t.Fatal(err)
	}
	principal, _ := clankmailplugin.ReadSeedFile(pk)
	hc := &clankmailplugin.HubClient{Base: srv.URL, Name: "hub.example", Key: chief, HTTP: srv.Client(), Now: time.Now, UA: "test"}
	handle, agents, found, err := hc.Handle(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	pc := clankmailplugin.PeerConf{Label: "acme/chief", Owner: "acme", Name: "chief", Key: clankmailplugin.KeyIDOf(chief)}
	res, err := clankmailplugin.VerifyChain(pc, "hub.example", handle, agents, found, "", time.Now().UnixMilli())
	if err != nil || res.Principal != clankmailplugin.KeyIDOf(principal) {
		t.Fatalf("the ceremony's chain does not verify: %v %v", res, err)
	}
	// keygen never overwrites.
	if rc := run([]string{"keygen", "--out", ck}, e, &out, &errb); rc == 0 {
		t.Fatal("keygen overwrote a key file")
	}
}

func TestServeRefusesABadConfiguration(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	os.WriteFile(tok, []byte("mpl_x\n"), 0o600)
	key := filepath.Join(dir, "chief.key")
	if _, err := clankmailplugin.WriteSeedFile(key); err != nil {
		t.Fatal(err)
	}
	good := map[string]string{"MUSTER_URL": "http://muster.example:8105", "HUB_URL": "https://hub.example",
		"HUB_NAME": "hub.example", "PLUGIN_TOKEN_FILE": tok, "CHIEF_KEY_FILE": key, "STATE_DIR": dir}
	if _, err := clankmailplugin.LoadConfig(env(good)); err != nil {
		t.Fatalf("a good configuration refused: %v", err)
	}
	for k, v := range map[string]string{
		"HUB_NAME": "Hub.Example", "POLL_INTERVAL": "10m", "CLANKMAIL_PEERS": "alice/chief=k_nope",
		"MUSTER_URL": "muster", "STATE_DIR": filepath.Join(dir, "missing"), "CHIEF_KEY_FILE": tok,
	} {
		bad := map[string]string{}
		for a, b := range good {
			bad[a] = b
		}
		bad[k] = v
		var errb bytes.Buffer
		if rc := run(nil, env(bad), &bytes.Buffer{}, &errb); rc != 2 {
			t.Errorf("%s=%q: rc %d, want 2 (refused at boot): %s", k, v, rc, errb.String())
		}
	}
}
