package push

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// TestASilentPayloadIsFannedOutOnlyToQuietBrowsers: through SendTo, a Silent
// payload reaches the non-Apple browser (with "silent":true) and neither a
// native device nor Safari; an ordinary payload reaches them all (control).
func TestASilentPayloadIsFannedOutOnlyToQuietBrowsers(t *testing.T) {
	sent := fanOut(t, Payload{Type: "attention", ID: "episode-7", Tag: "attention-episode-7", Title: "t", Silent: true})
	if !strings.Contains(sent[epFCM], `"silent":true`) {
		t.Fatalf("the FCM browser got %q; want the payload, silent", sent[epFCM])
	}
	for _, ep := range []string{epNtfy, epNtfyOn, epApple} {
		if b, ok := sent[ep]; ok {
			t.Errorf("%s was sent the silent re-show: %q", ep, b)
		}
	}
	all := fanOut(t, Payload{Type: "attention", ID: "episode-7", Tag: "attention-episode-7", Title: "t"})
	for _, ep := range []string{epFCM, epNtfy, epApple} {
		if _, ok := all[ep]; !ok {
			t.Errorf("CONTROL: %s was not sent an ordinary payload", ep)
		}
	}
}

// TestASilentPayloadReachesOnlyDevicesThatShowItQuietly: a Silent payload
// (a quiet re-show) goes to a browser on a push service that honours the
// Notifications API's `silent`, and to no native device (its app ignores
// payload v2's "update") and no Safari device (WebKit ignores `silent`); the
// browser's JSON carries "silent":true, and an ordinary payload carries no
// silent field at all.
func TestASilentPayloadReachesOnlyDevicesThatShowItQuietly(t *testing.T) {
	dev := func(kind, endpoint string) Device {
		var d Device
		d.Kind, d.Sub.Endpoint = kind, endpoint
		return d
	}
	devs := []Device{
		dev(KindBrowser, "https://fcm.googleapis.com/fcm/send/chrome"),
		dev(KindBrowser, "https://updates.push.services.mozilla.com/wpush/v2/firefox"),
		dev(KindBrowser, "https://web.push.apple.com/safari"),
		dev(KindAndroid, "https://ntfy.example.test/up-android"),
	}
	var got []string
	for _, d := range silentCapable(devs) {
		got = append(got, d.Sub.Endpoint)
	}
	want := []string{"https://fcm.googleapis.com/fcm/send/chrome", "https://updates.push.services.mozilla.com/wpush/v2/firefox"}
	if !slices.Equal(got, want) {
		t.Fatalf("silent devices: %q, want %q", got, want)
	}
	b, _ := json.Marshal(Payload{Type: "attention", Tag: "attention-episode-7", Silent: true})
	if !strings.Contains(string(b), `"silent":true`) {
		t.Fatalf("silent payload JSON %s carries no silent flag", b)
	}
	b, _ = json.Marshal(Payload{Type: "attention", Tag: "attention-episode-7"})
	if strings.Contains(string(b), "silent") {
		t.Fatalf("an ordinary payload carries a silent field: %s", b)
	}
}
