// Package plainscan holds the scans the plain-UI guards share (the operator
// rule: muster is for people who are not engineers, so its own chrome shows
// no address, flag, variable, command or jargon in its top layer).
//
// One definition per scan, so a guard in internal/api (rendered pages) and one
// in cmd/muster-server (the feature catalog's own words) cannot drift into
// two ideas of what "plain" means. The tests that use them run their own
// positive and negative controls.
package plainscan

import (
	"io"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

// Address is a tmux target's "tmux ", a place's " @ " and a window index —
// ":<digit>" after a name, not after a digit (a clock time, "01:10 UTC", is
// not one).
var Address = regexp.MustCompile(`\btmux |\s@\s|[^\s\d]:[0-9]`)

// Host is a command-line flag ("-interactive", "-opencode-send": a dash
// starting a word, bare or inside backticks or quotes — "`-interactive`",
// "\"-s\""), the hub's raw "mirror:" prefix, the host binary's name, a quoted
// id the hub appends ("(host "atlas", …)"), a token counter ("~+140 ctx",
// "+3.3k ctx"), and the tool-call count's own words ("2 tool calls").
var Host = regexp.MustCompile("(^|[\\s(`\"'“‘])--?[a-z][a-z-]*\\b|\\bmirror: |muster-mirror|\\(host \"|\\bctx\\b|\\btool calls?\\b|\\bdetails have more\\b")

// Setup is how a feature is SET UP, which belongs behind an admin's ⓘ: an
// environment variable's shape (MUSTER_STT_TOKEN, COMPOSE_PROFILES,
// S3_STORE_SECRET_KEY), an assignment, a placeholder ("<value>"), Docker
// Compose and its .env file, a docs path, and the deployment and protocol
// words a non-engineer does not use (Kubernetes, pod, API, URL, MCP, a token,
// a JWT, VAPID, GoTrue, S3, a webhook, "misconfigured").
var Setup = regexp.MustCompile(`\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b|=|<[a-z][^>]*>|\.env\b|\bdocs/|\.md\b|` +
	`(?i:\bdocker\b|\bcompose\b|\bkubernetes\b|\bpods?\b|\bAPI\b|\bURLs?\b|\bMCP\b|\btokens?\b|\bJWT\b|\bVAPID\b|\bGoTrue\b|\bS3\b|\bwebhooks?\b|\bmisconfigured\b)`)

// Jargon is the first jargon any scan finds in s, or "" when s is plain.
func Jargon(s string) string {
	for _, re := range []*regexp.Regexp{Setup, Host, Address} {
		if m := re.FindString(s); m != "" {
			return m
		}
	}
	return ""
}

// TopLayerTexts is what a person reads on markup without opening anything:
// every text node and every tooltip, aria-label and placeholder — but not the
// ⓘ details sheets ([data-info-sheet], the one place the address belongs),
// nor scripts, styles and templates, nor data-* attributes (never drawn).
func TopLayerTexts(markup string) ([]string, error) {
	z := html.NewTokenizer(strings.NewReader(markup))
	var out []string
	skip := 0 // depth inside a skipped subtree
	var stack []bool
	for {
		switch z.Next() {
		case html.ErrorToken:
			if z.Err() != io.EOF {
				return nil, z.Err()
			}
			return out, nil
		case html.TextToken:
			if skip == 0 {
				if s := strings.TrimSpace(string(z.Text())); s != "" {
					out = append(out, s)
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			skipped := slices.Contains([]string{"script", "style", "template"}, tok.Data)
			for _, a := range tok.Attr {
				switch a.Key {
				case "data-info-sheet":
					skipped = true
				case "title", "aria-label", "placeholder":
					if skip == 0 && !skipped {
						out = append(out, a.Val)
					}
				}
			}
			if tok.Type == html.SelfClosingTagToken || isVoid(tok.Data) {
				continue
			}
			stack = append(stack, skipped)
			if skipped {
				skip++
			}
		case html.EndTagToken:
			if n := len(stack); n > 0 {
				if stack[n-1] {
					skip--
				}
				stack = stack[:n-1]
			}
		}
	}
}

func isVoid(tag string) bool {
	return slices.Contains([]string{"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"}, tag)
}
