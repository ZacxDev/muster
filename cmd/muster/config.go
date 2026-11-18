package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"strings"
)

// defaultEnvFile is where this CLI looks for its credentials when nothing is
// exported and no flag is given.
//
// ⚠ IT IS NOT DERIVED FROM THE INVOKED PROGRAM NAME, even though the binary
// answers to more than one (see progName). An alias changes WHICH NAME the
// process is running under; it does not change where this machine keeps
// muster's credentials, and a config path that moved with argv[0] would send
// two invocations of the same binary to two different files for no reason a
// reader could predict. Point a differently-provisioned host at its own file
// with --env-file.
const defaultEnvFile = "~/.muster/muster.env"

const (
	// envAPIURL is the ONE base URL this binary speaks to.
	//
	// 🔴 ONE URL IS CORRECT HERE, AND IT IS A CONSEQUENCE OF THE TWO-BINARY
	// SPLIT RATHER THAN AN ASSUMPTION THAT SURVIVED IT. The upstream CLI this
	// was extracted from also had a single base URL, and that WAS a problem
	// there: its verbs were about to straddle two services. This binary's verbs
	// do not — every route in the wiring ledger is served by muster's own
	// mux — so one URL is sufficient rather than merely convenient.
	//
	// That is a claim about the route set, so it is CHECKED rather than
	// asserted: TestEveryWiredRouteIsServedByThisProject reads
	// internal/api/testdata/routes.golden and fails if any wired route is not in
	// it. A verb aimed at a second service would have to add a route the golden
	// does not carry, and that test is what turns "one URL is enough" into a
	// measurement.
	envAPIURL = "MUSTER_API_URL"
	// envToken is the ONE credential this binary presents (accounts PR 6).
	//
	// ⚠ WHAT IT HOLDS DEPENDS ON WHO RUNS THE CLIENT, and the cases are not
	// interchangeable:
	//   - a PERSON: their own personal API token (`mst_…`, minted at
	//     /account/tokens or with `token create`). It acts as that person on
	//     every machine-tier route, and it is the only credential `token …`
	//     accepts.
	//   - an operator HOST's hook scripts: the server's shared hook secret
	//     (api.AuthConfig.HookToken), acting as the machine user.
	//   - a managed AGENT POD: that pod's OWN per-agent token, which is what
	//     api.requireAgentToken resolves to an agent row. The `agent task`
	//     verbs work only there.
	envToken = "MUSTER_TOKEN"
	// envTokenAlias is the name this credential had before personal tokens,
	// and it is STILL READ: every hook host's env file and every agent pod
	// (internal/agentspec injects it) spells it this way. Within one source
	// envToken wins over it; a higher-precedence source still wins over both.
	envTokenAlias = "MUSTER_HOOK_TOKEN"
)

// config is the resolved connection config. The token is NEVER printed: it is
// not included in any error message, and it is only ever read from the env
// file, the process environment, or --token. It is never accepted as a
// positional argument, because argv is world-readable through /proc.
type config struct {
	apiURL string
	token  string
	// createTok is MUSTER_AGENT_CREATE_TOKEN, resolved with the URL+token pair
	// (resolveConfig, resolvePaired) from the env file or the environment,
	// and presented only by `agent create`.
	createTok string
	// createTokRefusal is why an exported MUSTER_AGENT_CREATE_TOKEN was NOT
	// accepted for this tenant (resolvePaired), or "". Only `agent create`
	// raises it: no other verb sends that token, so no other verb refuses.
	createTokRefusal string
}

// parseEnvFile reads a KEY=VALUE file (comments, blank lines, an optional
// `export ` prefix, optionally quoted values). A missing file is NOT an error —
// the environment or flags may supply everything.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()
	return parseEnv(f)
}

