package opencodehost

import "testing"

// TestMajorVersionReadsOpencodesVersionLine: the major version from
// `opencode --version` as each line shape prints it, and false for a line
// that names none. Majors are pairwise distinct and never MinMajorVersion
// alone, so a parser returning a constant cannot pass.
func TestMajorVersionReadsOpencodesVersionLine(t *testing.T) {
	for in, want := range map[string]int{
		"1.19.5":                   1,
		"opencode v2.0.32+6ec5682": 2,
		"v3.4.1":                   3,
		" opencode 12.0.0\n":       12,
		"7":                        7,
	} {
		if got, ok := MajorVersion(in); !ok || got != want {
			t.Errorf("MajorVersion(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "opencode", "dev", "v.1.2", "2x.0", "opencode (dev build)", "12345678.0"} {
		if got, ok := MajorVersion(in); ok {
			t.Errorf("MajorVersion(%q) = %d, true; want no version", in, got)
		}
	}
}
