package previewhost

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestABadPreviewsFileDisablesWithItsReason: every way previews.json can be
// wrong is an error that NAMES what is wrong — never a skipped entry, never a
// default in place of a value written wrong. Each case's reason is pinned, so
// a guard that stops firing fails here with the case named.
func TestABadPreviewsFileDisablesWithItsReason(t *testing.T) {
	for _, c := range []struct{ name, file, why string }{
		{"empty", "", "the file is empty"},
		{"whitespace", " \n ", "the file is empty"},
		{"not json", "ports: 3000", "not a previews.json object"},
		{"no ports key", `{}`, `"ports" lists no port`},
		{"empty ports", `{"ports": []}`, `"ports" lists no port`},
		{"privileged port", `{"ports": ["80"]}`, "port 80 is outside 1024-65535"},
		{"just under 1024", `{"ports": ["1023"]}`, "port 1023 is outside 1024-65535"},
		{"range dipping under 1024", `{"ports": ["1000-2000"]}`, "port 1000 is outside 1024-65535"},
		{"above 65535", `{"ports": ["65536"]}`, "port 65536 is outside 1024-65535"},
		{"zero", `{"ports": ["0"]}`, "port 0 is outside"},
		{"backwards range", `{"ports": ["8099-8000"]}`, "the range ends (8000) before it starts (8099)"},
		{"open range", `{"ports": ["3000-"]}`, `"" is not a port number`},
		{"letters", `{"ports": ["http"]}`, `"http" is not a port number`},
		{"space inside", `{"ports": ["30 00"]}`, `"30 00" is not a port number`},
		{"signed", `{"ports": ["+3000"]}`, `"+3000" is not a port number`},
		{"one bad among good", `{"ports": ["3000", "x", "5173"]}`, `port entry "x"`},
		{"ports as numbers", `{"ports": [3000]}`, "not a previews.json object"},
		{"misspelt cap", `{"ports": ["3000"], "maxconns": 8}`, `unknown field "maxconns"`},
		{"unknown key", `{"ports": ["3000"], "share": true}`, `unknown field "share"`},
		{"duplicate key in other case", `{"ports": ["3000"], "Ports": ["22"]}`, `unknown field "Ports"`},
		{"maxOpen zero", `{"ports": ["3000"], "maxOpen": 0}`, `"maxOpen" 0 is outside 1-32`},
		{"maxOpen over", `{"ports": ["3000"], "maxOpen": 33}`, `"maxOpen" 33 is outside 1-32`},
		{"maxConns zero", `{"ports": ["3000"], "maxConns": 0}`, `"maxConns" 0 is outside 1-64`},
		{"maxConns over the gateway's cap", `{"ports": ["3000"], "maxConns": 65}`, `"maxConns" 65 is outside 1-64`},
		{"two values", `{"ports": ["3000"]} {}`, "more than one JSON value"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse([]byte(c.file))
			if err == nil {
				t.Fatalf("accepted: %+v", got)
			}
			if !strings.Contains(err.Error(), c.why) {
				t.Fatalf("reason %q, want it to say %q", err, c.why)
			}
		})
	}
}