// parseEnv is parseEnvFile's parser, over an already-opened reader. A tenant
// file is opened by readTenantFile (O_NOFOLLOW, mode checked on the open
// descriptor) and parsed here, so the file checked is the file read.
func parseEnv(f io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	// Tokens can be long (a token plus a URL); 1 MiB is plenty and bounds the
	// read of a file that is not supposed to be huge.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = unquote(strings.TrimSpace(v))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unquote strips one layer of matching single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// expandHome resolves a leading ~/ against $HOME. It deliberately does not
// shell out and does not handle ~user.
func expandHome(path string, homeDir string) string {
	if path == "~" {
		return homeDir
	}
	if strings.HasPrefix(path, "~/") && homeDir != "" {
		return homeDir + path[1:]
	}
	return path
}

// resolveConfig applies the documented precedence, LOWEST to HIGHEST:
//
//	the env file  ->  process environment  ->  --api-url / --token
//
// A later source only overrides an earlier one when it actually supplies a
// value, so an empty environment variable does not blank out the file.
func resolveConfig(envFilePath string, fileVals map[string]string, getenv func(string) string, flagURL, flagToken string) (config, error) {
	var c config
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&c.apiURL, fileVals[envAPIURL])
	set(&c.token, fileVals[envTokenAlias])
	set(&c.token, fileVals[envToken])
	set(&c.apiURL, getenv(envAPIURL))
	set(&c.token, getenv(envTokenAlias))
	set(&c.token, getenv(envToken))
	set(&c.apiURL, flagURL)
	set(&c.token, flagToken)
	// The agent-create token (agent_create.go) has no flag: the file, then
	// the environment. On this one-tenant path the sources mix, as the
	// token's do.
	set(&c.createTok, fileVals[envAgentCreateToken])
	set(&c.createTok, getenv(envAgentCreateToken))

	if c.apiURL == "" {
		return config{}, failf(exitUsage,
			"no API URL: set %s in %s, export it, or pass --api-url (e.g. --api-url http://127.0.0.1:8080)",
			envAPIURL, envFilePath)
	}
	return checkAPIURL(c)
}

// pairOverride is one URL or token override of a tenant file, with the name an
// operator would recognise it by (a flag or a variable).
type pairOverride struct{ name, value string }

// fromFlag: this override came from a flag (--api-url, --token), not the
// environment.
func (o pairOverride) fromFlag() bool { return strings.HasPrefix(o.name, "--") }

// sourceKind names where an override came from, for a refusal.
func (o pairOverride) sourceKind() string {
	if o.fromFlag() {
		return "a flag"
	}
	return "the environment"
}

// firstOverride is the highest-precedence non-empty override, or the zero value.
func firstOverride(cands ...pairOverride) pairOverride {
	for _, c := range cands {
		if c.value != "" {
			return c
		}
	}
	return pairOverride{}
}

// pairRefusal is the one phrase every pair-rule refusal carries. Tests pin it,
// so a refusal for any OTHER reason (a missing file, an unknown flag) cannot
// pass for this one.
const pairRefusal = "refused: a URL and a token override are accepted only as a pair"

