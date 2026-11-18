package push

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTheIconIsTheBrowsersOnly: the browser payload carries Icon (the
// service worker draws it); payload v2 never does — the Android app reads
// the instance logo from GET /api/instance, so an icon field there would
// have no reader. The control: the same payload's v2 still carries its
// title, so the absence is about the icon, not an empty document.
func TestTheIconIsTheBrowsersOnly(t *testing.T) {
	const logo = "https://muster.example.com/brand/icon-192.png?v=0123456789abcdef"
	p := Payload{Type: "notify", Title: "the title", Body: "b", Icon: logo}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"icon":"`+logo+`"`) {
		t.Fatalf("the browser payload has no icon: %s", b)
	}
	v2, _ := json.Marshal(p.V2())
	if strings.Contains(string(v2), "icon") || strings.Contains(string(v2), logo) {
		t.Fatalf("payload v2 carries the icon: %s", v2)
	}
	if !strings.Contains(string(v2), `"title":"the title"`) {
		t.Fatalf("CONTROL: payload v2 lost its title: %s", v2)
	}
}
