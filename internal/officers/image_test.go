package officers_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// A PER-OFFICER RUNNER IMAGE. Allowlist entries and references are synthetic;
// every repository is distinct, and one entry is a PREFIX of another
// organisation's name (example-org vs example-orgx), so a match that is not
// bounded by '/' cannot pass.
// ---------------------------------------------------------------------------

const (
	imgAllow   = "ghcr.io/example-org, registry.example.test:5000/team/runners/"
	imgDigest  = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	imgOfficer = "release:example-web"
)

var imgAllowList = []string{"ghcr.io/example-org", "registry.example.test:5000/team/runners"}

// TestImageAllowed: the one rule. Allowed: a pinned reference whose
// repository is an entry or lies under one. Refused, each with its reason:
// unpinned, outside every entry, a look-alike prefix, a malformed reference,
// and anything at all with no allowlist.
func TestImageAllowed(t *testing.T) {
	for _, ref := range []string{
		"ghcr.io/example-org/officer-runner:1.2.3",
		"ghcr.io/example-org/officer-runner" + imgDigest,
		"ghcr.io/example-org/officer-runner:1.2.3" + imgDigest,
		"ghcr.io/example-org/deep/path/runner:abc1234",
		"registry.example.test:5000/team/runners:7",
		"registry.example.test:5000/team/runners/audit:7",
	} {
		if err := officers.ImageAllowed(ref, imgAllowList); err != nil {
			t.Errorf("%s: refused: %v", ref, err)
		}
	}
	for ref, says := range map[string]string{
		"ghcr.io/example-org/officer-runner":           "must pin a tag or a digest",
		"ghcr.io/example-orgx/officer-runner:1":        "is not in a repository",
		"ghcr.io/example/officer-runner:1":             "is not in a repository",
		"docker.io/example-org/officer-runner:1":       "is not in a repository",
		"registry.example.test:5000/team/runnersx:7":   "is not in a repository",
		"registry.example.test:5000/team/other/x:7":    "is not in a repository",
		"https://ghcr.io/example-org/officer-runner:1": "is not a full image reference",
		"ghcr.io/example-org/../x/officer-runner:1":    "is not a full image reference",
		"ghcr.io/example-org/Runner:1":                 "is not a full image reference",
		"ghcr.io:1":                                    "is not a full image reference",
		"ghcr.io/example-org/officer-runner:1 ":        "is not a full image reference",
	} {
		if err := officers.ImageAllowed(ref, imgAllowList); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%q: err = %v, want one saying %q", ref, err, says)
		}
	}
	if err := officers.ImageAllowed("ghcr.io/example-org/officer-runner:1.2.3", nil); err == nil || !strings.Contains(err.Error(), officers.ImageRegistriesEnv+" is not set") {
		t.Fatalf("no allowlist: err = %v, want a refusal naming %s", err, officers.ImageRegistriesEnv)
	}
}

func imgEnv(allow string) map[string]string {
	return map[string]string{"MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB": "tok-a", officers.ImageRegistriesEnv: allow}
}

// TestParseReadsAnOfficerImage: the untiered `image`, and each pool's
// tiers.<pool>.image, land on the pool that runs it; an officer with none
// runs the deployment's ("").
func TestParseReadsAnOfficerImage(t *testing.T) {
	list, err := officers.Parse(`[{"id":"`+imgOfficer+`","hq_url":"https://hq.example.com/api/v1","image":"ghcr.io/example-org/officer-runner:1.2.3"}]`,
		env(imgEnv(imgAllow)), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].Image; got != "ghcr.io/example-org/officer-runner:1.2.3" {
		t.Fatalf("image = %q", got)
	}
	list, err = officers.Parse(`[{"id":"`+imgOfficer+`","hq_url":"https://hq.example.com/api/v1","tiers":{
		"routine":{"model":"haiku","image":"registry.example.test:5000/team/runners/routine:4"},
		"judgement":{"model":"opus","image":"ghcr.io/example-org/officer-runner`+imgDigest+`"}}}]`, env(imgEnv(imgAllow)), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if j, r := o.Pool(officers.TierJudgement).Image, o.Pool(officers.TierRoutine).Image; j != "ghcr.io/example-org/officer-runner"+imgDigest ||
		r != "registry.example.test:5000/team/runners/routine:4" || o.Image != j {
		t.Fatalf("judgement %q, routine %q, officer %q", j, r, o.Image)
	}
	list, err = officers.Parse(`[{"id":"`+imgOfficer+`","hq_url":"https://hq.example.com/api/v1"}]`, env(imgEnv("")), "muster-agent-")
	if err != nil || list[0].Image != "" {
		t.Fatalf("no image: %v, %q", err, list[0].Image)
	}
}

// TestParseRefusesAnImageTheDeploymentDoesNotAllow: boot refusals, each naming
// the officer and the field.
func TestParseRefusesAnImageTheDeploymentDoesNotAllow(t *testing.T) {
	decl := func(extra string) string {
		return `[{"id":"` + imgOfficer + `","hq_url":"https://hq.example.com/api/v1",` + extra + `}]`
	}
	for name, c := range map[string]struct {
		raw, allow, says string
	}{
		"no allowlist":       {decl(`"image":"ghcr.io/example-org/officer-runner:1"`), "", "officer " + imgOfficer + ": image: "},
		"outside it":         {decl(`"image":"ghcr.io/someone/officer-runner:1"`), imgAllow, "is not in a repository"},
		"unpinned":           {decl(`"image":"ghcr.io/example-org/officer-runner"`), imgAllow, "must pin a tag or a digest"},
		"a routine pool's":   {decl(`"tiers":{"routine":{"model":"haiku","image":"ghcr.io/someone/r:1"},"judgement":{"model":"opus"}}`), imgAllow, "tiers.routine.image"},
		"a judgement pool's": {decl(`"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus","image":"ghcr.io/example-org/j"}}`), imgAllow, "tiers.judgement.image"},
		"top-level beside tiers": {decl(`"image":"ghcr.io/example-org/officer-runner:1","tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}}`),
			imgAllow, "remove the top-level image"},
		"a malformed allowlist entry": {decl(`"model":"opus"`), "ghcr.io/example-org:latest", "is not an image repository prefix"},
		"a glob entry":                {decl(`"model":"opus"`), "ghcr.io/*", "is not an image repository prefix"},
		"a scheme entry":              {decl(`"model":"opus"`), "https://ghcr.io/example-org", "is not an image repository prefix"},
	} {
		_, err := officers.Parse(c.raw, env(imgEnv(c.allow)), "muster-agent-")
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, c.says)
		}
	}
}
