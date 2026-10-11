package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// muster's connector store, from inside the pod.
//
// Two pieces, both driven by the pod's environment and both inert without it:
//
//   - `ccd seed` registers muster's MCP server (MUSTER_CONNECT_URL + "/mcp") in
//     the CLI's user config, so the session has the connectors_list /
//     connector_request / connector_status tools. See seedMCP.
//   - `ccd connect-git [connector]` routes git through muster's proxy for a
//     connector THIS agent holds an approved grant on: for each of the
//     connector's hosts, `url.<proxy>.insteadOf https://<host>/`, plus the
//     agent's own token as an extra header on proxy URLs only. Without an
//     approved grant it REMOVES that routing, so git talks to the platform
//     directly (public clones keep working) rather than through a proxy that
//     would refuse it. The entrypoint runs it once at start; the agent runs it
//     again after an approval.
//
// 🔴 NO PLATFORM CREDENTIAL EVER REACHES THIS POD. The only secret written here
// is the agent's OWN muster token, which the pod already holds in its
// environment (MUSTER_HOOK_TOKEN): muster adds the platform credential on the
// way out.

const (
	envConnectURL = "MUSTER_CONNECT_URL"
	envAgentToken = "MUSTER_HOOK_TOKEN"
	// mcpServerName is the key muster's server is registered under, so its tools
	// appear to the session as mcp__muster__<tool>.
	mcpServerName = "muster"
)

// seedMCP adds (or, when the pod is not offered the connector store, removes)
// muster's entry in the CLI's user-scope mcpServers. Every other server an
// operator registered is kept.
func seedMCP(st map[string]any, connectURL, token string) {
	servers, _ := st["mcpServers"].(map[string]any)
	if connectURL == "" || token == "" {
		if servers != nil {
			delete(servers, mcpServerName)
			if len(servers) == 0 {
				delete(st, "mcpServers")
			}
		}
		return
	}
	if servers == nil {
		servers = map[string]any{}
	}
	servers[mcpServerName] = map[string]any{
		"type":    "http",
		"url":     strings.TrimRight(connectURL, "/") + "/mcp",
		"headers": map[string]any{"X-Muster-Token": token},
	}
	st["mcpServers"] = servers
}

// agentConnector is the subset of muster's GET /agent/connectors/{name} answer
// this command reads.
type agentConnector struct {
	Name   string   `json:"name"`
	Exists bool     `json:"exists"`
	Hosts  []string `json:"hosts"`
	Grant  string   `json:"grant"`
}

// exitNotApproved is connect-git's exit when the agent holds no approved grant:
// distinct from a failure, so a script can tell "ask the operator" from "broken".
const exitNotApproved = 3

func runConnectGit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("connect-git", flag.ContinueOnError)
	fs.SetOutput(stderr)
	request := fs.Bool("request", false, "if not approved, ask the operator for access (files a request)")
	reason := fs.String("reason", "", "with --request: why access is needed")
	timeout := fs.Duration("timeout", 10*time.Second, "how long to wait for muster")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := "github"
	if fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	base := strings.TrimRight(os.Getenv(envConnectURL), "/")
	token := os.Getenv(envAgentToken)
	if base == "" || token == "" {
		fmt.Fprintf(stderr, "ccd connect-git: %s and %s must be set (this pod is not offered muster's connector store)\n",
			envConnectURL, envAgentToken)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := fetchConnector(ctx, base, token, name, *request, *reason)
	if err != nil {
		fmt.Fprintln(stderr, "ccd connect-git:", err)
		return 1
	}
	proxy := base + "/connect/" + name + "/"
	if !c.Exists || c.Grant != "approved" {
		if err := unsetGitRouting(proxy); err != nil {
			fmt.Fprintln(stderr, "ccd connect-git:", err)
			return 1
		}
		fmt.Fprintf(stdout, "ccd connect-git: %s is %s for this agent; git talks to the platform directly. "+
			"Ask with `ccd connect-git --request %s` (or the connector_request tool), then run this again.\n",
			name, c.Grant, name)
		return exitNotApproved
	}
	if err := setGitRouting(proxy, c.Hosts, token); err != nil {
		fmt.Fprintln(stderr, "ccd connect-git:", err)
		return 1
	}
	fmt.Fprintf(stdout, "ccd connect-git: git now reaches %s through muster's proxy (connector %s)\n",
		strings.Join(c.Hosts, ", "), name)
	return 0
}

func fetchConnector(ctx context.Context, base, token, name string, request bool, reason string) (agentConnector, error) {
	var c agentConnector
	u := base + "/agent/connectors/" + url.PathEscape(name)
	method, body := http.MethodGet, io.Reader(nil)
	if request {
		b, _ := json.Marshal(map[string]string{"reason": reason})
		method, u, body = http.MethodPost, u+"/request", strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return c, err
	}
	req.Header.Set("X-Muster-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return c, fmt.Errorf("reaching muster: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return c, fmt.Errorf("muster answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("muster's answer is not JSON: %w", err)
	}
	if c.Grant == "" {
		return c, errors.New("muster's answer carries no grant status")
	}
	return c, nil
}

func git(args ...string) error {
	out, err := exec.Command("git", args...).CombinedOutput()
	// Exit 5 from `--unset-all` means "nothing to unset", which is success here.
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 5 && slices.Contains(args, "--unset-all") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setGitRouting points every https://<host>/ of the connector at the proxy, and
// sends the agent's token on proxy URLs (and ONLY on them: an http.<url> key is
// scoped to URLs under that prefix).
func setGitRouting(proxy string, hosts []string, token string) error {
	if err := unsetGitRouting(proxy); err != nil {
		return err
	}
	for _, h := range hosts {
		if err := git("config", "--global", "--add", "url."+proxy+h+"/.insteadOf", "https://"+h+"/"); err != nil {
			return err
		}
	}
	return git("config", "--global", "http."+proxy+".extraHeader", "X-Muster-Token: "+token)
}

// unsetGitRouting removes every key setGitRouting writes for this proxy prefix.
func unsetGitRouting(proxy string) error {
	out, err := exec.Command("git", "config", "--global", "--name-only", "--get-regexp", `^url\..*\.insteadof$`).Output()
	if err == nil {
		for _, key := range strings.Fields(string(out)) {
			// Keys come back lowercased in their section/variable parts but the
			// subsection (the URL) is verbatim.
			sub := strings.TrimSuffix(strings.TrimPrefix(key, "url."), ".insteadof")
			if strings.HasPrefix(sub, proxy) {
				if err := git("config", "--global", "--unset-all", "url."+sub+".insteadOf"); err != nil {
					return err
				}
			}
		}
	}
	return git("config", "--global", "--unset-all", "http."+proxy+".extraHeader")
}
