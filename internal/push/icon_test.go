package push

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPayloadV2CarriesTheLogoIconOnlyForALogo: the native payload names the
// icon and its ETag when the instance has a logo (IconETag set), never
// muster's default icon (no ETag), never on a "resolved", and not when the
// URL is longer than MaxNativeIcon. The browser payload carries Icon in both
// cases and never the ETag.
func TestPayloadV2CarriesTheLogoIconOnlyForALogo(t *testing.T) {
	const logo = "https://muster.example.com/brand/icon-192.png?v=0123456789abcdef"
	p := Payload{Type: "notify", Title: "t", Body: "b", Icon: logo, IconETag: "0123456789abcdef"}
	if v := p.V2(); v.Icon != logo || v.IconETag != "0123456789abcdef" {
		t.Fatalf("a logo's icon: %+v", v)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"icon":"`+logo+`"`) || strings.Contains(string(b), "etag") {
		t.Fatalf("the browser payload: %s", b)
	}
	def := Payload{Type: "notify", Title: "t", Icon: "/static/icons/icon-192.png"}
	if v := def.V2(); v.Icon != "" || v.IconETag != "" {
		t.Fatalf("muster's own icon reached the native payload: %+v", v)
	}
	res := Payload{Type: TypeResolved, Tag: "x", Icon: logo, IconETag: "0123456789abcdef"}
	if v := res.V2(); v.Icon != "" {
		t.Fatalf("a resolved carries an icon: %+v", v)
	}
	long := p
	long.Icon = "https://" + strings.Repeat("a", MaxNativeIcon) + "/brand/icon-192.png"
	if v := long.V2(); v.Icon != "" || v.IconETag != "" {
		t.Fatalf("an icon URL over %d bytes was carried", MaxNativeIcon)
	}
	edge := p
	edge.Icon = strings.Repeat("a", MaxNativeIcon)
	if v := edge.V2(); v.Icon != edge.Icon {
		t.Fatalf("an icon URL of exactly %d bytes was dropped", MaxNativeIcon)
	}
	// A byte JSON would escape (six bytes for one) is never carried: the cap
	// is on what is sent.
	for _, c := range []string{"<", ">", "&", `"`, `\`, " ", "\x00", "é"} {
		esc := p
		esc.Icon = "https://muster.example.com/brand/" + c + "icon-192.png"
		if v := esc.V2(); v.Icon != "" {
			t.Errorf("an icon URL holding %q was carried", c)
		}
	}
}

// TestTheWorstCaseAskStillFitsWithTheLongestIcon: the worst-case ask
// (TestTheWorstCaseAskStillFitsWithItsQuestion) with an icon URL of
// MaxNativeIcon bytes still fits one record and keeps its whole question:
// the icon costs chips, never the question.
func TestTheWorstCaseAskStillFitsWithTheLongestIcon(t *testing.T) {
	lt := func(n int) string { return strings.Repeat("<", n) }
	worst := Payload{Type: "attention", ID: "ask-123456789", Tag: "attention-ask-123456789", Title: lt(MaxNativeTitle),
		Body: lt(MaxNativeBody), Data: map[string]string{"url": "/attention/open/ask-123456789"}, Update: true,
		Ask:  &V2Ask{ID: 123456789, Options: []string{lt(MaxNativeChip)}, Suggestions: []string{lt(MaxNativeChip)}},
		Icon: "https://" + strings.Repeat("a", MaxNativeIcon-8), IconETag: "0123456789abcdef"}
	b, err := nativeBody(worst)
	if err != nil {
		t.Fatalf("does not fit: %v", err)
	}
	var v PayloadV2
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Title != worst.Title || v.Body != worst.Body || v.Icon != worst.Icon || len(b) > NativeMaxPlaintext {
		t.Fatalf("%d bytes; title/body/icon kept: %v %v %v", len(b), v.Title == worst.Title, v.Body == worst.Body, v.Icon == worst.Icon)
	}
}