// TestAGoodPreviewsFileLoadsWithItsDefaults: defaults for what is left out,
// the values written for what is not, and the boundaries themselves accepted.
func TestAGoodPreviewsFileLoadsWithItsDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"ports": ["5173", "1024", "8000-8099", "65535"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Offer || c.MaxOpen != DefaultMaxOpen || c.MaxConns != DefaultMaxConns || c.Ports.String() != "1024,5173,8000-8099,65535" {
		t.Fatalf("defaults: %+v (ports %s)", c, c.Ports)
	}
	c, err = Parse([]byte(`{"ports": ["3000"], "offer": false, "maxOpen": 3, "maxConns": 7}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Offer || c.MaxOpen != 3 || c.MaxConns != 7 {
		t.Fatalf("written values: %+v", c)
	}
	if _, err := Parse([]byte(`{"ports": ["3000"], "maxOpen": 32, "maxConns": 64}`)); err != nil {
		t.Fatalf("the caps themselves were refused: %v", err)
	}
}

// TestLoadSaysWhyAFileCannotBeUsed: a missing file, a directory and an
// oversized file each disable previews with a reason naming the path.
func TestLoadSaysWhyAFileCannotBeUsed(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "previews.json")
	if _, err := Load(missing); err == nil || !strings.Contains(err.Error(), missing+" does not exist") {
		t.Fatalf("missing: %v", err)
	}
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "no previews file") {
		t.Fatalf("no path: %v", err)
	}
	big := filepath.Join(dir, "big.json")
	must(t, os.WriteFile(big, []byte(`{"ports": ["3000"]}`+strings.Repeat(" ", maxFileBytes)), 0o600))
	if _, err := Load(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	must(t, os.WriteFile(bad, []byte(`{"ports": ["80"]}`), 0o600))
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), bad+": port entry") {
		t.Fatalf("invalid: %v", err)
	}
	good := filepath.Join(dir, "good.json")
	must(t, os.WriteFile(good, []byte(`{"ports": ["3000"]}`), 0o600))
	if c, err := Load(good); err != nil || !c.Ports.Contains(3000) {
		t.Fatalf("good: %+v, %v", c, err)
	}
}

// TestTheMCPAndOpencodePortsAreRefusedEvenWhenListed: the always-refused ports
// are refused with port_not_allowed and say what they are, though the
// allowlist holds them; a listed port is allowed, an unlisted one refused.
func TestTheMCPAndOpencodePortsAreRefusedEvenWhenListed(t *testing.T) {
	c, err := Parse([]byte(`{"ports": ["4096", "40000-50000", "5173"]}`))
	if err != nil {
		t.Fatal(err)
	}
	refused := AlwaysRefused(func() map[int]string {
		return map[int]string{47823: "the MCP endpoint", 4096: "the opencode server"}
	})
	check := func(port int, wantCode, wantWhy string) {
		t.Helper()
		err := c.Allows(port, refused)
		var r *Refusal
		switch {
		case wantCode == "" && err != nil:
			t.Errorf("port %d refused: %v", port, err)
		case wantCode != "" && (!errors.As(err, &r) || r.Code != wantCode || !strings.Contains(r.Why, wantWhy)):
			t.Errorf("port %d: %v, want %s saying %q", port, err, wantCode, wantWhy)
		}
	}
	check(47823, CodePortNotAllowed, "the MCP endpoint, which is never exposed")
	check(4096, CodePortNotAllowed, "the opencode server, which is never exposed")
	check(5173, "", "")
	check(45000, "", "")
	check(3000, CodePortNotAllowed, "not on the allowlist")
	// nil refused: the allowlist alone.
	if err := c.Allows(4096, nil); err != nil {
		t.Errorf("4096 with no refused set: %v", err)
	}
}

// TestPortsOfTheRefusedEndpoints: how muster-mirror finds the ports to refuse
// from -mcp-listen and the opencode token's url.
func TestPortsOfTheRefusedEndpoints(t *testing.T) {
	for addr, want := range map[string]int{"127.0.0.1:47823": 47823, "[::1]:4000": 4000, "": 0, "127.0.0.1": 0, "x:0": 0} {
		if got, ok := PortOfAddr(addr); got != want || ok != (want != 0) {
			t.Errorf("PortOfAddr(%q) = %d, %v; want %d", addr, got, ok, want)
		}
	}
	for u, want := range map[string]int{"http://127.0.0.1:4096": 4096, "http://127.0.0.1": 80, "https://[::1]": 443, "http://127.0.0.1:4096/x": 4096, "": 0, "ftp://h": 0} {
		if got, ok := PortOfURL(u); got != want || ok != (want != 0) {
			t.Errorf("PortOfURL(%q) = %d, %v; want %d", u, got, ok, want)
		}
	}
}
