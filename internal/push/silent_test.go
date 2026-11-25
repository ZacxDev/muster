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
	for _, d := range silentCapable(devs, false) {
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

// TestASilentUpdateReachesPairedPhonesAsAnUpdate: a payload both Silent and
// Update (a notification replaced in place: the fleet's "N proposals
// waiting" with a new count) reaches the non-Apple browser with
// "silent":true AND the paired phone, whose payload v2 carries
// "update":true — what the Android app draws without alerting
// (onlyAlertOnce) — and never Safari, which ignores `silent`. A Silent
// payload without Update still skips the phone (the control): payload v2 has
// no field that would keep it quiet.
//
// Red at origin/main: silentCapable skipped every native device, so a phone
// kept the old count and its link to a proposal no longer waiting.
func TestASilentUpdateReachesPairedPhonesAsAnUpdate(t *testing.T) {
	p := Payload{Type: "attention", ID: "fleet-proposals", Tag: "fleet-proposals", Title: "2 proposals waiting",
		Data: map[string]string{"url": "/tasks?chief=open&fleet=proposed&proposal=31"}, Update: true, Silent: true}
	sent := fanOut(t, p)
	if !strings.Contains(sent[epFCM], `"silent":true`) {
		t.Fatalf("the FCM browser got %q; want the payload, silent", sent[epFCM])
	}
	var v2 PayloadV2
	if err := json.Unmarshal([]byte(sent[epNtfy]), &v2); err != nil {
		t.Fatalf("the paired phone got %q: %v", sent[epNtfy], err)
	}
	if !v2.Update || v2.Tag != "fleet-proposals" || v2.Title != "2 proposals waiting" || v2.URL != "/tasks?chief=open&fleet=proposed&proposal=31" {
		t.Fatalf("the paired phone's payload v2: %+v; want the update, in place, under the same tag", v2)
	}
	if b, ok := sent[epApple]; ok {
		t.Errorf("Safari was sent the silent update: %q", b)
	}
	p.Update = false
	if b, ok := fanOut(t, p)[epNtfy]; ok {
		t.Errorf("CONTROL: a silent re-show without update reached the paired phone: %q", b)
	}
}
