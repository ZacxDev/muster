package plugins

import (
	"strings"
	"testing"
)

// TestAMessageURLIsAnHTTPURLOrRefused: MessageIn.url is optional; when sent it
// is an absolute http(s) URL with a host (fence.WebURL), and anything else is
// refused by name rather than stored.
func TestAMessageURLIsAnHTTPURLOrRefused(t *testing.T) {
	base := MessageIn{ExternalID: "x1", Body: "b"}
	for _, ok := range []string{"", "https://example.test/p/1", "http://example.test:8080/a?b=c#d"} {
		in := base
		in.URL = ok
		if err := in.Validate(); err != nil {
			t.Errorf("url %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,x", "ftp://example.test/x", "//example.test/x", "/relative",
		"https://", "https://user:pw@example.test/", "https://example.test/a b", "https://example.test/‮", "https://example.test/\x00",
		"https://example.test/" + strings.Repeat("a", 2048)} {
		in := base
		in.URL = bad
		if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "url is an absolute http or https URL") {
			t.Errorf("url %q: %v, want refused by name", bad, err)
		}
	}
}
