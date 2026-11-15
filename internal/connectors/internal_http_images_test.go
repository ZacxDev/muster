package connectors_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// Image fixtures: the format signatures http.DetectContentType sniffs, then
// filler bytes. Each is distinct, so a mix-up between two is visible.
var (
	ihPNG  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1, 2, 3, 0xfe}, 300)...)
	ihJPEG = append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{9, 8, 7}, 211)...)
	b64    = base64.StdEncoding.EncodeToString
)

// TestSplitImagesReturnsAScreenshotAsAnImage: a JSON response whose top-level
// object has `*_base64` fields that decode to images gives those images back
// (in key order) with the rest of the object as text; a binary image body
// gives its image. Every other shape — a `*_base64` field that is not an
// image, an image under another key or nested, a non-JSON text, a non-image
// binary — is returned unchanged with no image.
func TestSplitImagesReturnsAScreenshotAsAnImage(t *testing.T) {
	jsonRes := func(body string) connectors.InternalHTTPResult {
		return connectors.InternalHTTPResult{Status: 200, ContentType: "application/json; charset=utf-8", Body: body, Bytes: len(body)}
	}

	// The screenshot shape: png_base64 beside ordinary fields.
	res, imgs := jsonRes(`{"png_base64":"` + b64(ihPNG) + `","width":1280,"url":"/v1/s/abc"}`).SplitImages()
	if len(imgs) != 1 || imgs[0].MIMEType != "image/png" || !bytes.Equal(imgs[0].Data, ihPNG) || res.Images != 1 {
		t.Fatalf("png_base64: %d images %+v, Images=%d", len(imgs), imgs, res.Images)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(res.Body), &obj); err != nil {
		t.Fatalf("the remaining body is not JSON: %v: %s", err, res.Body)
	}
	if obj["width"] != float64(1280) || obj["url"] != "/v1/s/abc" ||
		obj["png_base64"] != "[image 1 of this result: image/png, 1208 bytes, returned as an image]" {
		t.Fatalf("the remaining body: %s", res.Body)
	}
	if strings.Contains(res.Body, b64(ihPNG)[:40]) {
		t.Fatalf("the base64 is still in the text: %s", res.Body)
	}

	// Two images, a jpeg too: in key order, each its own type.
	_, imgs = jsonRes(`{"b_base64":"` + b64(ihJPEG) + `","a_base64":"` + b64(ihPNG) + `"}`).SplitImages()
	if len(imgs) != 2 || imgs[0].MIMEType != "image/png" || imgs[1].MIMEType != "image/jpeg" ||
		!bytes.Equal(imgs[1].Data, ihJPEG) {
		t.Fatalf("two fields: %+v", imgs)
	}

	// A binary image body.
	bin := connectors.InternalHTTPResult{Status: 200, ContentType: "image/png", BodyBase64: b64(ihPNG), Bytes: len(ihPNG)}
	res, imgs = bin.SplitImages()
	if len(imgs) != 1 || imgs[0].MIMEType != "image/png" || res.BodyBase64 != "" || res.Images != 1 {
		t.Fatalf("binary png: %+v %+v", res, imgs)
	}

	for name, in := range map[string]connectors.InternalHTTPResult{
		"a *_base64 field that is not an image": jsonRes(`{"data_base64":"` + b64([]byte("plain text, not an image at all")) + `"}`),
		"a *_base64 field that is not base64":   jsonRes(`{"png_base64":"not base64!"}`),
		"an image under another key":            jsonRes(`{"screenshot":"` + b64(ihPNG) + `"}`),
		"a nested image":                        jsonRes(`{"result":{"png_base64":"` + b64(ihPNG) + `"}}`),
		"a top-level array":                     jsonRes(`["` + b64(ihPNG) + `"]`),
		"a non-JSON text body": {Status: 200, ContentType: "text/plain",
			Body: `{"png_base64":"` + b64(ihPNG) + `"}`},
		"a non-image binary": {Status: 200, ContentType: "application/octet-stream",
			BodyBase64: b64([]byte("PK\x03\x04 a zip, not an image"))},
	} {
		out, imgs := in.SplitImages()
		if len(imgs) != 0 || out != in {
			t.Errorf("%s: %d images, result changed %v, want it unchanged", name, len(imgs), out != in)
		}
	}
}
