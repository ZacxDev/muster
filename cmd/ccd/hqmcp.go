package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// The HQ MCP server in the CLI's user-scope mcpServers (internal/podagent
// hqmcp.go says what it is and why no HQ credential reaches the pod).
//
// `ccd seed` registers it when ALL of these hold, and otherwise removes the
// entry it wrote before:
//
//   - the pod was told to offer it (podagent.EnvHQMCP=1: the chief only);
//   - the pod is offered muster's connector store (MUSTER_CONNECT_URL and the
//     agent's own token), the only way the server can reach HQ;
//   - `hq` is on PATH (an image without the HQ CLI gets no server that can only
//     fail to start);
//   - muster's GET /agent/connectors/hq says the connector exists and this
//     agent's grant on it is not denied or revoked (podagent.HQServes).
//
// When muster cannot be read (the GET fails or times out) the entry is LEFT AS
// IT WAS: with muster unreachable the proxy is too, and the next pod start
// decides again. The registration is a convenience, not the gate — the proxy
// checks the grant on every call.
//
// ⚠ IT IS DECIDED AT POD START (the entrypoint's `ccd seed`). A connector
// created, or a grant revoked, while the CLI runs takes effect on the next pod
// start (the chief's restart); the CLI reads mcpServers only when it starts.

// hqStatusTimeout bounds seed's one read of the connector: the entrypoint runs
// under `set -e`, and a pod start must never hang on muster.
const hqStatusTimeout = 5 * time.Second

// hqConnectorStatus is this agent's view of the `hq` connector. A var so tests
// answer it without a muster.
var hqConnectorStatus = func(ctx context.Context, base, token string) (agentConnector, error) {
	var c agentConnector
	err := musterGET(ctx, base, token, "/agent/connectors/"+url.PathEscape(podagent.HQConnector), &c)
	return c, err
}

// hqLookPath finds the hq CLI. A var so tests need no binary.
var hqLookPath = exec.LookPath

// hqMCPEntry is the mcpServers entry seed writes.
//
// HQ_TOKEN is the agent's OWN muster token — the value the muster entry beside
// it already carries as X-Muster-Token, in the same 0600 file — never an HQ
// credential.
func hqMCPEntry(connectURL, host, token string) map[string]any {
	args := make([]any, len(podagent.HQArgs))
	for i, a := range podagent.HQArgs {
		args[i] = a
	}
	return map[string]any{
		"type":    "stdio",
		"command": podagent.HQCommand,
		"args":    args,
		"env": map[string]any{
			"HQ_URL":   podagent.HQProxyURL(connectURL, host),
			"HQ_TOKEN": token,
		},
	}
}

// hqProxyPrefix is the part of HQ_URL every entry seed writes starts with.
func hqProxyPrefix(connectURL string) string {
	return podagent.HQProxyURL(connectURL, "")
}

// ownHQEntry reports whether v is an entry seed wrote: `hq` whose HQ_URL goes
// through a muster proxy's `hq` connector. Anything else under the name — an
// operator's own server — is never touched.
func ownHQEntry(v any) bool {
	m, _ := v.(map[string]any)
	env, _ := m["env"].(map[string]any)
	u, _ := env["HQ_URL"].(string)
	return m["command"] == podagent.HQCommand && strings.Contains(u, "/connect/"+podagent.HQConnector+"/")
}

// seedHQMCP adds or removes the HQ entry in st (.claude.json) by the rules
// above. on is podagent.EnvHQMCP as parsed.
func seedHQMCP(st map[string]any, on bool, connectURL, token string, stderr io.Writer) {
	connectURL = strings.TrimRight(connectURL, "/")
	servers, _ := st["mcpServers"].(map[string]any)
	remove := func(why string) {
		if servers == nil || !ownHQEntry(servers[podagent.HQMCPServerName]) {
			return
		}
		delete(servers, podagent.HQMCPServerName)
		if len(servers) == 0 {
			delete(st, "mcpServers")
		}
		fmt.Fprintf(stderr, "ccd seed: removed the HQ MCP server: %s\n", why)
	}
	if !on {
		remove("this agent is not offered it")
		return
	}
	if connectURL == "" || token == "" {
		remove("this pod is not offered muster's connector store")
		return
	}
	if _, err := hqLookPath(podagent.HQCommand); err != nil {
		fmt.Fprintf(stderr, "ccd seed: no HQ MCP server: %q is not on PATH in this image\n", podagent.HQCommand)
		remove("the hq CLI is not in this image")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hqStatusTimeout)
	defer cancel()
	c, err := hqConnectorStatus(ctx, connectURL, token)
	if err != nil {
		fmt.Fprintf(stderr, "ccd seed: could not read the %q connector (%v); the HQ MCP server is left as it was\n",
			podagent.HQConnector, err)
		return
	}
	host, ok := podagent.HQServes(c)
	if !ok {
		fmt.Fprintf(stderr, "ccd seed: no HQ MCP server: connector %q exists=%v, grant %q\n", podagent.HQConnector, c.Exists, c.Grant)
		remove("connector " + podagent.HQConnector + " does not exist, or this agent's grant was denied or revoked")
		return
	}
	if servers == nil {
		servers = map[string]any{}
	}
	servers[podagent.HQMCPServerName] = hqMCPEntry(connectURL, host, token)
	st["mcpServers"] = servers
}

// checkHQMCP is checkSeed's half for the HQ entry: an entry seed wrote is
// there only when the agent is offered it AND the pod the connector store, and
// then has exactly seed's shape for this pod's URL and token. Whether the
// grant allowed it cannot be checked offline (the image smoke test has no
// muster), so a missing entry is never an error.
func checkHQMCP(st map[string]any, on bool, connectURL, token string) error {
	connectURL = strings.TrimRight(connectURL, "/")
	servers, _ := st["mcpServers"].(map[string]any)
	v, present := servers[podagent.HQMCPServerName]
	if !present || !ownHQEntry(v) {
		return nil
	}
	if !on || connectURL == "" || token == "" {
		return fmt.Errorf(".claude.json: the HQ MCP server is registered but this pod is not offered it "+
			"(%s=%v, connector store offered=%v)", podagent.EnvHQMCP, on, connectURL != "" && token != "")
	}
	env, _ := v.(map[string]any)["env"].(map[string]any)
	u, _ := env["HQ_URL"].(string)
	host, found := strings.CutPrefix(u, hqProxyPrefix(connectURL))
	if !found || host == "" || strings.Contains(host, "/") {
		return fmt.Errorf(".claude.json: the HQ MCP server's HQ_URL %q is not this pod's proxy prefix %s<host>", u, hqProxyPrefix(connectURL))
	}
	a, _ := json.Marshal(v)
	b, _ := json.Marshal(hqMCPEntry(connectURL, host, token))
	if string(a) != string(b) {
		return fmt.Errorf(".claude.json: the HQ MCP server entry differs from the one seed writes")
	}
	return nil
}
