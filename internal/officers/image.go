package officers

import (
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// A PER-OFFICER RUNNER IMAGE (docs/officers.md "A custom runner image").
//
// An officer (untiered: its `image`; tiered: tiers.<pool>.image) may run its
// instances on an image other than the deployment's claude-code image
// (MUSTER_AGENT_CC_IMAGE): one built FROM muster's runner image that adds the
// officer's own tooling. The image is the pod's whole userland and runs with
// every tool allowed, so it is refused at boot unless it is
//
//   - pinned by DIGEST (@sha256:…; a tag beside it is allowed and ignored for
//     the pin). 🔴 A tag alone is refused: anyone who can push to an allowed
//     repository can move a tag, and the next pod would run that code with
//     every tool allowed and the session's credentials;
//   - named with a REAL registry host: the first component contains '.' or
//     ':' or is exactly "localhost" — the rule container runtimes use, so
//     `registry/x` (a Docker Hub path to a runtime) is never read as a host
//     called "registry". The same holds for every allowlist entry;
//   - in a repository the deployment allowlists in ImageRegistriesEnv: its
//     repository (the reference without tag and digest) IS an entry, or lies
//     under one (`<entry>/…`). No allowlist: every override is refused.
//
// muster cannot see what an image was built FROM; that it carries a ccd and a
// CLI muster speaks to is the image builder's promise (docs).
// ---------------------------------------------------------------------------

// ImageRegistriesEnv is the deployment's allowlist of the image repositories
// an officer's `image` may name: comma-separated `<registry host>[/<path>]`.
const ImageRegistriesEnv = "MUSTER_OFFICER_IMAGE_REGISTRIES"

const (
	imageComponent = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	imageHost      = `[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*(?::[0-9]{1,5})?`
)

var (
	// imageRefRE is a FULL reference: host, at least one path component, then
	// a tag and/or a digest (groups 1 repository, 2 tag, 3 digest).
	imageRefRE = regexp.MustCompile(`^(` + imageHost + `(?:/` + imageComponent + `)+)(?::([A-Za-z0-9_][A-Za-z0-9_.-]{0,127}))?(@sha256:[a-f0-9]{64})?$`)
	// imageEntryRE is an allowlist entry: a host, optionally a path.
	imageEntryRE = regexp.MustCompile(`^` + imageHost + `(?:/` + imageComponent + `)*$`)
)

// registryHost reports whether the first '/'-component of s is what a
// container runtime treats as a registry host rather than a Docker Hub
// namespace: it contains '.' or ':' or is exactly "localhost".
func registryHost(s string) bool {
	host, _, _ := strings.Cut(s, "/")
	return strings.ContainsAny(host, ".:") || host == "localhost"
}

// parseImageRegistries reads ImageRegistriesEnv: trimmed, a trailing '/'
// dropped, empty entries skipped; an entry that is not a host[/path] (a tag,
// a digest, a scheme, a glob) is an error naming it.
func parseImageRegistries(raw string) ([]string, error) {
	var out []string
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSuffix(strings.TrimSpace(e), "/")
		if e == "" {
			continue
		}
		if !imageEntryRE.MatchString(e) {
			return nil, fmt.Errorf("%s entry %q is not an image repository prefix (<registry host>[/<path>], no scheme, tag, digest or glob)", ImageRegistriesEnv, e)
		}
		if !registryHost(e) {
			return nil, fmt.Errorf("%s entry %q: its first component is not a registry host (it needs a '.' or a ':', or to be localhost; a container runtime reads a bare name as a Docker Hub path)", ImageRegistriesEnv, e)
		}
		out = append(out, e)
	}
	return out, nil
}

// ImageAllowed is THE rule for an officer's `image`: nil when ref is a
// digest-pinned, well-formed reference with a real registry host whose repository is an entry of allow or lies
// under one. Every refusal names the reason.
func ImageAllowed(ref string, allow []string) error {
	m := imageRefRE.FindStringSubmatch(ref)
	if m == nil {
		return fmt.Errorf("image %q is not a full image reference (<registry host>/<path>[:<tag>][@sha256:<digest>])", ref)
	}
	repo, digest := m[1], m[3]
	if !registryHost(repo) {
		return fmt.Errorf("image %q: its first component is not a registry host (it needs a '.' or a ':', or to be localhost; a container runtime would pull it from Docker Hub)", ref)
	}
	if digest == "" {
		return fmt.Errorf("image %q must be pinned by digest (@sha256:<digest>): a tag can be moved by anyone who can push to the repository, and the pod runs the image with every tool allowed", ref)
	}
	if len(allow) == 0 {
		return fmt.Errorf("image %q: %s is not set, so no officer image is allowed (list the registries — and paths — custom officer images may come from)", ref, ImageRegistriesEnv)
	}
	for _, e := range allow {
		if repo == e || strings.HasPrefix(repo, e+"/") {
			return nil
		}
	}
	return fmt.Errorf("image %q is not in a repository %s allows (%s)", ref, ImageRegistriesEnv, strings.Join(allow, ", "))
}
