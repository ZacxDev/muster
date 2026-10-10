package provision

import "testing"

// fingerprintFixture declares no Security, like every gateway-kind spec.
func fingerprintFixture() Spec {
	return Spec{
		Ref:       Ref{Name: "pinned-wren", ID: 31},
		Runtime:   Runtime{Image: "ghcr.io/example-org/runtime:9.1", WorkingDir: "/srv/w"},
		Env:       []EnvVar{{Name: "A", Value: "1"}},
		Secrets:   []EnvVar{{Name: "S", Value: "fixture-secret"}},
		Ports:     []Port{{Name: DefaultPortName, Port: 18789}},
		Health:    Health{HTTPGetPath: "/", PortName: DefaultPortName},
		Workspace: Workspace{Path: "/srv/w", Size: "4Gi", Persist: true},
	}
}

// pinnedFingerprint is Fingerprint(fingerprintFixture()) MEASURED AT 34118de,
// before provision.Security existed. Adding the type must not move it: a moved
// fingerprint is a roll of every live instance on its next Update for a pod
// template that did not change.
const pinnedFingerprint = "674044b3f0e73f1d5485be9ff15f29e0b16648a0be495411de49dea6d435d8d4"

func TestASpecDeclaringNoSecurityKeepsItsPreSecurityFingerprint(t *testing.T) {
	if got := Fingerprint(fingerprintFixture()); got != pinnedFingerprint {
		t.Fatalf("Fingerprint = %s, want the pre-Security value %s", got, pinnedFingerprint)
	}
}

func TestDeclaringSecurityMovesTheFingerprint(t *testing.T) {
	base := fingerprintFixture()
	for name, sec := range map[string]Security{
		"user":       {RunAsUser: 1000},
		"group":      {RunAsGroup: 1000},
		"fsgroup":    {FSGroup: 1000},
		"nonroot":    {RunAsNonRoot: true},
		"restricted": {Restricted: true},
		"no sa":      {NoServiceAccountToken: true},
	} {
		s := fingerprintFixture()
		s.Security = sec
		if Fingerprint(s) == Fingerprint(base) {
			t.Errorf("%s: declaring %+v did not move the fingerprint; the pod template changed and nothing would roll", name, sec)
		}
	}
}
