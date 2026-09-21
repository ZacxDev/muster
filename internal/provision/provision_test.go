package provision_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// --------------------------------------------------------------------------
// Spec validity
// --------------------------------------------------------------------------

func TestSpecValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*provision.Spec)
		wantErr bool
		mustSay string
	}{
		{name: "minimal is valid"},
		{
			name:    "no runtime at all",
			mutate:  func(s *provision.Spec) { s.Runtime = provision.Runtime{} },
			wantErr: true, mustSay: "Image or Command",
		},
		{
			name:   "command only is valid — it is a union",
			mutate: func(s *provision.Spec) { s.Runtime = provision.Runtime{Command: []string{"/usr/bin/agent"}} },
		},
		{
			name:    "empty name",
			mutate:  func(s *provision.Spec) { s.Ref.Name = "" },
			wantErr: true, mustSay: "[a-z0-9-]",
		},
		{
			name:    "name with an underscore",
			mutate:  func(s *provision.Spec) { s.Ref.Name = "bad_name" },
			wantErr: true, mustSay: "[a-z0-9-]",
		},
		{
			name:    "name ending in a dash",
			mutate:  func(s *provision.Spec) { s.Ref.Name = "trailing-" },
			wantErr: true, mustSay: "[a-z0-9-]",
		},
		{
			name:    "name over the length budget",
			mutate:  func(s *provision.Spec) { s.Ref.Name = strings.Repeat("a", 49) },
			wantErr: true, mustSay: "1-48",
		},
		{
			name:   "name exactly at the length budget",
			mutate: func(s *provision.Spec) { s.Ref.Name = strings.Repeat("a", 48) },
		},
		{
			name: "relative file path",
			mutate: func(s *provision.Spec) {
				s.Files = []provision.File{{Path: "etc/relative"}}
			},
			wantErr: true, mustSay: "must be absolute",
		},
		{
			name: "duplicate file paths",
			mutate: func(s *provision.Spec) {
				s.Files = []provision.File{
					{Path: "/etc/muster/a", Content: []byte("first")},
					{Path: "/etc/muster/a", Content: []byte("second")},
				}
			},
			wantErr: true, mustSay: "appears twice",
		},
		{
			name: "port number out of range",
			mutate: func(s *provision.Spec) {
				s.Ports = []provision.Port{{Name: "gateway", Port: 70000}}
			},
			wantErr: true, mustSay: "out of range",
		},
		{
			name: "duplicate port names",
			mutate: func(s *provision.Spec) {
				s.Ports = []provision.Port{{Name: "gateway", Port: 1}, {Name: "gateway", Port: 2}}
			},
			wantErr: true, mustSay: "appears twice",
		},
		{
			name: "repo url carrying a credential",
			mutate: func(s *provision.Spec) {
				s.Repo = provision.Repo{URL: "https://someuser:sometoken@git.example.test/org/repo.git"}
			},
			wantErr: true, mustSay: "must not carry credentials",
		},
		{
			name: "repo url without one",
			mutate: func(s *provision.Spec) {
				s.Repo = provision.Repo{URL: "https://git.example.test/org/repo.git", Branch: "main"}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := provisiontest.MinimalSpec("valid")
			if c.mutate != nil {
				c.mutate(&spec)
			}
			err := spec.Validate()
			if c.wantErr {
				if !errors.Is(err, provision.ErrInvalidSpec) {
					t.Fatalf("want ErrInvalidSpec, got %v", err)
				}
				if c.mustSay != "" && !strings.Contains(err.Error(), c.mustSay) {
					t.Fatalf("message must contain %q, got %q", c.mustSay, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want valid, got %v", err)
			}
		})
	}
}

// TestRepoCredentialRefusalIsReachable proves the credential check is not
// shadowed by an earlier one: the spec is valid in every other respect, so the
// only thing that can fail it is the branch under test.
func TestRepoCredentialRefusalIsReachable(t *testing.T) {
	spec := provisiontest.MinimalSpec("clone")
	spec.Repo = provision.Repo{URL: "https://git.example.test/org/repo.git"}
	if err := spec.Validate(); err != nil {
		t.Fatalf("control: the same spec without a credential must be valid, got %v", err)
	}
	spec.Repo.URL = "https://someuser:sometoken@git.example.test/org/repo.git"
	err := spec.Validate()
	if err == nil || !strings.Contains(err.Error(), "must not carry credentials") {
		t.Fatalf("want the credential refusal, got %v", err)
	}
}

func TestFileEffectiveMode(t *testing.T) {
	if got := (provision.File{}).EffectiveMode(); got != 0o644 {
		t.Errorf("plain file default mode %o, want 644", got)
	}
	if got := (provision.File{Secret: true}).EffectiveMode(); got != 0o600 {
		t.Errorf("secret file default mode %o, want 600 — a secret that lands world-readable because "+
			"one driver defaulted differently is not a difference anyone looks for", got)
	}
	if got := (provision.File{Mode: 0o755}).EffectiveMode(); got != 0o755 {
		t.Errorf("explicit mode %o, want 755", got)
	}
}

func TestDesiredReplicas(t *testing.T) {
	if got := (provision.Spec{}).DesiredReplicas(); got != 1 {
		t.Errorf("unset Replicas means one, got %d", got)
	}
	if got := (provision.Spec{Replicas: 4}).DesiredReplicas(); got != 4 {
		t.Errorf("Replicas 4, got %d", got)
	}
	if got := (provision.Spec{Replicas: -2}).DesiredReplicas(); got != 1 {
		t.Errorf("a negative Replicas must not become a negative desired count, got %d", got)
	}
}

func TestSpecPortNumber(t *testing.T) {
	named := provision.Spec{Ports: []provision.Port{
		{Name: "metrics", Port: 9102},
		{Name: provision.DefaultPortName, Port: 8421},
	}}
	if got := named.PortNumber(provision.DefaultPortName); got != 8421 {
		t.Errorf("named lookup got %d, want 8421", got)
	}
	if got := named.PortNumber("metrics"); got != 9102 {
		t.Errorf("named lookup got %d, want 9102", got)
	}
	// Fall back to the sole port only when there is exactly one.
	single := provision.Spec{Ports: []provision.Port{{Port: 7777}}}
	if got := single.PortNumber(provision.DefaultPortName); got != 7777 {
		t.Errorf("single unnamed port got %d, want 7777", got)
	}
	ambiguous := provision.Spec{Ports: []provision.Port{{Port: 1111}, {Port: 2222}}}
	if got := ambiguous.PortNumber(provision.DefaultPortName); got != 0 {
		t.Errorf("two unnamed ports must resolve to 0 rather than guess, got %d", got)
	}
	if got := (provision.Spec{}).PortNumber(""); got != 0 {
		t.Errorf("no ports at all, got %d", got)
	}
}

// --------------------------------------------------------------------------
// Fingerprint
// --------------------------------------------------------------------------

// TestFingerprintMovesForEveryFieldThatMatters is the mechanical control the
// divergence check needs: for each covered field, change it and watch the
// digest move. A Fingerprint that hashed only the name would pass a test that
// merely asserted it was stable.
func TestFingerprintMovesForEveryFieldThatMatters(t *testing.T) {
	base := provisiontest.MinimalSpec("fp")
	baseFP := provision.Fingerprint(base)

	mutations := map[string]func(*provision.Spec){
		"image":         func(s *provision.Spec) { s.Runtime.Image = "ghcr.io/muster-example/changed:9" },
		"command":       func(s *provision.Spec) { s.Runtime.Command = []string{"/bin/other"} },
		"args":          func(s *provision.Spec) { s.Runtime.Args = []string{"--flag"} },
		"workdir":       func(s *provision.Spec) { s.Runtime.WorkingDir = "/srv" },
		"env value":     func(s *provision.Spec) { s.Env = []provision.EnvVar{{Name: "MUSTER_INSTANCE", Value: "other"}} },
		"env name":      func(s *provision.Spec) { s.Env = []provision.EnvVar{{Name: "OTHER", Value: "fp"}} },
		"secret":        func(s *provision.Spec) { s.Secrets = []provision.EnvVar{{Name: "K", Value: "v"}} },
		"file content":  func(s *provision.Spec) { s.Files = []provision.File{{Path: "/a", Content: []byte("x")}} },
		"file mode":     func(s *provision.Spec) { s.Files = []provision.File{{Path: "/a", Content: []byte("x"), Mode: 0o700}} },
		"init":          func(s *provision.Spec) { s.Init = []string{"echo hello"} },
		"resources":     func(s *provision.Spec) { s.Resources = provision.Resources{MemoryLimit: "3Gi"} },
		"workspace":     func(s *provision.Spec) { s.Workspace = provision.Workspace{Path: "/data", Persist: true} },
		"repo":          func(s *provision.Spec) { s.Repo = provision.Repo{URL: "https://git.example.test/o/r.git"} },
		"port":          func(s *provision.Spec) { s.Ports = []provision.Port{{Name: "gateway", Port: 9999}} },
		"endpoint":      func(s *provision.Spec) { s.Endpoint = &provision.Endpoint{Host: "h.example.test", Port: 1} },
		"config":        func(s *provision.Spec) { s.Config = map[string]any{"k": "v"} },
		"instance name": func(s *provision.Spec) { s.Ref.Name = "fp2" },
	}
	seen := map[string]string{baseFP: "base"}
	for name, mutate := range mutations {
		spec := base
		mutate(&spec)
		fp := provision.Fingerprint(spec)
		if fp == baseFP {
			t.Errorf("changing %s did not move the fingerprint; Create would treat the new spec as identical "+
				"and never rebuild the instance", name)
			continue
		}
		if other, dup := seen[fp]; dup {
			t.Errorf("changing %s collides with %s", name, other)
		}
		seen[fp] = name
	}

	// "file mode" builds on "file content", so the two differ only in the mode
	// bits. Assert that pair explicitly — it is the narrowest distinction the
	// digest has to make, and the one an implementation is most likely to drop.
	withContent := base
	mutations["file content"](&withContent)
	withMode := base
	mutations["file mode"](&withMode)
	if provision.Fingerprint(withContent) == provision.Fingerprint(withMode) {
		t.Error("two specs differing ONLY in a file's mode share a fingerprint")
	}
}

// TestFingerprintIgnoresWhatMustNotCauseARebuild is the other half. Each of
// these changing would make every reconcile pass see divergence.
func TestFingerprintIgnoresWhatMustNotCauseARebuild(t *testing.T) {
	base := provisiontest.MinimalSpec("stable")
	base.Env = []provision.EnvVar{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}
	baseFP := provision.Fingerprint(base)

	same := map[string]func(*provision.Spec){
		"correlation id": func(s *provision.Spec) { s.Ref.ID = 4217 },
		"replica count":  func(s *provision.Spec) { s.Replicas = 3 },
		"labels":         func(s *provision.Spec) { s.Labels = map[string]string{"team": "ops"} },
		"env order":      func(s *provision.Spec) { s.Env = []provision.EnvVar{{Name: "B", Value: "2"}, {Name: "A", Value: "1"}} },
	}
	for name, mutate := range same {
		spec := base
		mutate(&spec)
		if got := provision.Fingerprint(spec); got != baseFP {
			t.Errorf("changing %s moved the fingerprint; every reconcile pass would see divergence and "+
				"rebuild the instance", name)
		}
	}
}

// TestFingerprintDoesNotCarrySecretValues. The digest is written into a label
// or annotation on a real backend, and those are readable by anything that can
// read the object.
func TestFingerprintDoesNotCarrySecretValues(t *testing.T) {
	const secret = "sup3rs3cret-probe-value"
	spec := provisiontest.MinimalSpec("hashed")
	spec.Secrets = []provision.EnvVar{{Name: "TOKEN", Value: secret}}
	spec.Files = []provision.File{{Path: "/etc/muster/creds", Content: []byte(secret), Secret: true}}
	fp := provision.Fingerprint(spec)
	if strings.Contains(fp, secret) {
		t.Fatal("the fingerprint contains a secret value verbatim")
	}
	// Positive control: the digest still MOVES when the secret changes, so the
	// absence above is hashing rather than omission.
	spec.Secrets[0].Value = secret + "-rotated"
	if provision.Fingerprint(spec) == fp {
		t.Fatal("rotating a secret did not move the fingerprint; the value is not merely hidden, it is ignored")
	}
}

// --------------------------------------------------------------------------
// Endpoint resolution
// --------------------------------------------------------------------------

func TestResolveEndpointPrecedence(t *testing.T) {
	tmpl, err := provision.ParseEndpointTemplate("{{.Name}}.{{.Group}}.svc.example.test")
	if err != nil {
		t.Fatalf("ParseEndpointTemplate: %v", err)
	}
	vars := provision.EndpointVars{Name: "agent", Group: "team"}

	// Layer 2: the driver's template.
	ep, err := provision.ResolveEndpoint(nil, tmpl, vars, 8421, "http")
	if err != nil {
		t.Fatalf("template layer: %v", err)
	}
	if ep.Host != "agent.team.svc.example.test" || ep.Port != 8421 {
		t.Fatalf("template layer resolved %+v", ep)
	}

	// Layer 1: the per-instance override wins outright.
	override := &provision.Endpoint{Scheme: "https", Host: "elsewhere.example.test", Port: 19731}
	ep, err = provision.ResolveEndpoint(override, tmpl, vars, 8421, "http")
	if err != nil {
		t.Fatalf("override layer: %v", err)
	}
	if ep.Host != "elsewhere.example.test" || ep.Port != 19731 || ep.Scheme != "https" {
		t.Fatalf("the override did not win: %+v", ep)
	}

	// A partial override inherits the parts it omits rather than being rejected.
	partial := &provision.Endpoint{Host: "partial.example.test"}
	ep, err = provision.ResolveEndpoint(partial, tmpl, vars, 8421, "https")
	if err != nil {
		t.Fatalf("partial override: %v", err)
	}
	if ep.Host != "partial.example.test" || ep.Port != 8421 || ep.Scheme != "https" {
		t.Fatalf("partial override resolved %+v", ep)
	}

	// Layer 3: nothing at all.
	if _, err := provision.ResolveEndpoint(nil, provision.EndpointTemplate{}, vars, 8421, ""); !errors.Is(err, provision.ErrNoEndpoint) {
		t.Fatalf("no template and no override must be ErrNoEndpoint, got %v", err)
	}
	// A template but no port is a different absence, reported as the same kind.
	if _, err := provision.ResolveEndpoint(nil, tmpl, vars, 0, ""); !errors.Is(err, provision.ErrNoEndpoint) {
		t.Fatalf("no port must be ErrNoEndpoint, got %v", err)
	}
}

func TestParseEndpointTemplateRejectsGarbage(t *testing.T) {
	if _, err := provision.ParseEndpointTemplate("{{.Name"); err == nil {
		t.Fatal("an unparseable template must fail at construction, not per request")
	}
	empty, err := provision.ParseEndpointTemplate("   ")
	if err != nil {
		t.Fatalf("an empty template is not an error, it is an absence: %v", err)
	}
	if !empty.IsZero() {
		t.Fatal("a whitespace-only template must be treated as unset")
	}
}

// TestEndpointTemplateRenderingEmptyIsAnError guards the shape where a template
// renders to nothing and the caller gets an address rather than a diagnosis.
func TestEndpointTemplateRenderingEmptyIsAnError(t *testing.T) {
	tmpl, err := provision.ParseEndpointTemplate("{{.Group}}")
	if err != nil {
		t.Fatalf("ParseEndpointTemplate: %v", err)
	}
	if _, err := tmpl.Host(provision.EndpointVars{Name: "agent"}); err == nil {
		t.Fatal("a template that renders empty must error; otherwise the caller dials \"\" and the " +
			"failure is reported as DNS")
	}
}

func TestEndpointRendering(t *testing.T) {
	ep := provision.Endpoint{Host: "h.example.test", Port: 8421}
	if got := ep.Addr(); got != "h.example.test:8421" {
		t.Errorf("Addr() = %q", got)
	}
	if got := ep.URL(); got != "http://h.example.test:8421" {
		t.Errorf("URL() defaults the scheme to http, got %q", got)
	}
	withPath := provision.Endpoint{Scheme: "https", Host: "h.example.test", Port: 443, Path: "v1"}
	if got := withPath.URL(); got != "https://h.example.test:443/v1" {
		t.Errorf("URL() must add the leading slash, got %q", got)
	}
	v6 := provision.Endpoint{Host: "2001:db8::1", Port: 80}
	if got := v6.Addr(); got != "[2001:db8::1]:80" {
		t.Errorf("Addr() must bracket an IPv6 literal, got %q", got)
	}
}

// --------------------------------------------------------------------------
// Grant / Revoke / Exec — the optional-interface branches
// --------------------------------------------------------------------------

// fakeGranter is a driver that implements PolicyGranter, with its capability
// declaration and its grant record under the test's control. It exists so the
// three refusal branches in Grant can each be reached in isolation; the noop
// driver deliberately does NOT implement PolicyGranter, which covers the first
// branch and only the first.
type fakeGranter struct {
	*provision.Noop
	caps    provision.Capabilities
	granted []string
	revoked []string
}

func (f *fakeGranter) Capabilities() provision.Capabilities { return f.caps }

func (f *fakeGranter) Grant(_ context.Context, ref provision.Ref, p provision.Policy) error {
	f.granted = append(f.granted, ref.Name+"/"+p.Name)
	return nil
}

func (f *fakeGranter) Revoke(_ context.Context, ref provision.Ref, name string) error {
	f.revoked = append(f.revoked, ref.Name+"/"+name)
	return nil
}

func newFakeGranter(caps provision.Capabilities) *fakeGranter {
	return &fakeGranter{Noop: provision.MustNewNoop(provision.NoopCapabilities(caps)), caps: caps}
}

// TestGrantRefusalBranches walks every way Grant can refuse, each reached in
// isolation so a mutation that removes one branch cannot be killed by another.
//
// MUTATION MATRIX:
//
//	delete the PolicyGranter type assertion -> "driver is not a granter" RED
//	delete the !caps.Policy branch          -> "granter without the capability" RED
//	delete the !caps.Files branch           -> "policy files, driver cannot place files" RED
//	delete the secret-file loop             -> "policy secret file, no store" RED
//	replace Grant's body with `return nil`  -> ALL FOUR RED
func TestGrantRefusalBranches(t *testing.T) {
	ctx := context.Background()
	ref := provision.Ref{Name: "subject"}
	plain := provision.Policy{Name: "read-only", Rules: []byte(`{"verbs":["get"]}`)}
	withFile := provision.Policy{
		Name:  "with-config",
		Rules: []byte(`{"verbs":["get"]}`),
		Files: []provision.File{{Path: "/etc/muster/policy.conf", Content: []byte("x")}},
	}
	withSecretFile := provision.Policy{
		Name:  "with-credential",
		Rules: []byte(`{"verbs":["get"]}`),
		Files: []provision.File{{Path: "/etc/muster/policy.cred", Content: []byte("x"), Secret: true}},
	}

	t.Run("driver is not a granter", func(t *testing.T) {
		p := provision.MustNewNoop(provision.NoopCapabilities(provision.Capabilities{Policy: true, Files: true, Secrets: true}))
		err := provision.Grant(ctx, p, ref, plain)
		if !errors.Is(err, provision.ErrUnsupported) {
			t.Fatalf("want ErrUnsupported, got %v", err)
		}
		if !strings.Contains(err.Error(), "cannot apply authorisation policy") {
			t.Fatalf("message must name the reason, got %q", err)
		}
	})

	t.Run("granter without the capability", func(t *testing.T) {
		g := newFakeGranter(provision.Capabilities{Files: true, Secrets: true}) // Policy false
		err := provision.Grant(ctx, g, ref, plain)
		if !errors.Is(err, provision.ErrUnsupported) {
			t.Fatalf("want ErrUnsupported, got %v", err)
		}
		if len(g.granted) != 0 {
			t.Fatalf("the refusal still called through to the driver: %v", g.granted)
		}
	})

	t.Run("policy files, driver cannot place files", func(t *testing.T) {
		g := newFakeGranter(provision.Capabilities{Policy: true, Secrets: true}) // Files false
		// Control: the same driver accepts a policy with no files, so the
		// refusal below is about the files and not about the driver.
		if err := provision.Grant(ctx, g, ref, plain); err != nil {
			t.Fatalf("control: a file-less policy must be granted, got %v", err)
		}
		err := provision.Grant(ctx, g, ref, withFile)
		if !errors.Is(err, provision.ErrUnsupported) {
			t.Fatalf("want ErrUnsupported, got %v", err)
		}
		if len(g.granted) != 1 {
			t.Fatalf("expected exactly the control grant to have gone through, got %v", g.granted)
		}
	})

	t.Run("policy secret file, no store", func(t *testing.T) {
		g := newFakeGranter(provision.Capabilities{Policy: true, Files: true}) // Secrets false
		if err := provision.Grant(ctx, g, ref, withFile); err != nil {
			t.Fatalf("control: a non-secret file must be granted, got %v", err)
		}
		err := provision.Grant(ctx, g, ref, withSecretFile)
		if !errors.Is(err, provision.ErrUnsupported) {
			t.Fatalf("want ErrUnsupported, got %v", err)
		}
	})

	t.Run("a capable driver grants", func(t *testing.T) {
		g := newFakeGranter(provision.Capabilities{Policy: true, Files: true, Secrets: true})
		if err := provision.Grant(ctx, g, ref, withSecretFile); err != nil {
			t.Fatalf("a fully capable granter must grant, got %v", err)
		}
		if len(g.granted) != 1 || g.granted[0] != "subject/with-credential" {
			t.Fatalf("grant record is %v", g.granted)
		}
	})
}

func TestRevokeIsAsymmetricOnPurpose(t *testing.T) {
	ctx := context.Background()
	ref := provision.Ref{Name: "subject"}

	// A driver that cannot grant cannot have granted, so revoking is a no-op.
	// Refusing here would leave a caller with no way to clean up after a driver
	// swap.
	if err := provision.Revoke(ctx, provision.MustNewNoop(), ref, "anything"); err != nil {
		t.Fatalf("Revoke against a non-granter must be nil, got %v", err)
	}
	g := newFakeGranter(provision.Capabilities{Policy: true})
	if err := provision.Revoke(ctx, g, ref, "read-only"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if len(g.revoked) != 1 {
		t.Fatalf("Revoke did not reach the driver: %v", g.revoked)
	}
}

// fakeExecer declares Exec and records calls.
type fakeExecer struct {
	*provision.Noop
	caps  provision.Capabilities
	calls int
}

func (f *fakeExecer) Capabilities() provision.Capabilities { return f.caps }

func (f *fakeExecer) Exec(context.Context, provision.Ref, []string, io.Reader, io.Writer, io.Writer) error {
	f.calls++
	return nil
}

func TestExecHelperChecksTheCapabilityNotJustTheAssertion(t *testing.T) {
	ctx := context.Background()
	ref := provision.Ref{Name: "subject"}

	// Not an Execer at all.
	if err := provision.Exec(ctx, provision.MustNewNoop(), ref, []string{"true"}, nil, nil, nil); !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}

	// 🔴 AN EXECER THAT REPORTS Exec FALSE. This is the case a bare type
	// assertion at a call site gets wrong: the assertion succeeds, the call
	// goes through, and a driver whose runtime configuration cannot support
	// exec fails somewhere far from the decision.
	declined := &fakeExecer{Noop: provision.MustNewNoop(), caps: provision.Capabilities{}}
	if err := provision.Exec(ctx, declined, ref, []string{"true"}, nil, nil, nil); !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported for an Execer declaring Exec false, got %v", err)
	}
	if declined.calls != 0 {
		t.Fatal("the helper called through to a driver that declared it could not exec")
	}

	capable := &fakeExecer{Noop: provision.MustNewNoop(), caps: provision.Capabilities{Exec: true}}
	if err := provision.Exec(ctx, capable, ref, []string{"true"}, nil, nil, nil); err != nil {
		t.Fatalf("a capable Execer must be called, got %v", err)
	}
	if capable.calls != 1 {
		t.Fatalf("Exec calls = %d, want 1", capable.calls)
	}
}

func TestPolicyHasRules(t *testing.T) {
	for raw, want := range map[string]bool{
		``:                  false,
		`   `:               false,
		`null`:              false,
		`{}`:                false,
		`[]`:                false,
		`{"verbs":["get"]}`: true,
	} {
		if got := (provision.Policy{Rules: []byte(raw)}).HasRules(); got != want {
			t.Errorf("HasRules(%q) = %t, want %t", raw, got, want)
		}
	}
}