// resolvePaired is resolution OFF the zero-touch path (see tenant.go): the
// selected file is authoritative for the URL+token PAIR.
//
// 🔴 THIS IS WHAT STOPS ONE TENANT'S TOKEN REACHING ANOTHER TENANT'S ORIGIN.
// resolveConfig mixes sources field by field, so an exported MUSTER_API_URL
// for tenant A plus tenant B's file sends B's token to A. Here an override
// replaces the file only when BOTH halves are overridden FROM THE SAME KIND OF
// SOURCE: both flags (--api-url + --token) or both environment variables. One
// half alone is refused, exit 2, before any request — and so is a pair whose
// halves come from different kinds of source: a URL (or token) already
// exported in the shell for some other server plus a typed flag for the other
// half names no server's pair, and accepting it sends the typed token to the
// exported URL, or the exported token to the typed URL (round-1 audit, R1).
// MUSTER_HOOK_TOKEN is an override like MUSTER_TOKEN: it is the name agent
// pods and hook hosts carry, and on today's resolver it beats a file's
// MUSTER_TOKEN.
//
// 🔴 MUSTER_AGENT_CREATE_TOKEN (agent_create.go) IS A TOKEN TOO, AND THE SAME
// RULE DECIDES IT, HERE (round-1 audit of #532, R1: it used to be read after
// this function, straight from the environment, so `--tenant work` sent
// default's exported agent-create token to work's origin). It has no flag. An
// environment value is accepted exactly when an environment MUSTER_TOKEN is:
// as part of an environment URL+token pair. Otherwise it comes from the
// selected file, and only when that file also supplies the URL; an exported
// one beside a file's URL, or beside a flag pair, is refused like a lone half —
// but the refusal is RECORDED (createTokRefusal) rather than returned, and only
// `agent create`, the one verb that sends the token, raises it (round 2, F1).
func resolvePaired(where string, fileVals map[string]string, getenv func(string) string, flagURL, flagToken string) (config, error) {
	u := firstOverride(pairOverride{"--api-url", flagURL}, pairOverride{envAPIURL, getenv(envAPIURL)})
	t := firstOverride(pairOverride{"--token", flagToken}, pairOverride{envToken, getenv(envToken)},
		pairOverride{envTokenAlias, getenv(envTokenAlias)})

	var c config
	switch {
	case u.value != "" && t.value != "" && u.fromFlag() != t.fromFlag():
		return config{}, failf(exitUsage,
			"%s comes from %s but %s comes from %s, for %s: %s from one source (both flags, or both "+
				"environment variables) when more than one tenant is configured, and no request was sent.",
			u.name, u.sourceKind(), t.name, t.sourceKind(), where, pairRefusal)
	case u.value != "" && t.value != "":
		c = config{apiURL: u.value, token: t.value}
	case u.value != "" || t.value != "":
		lone, missing := u.name, "a token ("+envToken+" or --token)"
		if t.value != "" {
			lone, missing = t.name, "a URL ("+envAPIURL+" or --api-url)"
		}
		return config{}, failf(exitUsage,
			"%s is set without %s for %s: %s when more than one tenant is configured, "+
				"and no request was sent. Set both, or neither to use that file's own pair.",
			lone, missing, where, pairRefusal)
	default:
		c.apiURL = fileVals[envAPIURL]
		c.token = fileVals[envToken]
		if c.token == "" {
			c.token = fileVals[envTokenAlias]
		}
	}
	envPair := u.value != "" && t.value != "" && !u.fromFlag() && !t.fromFlag()
	switch ct := getenv(envAgentCreateToken); {
	case ct != "" && envPair:
		c.createTok = ct
	case ct != "":
		from := "the URL comes from " + where
		if u.value != "" {
			from = u.name + " comes from " + u.sourceKind()
		}
		c.createTokRefusal = fmt.Sprintf(
			"%s is set in the environment but %s: %s (an environment %s only with %s and %s), "+
				"and no request was sent. On a host with more than one tenant, put %s in that tenant's file, "+
				"or export it beside an %s + %s pair.",
			envAgentCreateToken, from, pairRefusal, envAgentCreateToken, envAPIURL, envToken,
			envAgentCreateToken, envAPIURL, envToken)
	case u.value == "":
		c.createTok = fileVals[envAgentCreateToken]
	}
	if c.apiURL == "" {
		return config{}, failf(exitUsage, "no API URL: %s sets no %s", where, envAPIURL)
	}
	return checkAPIURL(c)
}

// checkAPIURL validates and normalises the resolved base URL.
func checkAPIURL(c config) (config, error) {
	u, err := url.Parse(c.apiURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return config{}, failf(exitUsage, "invalid API URL %q: want an absolute http(s) URL", c.apiURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return config{}, failf(exitUsage, "invalid API URL scheme %q: want http or https", u.Scheme)
	}
	c.apiURL = strings.TrimSuffix(c.apiURL, "/")
	return c, nil
}
