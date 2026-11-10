package branding

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"testing"
)

// The fixtures are built here, from pixels whose values are pairwise distinct
// (x, y and a constant blue all differ), except the two WebP files, which no
// Go encoder can write: testdata/logo-40x20{,-lossy}.webp are the same 40×20
// gradient, written by libwebp's cwebp (-lossless, and -q 80).

func gradient(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 6), uint8(y * 12), 200, 255})
		}
	}
	return img
}

func pngOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// chunk is one PNG chunk: length, type, data, CRC.
func chunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return b.Bytes()
}

// headerOnlyPNG is a PNG signature, an IHDR declaring w×h (8-bit RGBA), and
// IEND: a valid header over no pixels — the shape of a decompression bomb as
// DecodeConfig sees it.
func headerOnlyPNG(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // bit depth 8, colour type RGBA
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, chunk("IHDR", ihdr)...)
	return append(out, chunk("IEND", nil)...)
}

// withTextChunk inserts a tEXt chunk right after IHDR (offset 8 + 25).
func withTextChunk(p []byte, key, val string) []byte {
	at := 8 + 25
	out := append([]byte{}, p[:at]...)
	out = append(out, chunk("tEXt", append(append([]byte(key), 0), val...))...)
	return append(out, p[at:]...)
}

// withExif inserts an APP1 Exif segment right after a JPEG's SOI marker.
func withExif(j []byte, payload string) []byte {
	seg := append([]byte("Exif\x00\x00"), payload...)
	var b bytes.Buffer
	b.Write(j[:2])
	b.Write([]byte{0xff, 0xe1})
	_ = binary.Write(&b, binary.BigEndian, uint16(len(seg)+2))
	b.Write(seg)
	b.Write(j[2:])
	return b.Bytes()
}

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("output is not a PNG: % x", b[:min(8, len(b))])
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("output does not decode as a PNG: %v", err)
	}
	return img
}

// TestProcessAcceptsEachFormat: a PNG, a JPEG, and both kinds of WebP (VP8L
// lossless, VP8 lossy) each come back as three PNGs — the logo at the
// upload's own size (40×20, under LogoMaxSide) and the two icons at exactly
// 192×192 and 512×512 — with a 16-hex-digit ETag over all three.
func TestProcessAcceptsEachFormat(t *testing.T) {
	src := gradient(40, 20)
	for name, data := range map[string][]byte{
		"png":          pngOf(t, src),
		"jpeg":         jpegOf(t, src),
		"webp":         readFixture(t, "logo-40x20.webp"),
		"webp (lossy)": readFixture(t, "logo-40x20-lossy.webp"),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Process(data)
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if b := decodePNG(t, out.Logo).Bounds(); b.Dx() != 40 || b.Dy() != 20 {
				t.Errorf("logo is %dx%d, want 40x20", b.Dx(), b.Dy())
			}
			if b := decodePNG(t, out.Icon192).Bounds(); b.Dx() != 192 || b.Dy() != 192 {
				t.Errorf("icon-192 is %dx%d", b.Dx(), b.Dy())
			}
			if b := decodePNG(t, out.Icon512).Bounds(); b.Dx() != 512 || b.Dy() != 512 {
				t.Errorf("icon-512 is %dx%d", b.Dx(), b.Dy())
			}
			if len(out.ETag) != 16 || strings.Trim(out.ETag, "0123456789abcdef") != "" {
				t.Errorf("etag %q is not 16 lowercase hex digits", out.ETag)
			}
			if out.ETag != ETagOf(out.Logo, out.Icon192, out.Icon512) {
				t.Error("etag is not ETagOf the three images")
			}
		})
	}
}

// TestProcessKeepsTheAspectInTheIcons: a 40×20 logo is letterboxed — centred,
// full width, transparent bands above and below — and a 20×40 one
// pillarboxed; nothing is cropped. The pixel values are the source's own, so
// a mutant that stretched or cropped would show here.
func TestProcessKeepsTheAspectInTheIcons(t *testing.T) {
	alpha := func(img image.Image, x, y int) uint32 { _, _, _, a := img.At(x, y).RGBA(); return a }
	wide, err := Process(pngOf(t, gradient(40, 20)))
	if err != nil {
		t.Fatal(err)
	}
	icon := decodePNG(t, wide.Icon192)
	// 40×20 into 192: 192×96, at y 48..143.
	if alpha(icon, 96, 10) != 0 || alpha(icon, 96, 181) != 0 {
		t.Error("wide logo: the bands above and below are not transparent")
	}
	if alpha(icon, 96, 96) == 0 || alpha(icon, 1, 96) == 0 || alpha(icon, 190, 96) == 0 {
		t.Error("wide logo: the middle row is not opaque edge to edge")
	}
	tall, err := Process(pngOf(t, gradient(20, 40)))
	if err != nil {
		t.Fatal(err)
	}
	icon = decodePNG(t, tall.Icon512)
	if alpha(icon, 10, 256) != 0 || alpha(icon, 501, 256) != 0 {
		t.Error("tall logo: the bands left and right are not transparent")
	}
	if alpha(icon, 256, 1) == 0 || alpha(icon, 256, 510) == 0 {
		t.Error("tall logo: the middle column is not opaque top to bottom")
	}
}

// TestProcessScalesALargeLogoDownOnly: a 2000×1000 upload is stored at
// 1024×512 (LogoMaxSide on the longer side, aspect kept); a 40×20 one is not
// scaled up (TestProcessAcceptsEachFormat).
func TestProcessScalesALargeLogoDownOnly(t *testing.T) {
	out, err := Process(pngOf(t, image.NewGray(image.Rect(0, 0, 2000, 1000))))
	if err != nil {
		t.Fatal(err)
	}
	if b := decodePNG(t, out.Logo).Bounds(); b.Dx() != 1024 || b.Dy() != 512 {
		t.Errorf("logo is %dx%d, want 1024x512", b.Dx(), b.Dy())
	}
}

// TestProcessStripsMetadataAndTrailingBytes: what is stored carries none of
// the upload's bytes but its pixels — a PNG tEXt chunk, a JPEG's Exif
// segment, and HTML appended after a PNG's IEND (a PNG/HTML polyglot) are all
// absent from all three outputs. The control: each marker IS in the input.
func TestProcessStripsMetadataAndTrailingBytes(t *testing.T) {
	const marker = "MUSTER-SECRET-MARKER"
	src := gradient(40, 20)
	cases := map[string][]byte{
		"png tEXt":         withTextChunk(pngOf(t, src), "Comment", marker),
		"jpeg exif":        withExif(jpegOf(t, src), marker),
		"png+html trailer": append(pngOf(t, src), []byte("<html><script>alert('"+marker+"')</script></html>")...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if !bytes.Contains(data, []byte(marker)) {
				t.Fatal("control: the input does not carry the marker")
			}
			out, err := Process(data)
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			for which, b := range map[string][]byte{"logo": out.Logo, "icon-192": out.Icon192, "icon-512": out.Icon512} {
				if bytes.Contains(b, []byte(marker)) || bytes.Contains(b, []byte("tEXt")) || bytes.Contains(b, []byte("<script")) {
					t.Errorf("%s still carries the upload's metadata", which)
				}
			}
		})
	}
}

// TestProcessRefusesEachBadUpload: every refusal, each with its OWN error —
// so a guard that stopped running would surface as a different error, not as
// a pass. Each input is otherwise the most acceptable thing it can be: the
// oversized file is a real PNG, the SVG and HTML are short and well formed,
// the pixel bomb is within the side cap, so no EARLIER guard can be what
// refuses it.
func TestProcessRefusesEachBadUpload(t *testing.T) {
	var gifBuf bytes.Buffer
	pal := image.NewPaletted(image.Rect(0, 0, 4, 4), []color.Color{color.Black, color.White})
	if err := gif.Encode(&gifBuf, pal, nil); err != nil {
		t.Fatal(err)
	}
	good := pngOf(t, gradient(40, 20))
	big := withTextChunk(good, "pad", strings.Repeat("x", MaxUploadBytes))
	cases := []struct {
		name string
		data []byte
		want *Error
	}{
		{"empty", nil, ErrEmpty},
		{"over 1 MiB (a valid PNG padded with a text chunk)", big, ErrTooLarge},
		{"svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><script>alert(1)</script></svg>`), ErrSVG},
		{"svg without an xml declaration", []byte("\n  <SVG viewBox='0 0 1 1'></SVG>"), ErrSVG},
		{"gif", gifBuf.Bytes(), ErrUnsupported},
		{"html", []byte("<!doctype html><html><body><img src=x onerror=alert(1)></body></html>"), ErrUnsupported},
		{"html wrapping a png (polyglot led by markup)", append([]byte("<html><!--"), good...), ErrUnsupported},
		{"bmp", append([]byte("BM"), make([]byte, 64)...), ErrUnsupported},
		{"png truncated inside its header", good[:20], ErrCorrupt},
		{"png truncated inside its pixels", good[:len(good)-20], ErrCorrupt},
		{"jpeg truncated", jpegOf(t, gradient(40, 20))[:200], ErrCorrupt},
		{"webp truncated", readFixture(t, "logo-40x20-lossy.webp")[:40], ErrCorrupt},
		{"wider than 4096", headerOnlyPNG(4097, 10), ErrDimensions},
		{"taller than 4096", headerOnlyPNG(10, 4097), ErrDimensions},
		{"zero width", headerOnlyPNG(0, 10), ErrCorrupt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Process(c.data)
			if !errors.Is(err, c.want) {
				t.Fatalf("Process = %v, want %s (%v)", err, c.want.Code, c.want)
			}
		})
	}
	if len(big) <= MaxUploadBytes || len(big) > MaxUploadBytes+1024 {
		t.Fatalf("control: the oversized fixture is %d bytes; want just over %d", len(big), MaxUploadBytes)
	}
}

// TestProcessRefusesAPixelBombWithinTheSideCap: a REAL decompression bomb —
// a 4096×3907 grayscale PNG, every side within MaxSide, 16,003,072 pixels,
// compressing to a few kilobytes — is refused for its pixel count before it
// is decoded. And the header-only shape of the same declaration is refused
// the same way, which is what proves the check runs on the header: there are
// no pixels in it to decode.
func TestProcessRefusesAPixelBombWithinTheSideCap(t *testing.T) {
	bomb := pngOf(t, image.NewGray(image.Rect(0, 0, 4096, 3907)))
	if len(bomb) > MaxUploadBytes {
		t.Fatalf("control: the bomb is %d bytes, over the upload cap, so the size guard would refuse it first", len(bomb))
	}
	for name, data := range map[string][]byte{"real": bomb, "header only": headerOnlyPNG(4096, 3907)} {
		if _, err := Process(data); !errors.Is(err, ErrPixels) {
			t.Errorf("%s: Process = %v, want %s", name, err, ErrPixels.Code)
		}
	}
	// The boundary: exactly MaxPixels is accepted as far as the pixel guard is
	// concerned (header only, so it then fails as corrupt — not as a bomb).
	if _, err := Process(headerOnlyPNG(4000, 4000)); !errors.Is(err, ErrCorrupt) {
		t.Errorf("4000x4000 (exactly MaxPixels): Process = %v, want corrupt (past the pixel guard)", err)
	}
}

// TestUnsupportedNamesWhatItSaw: the unsupported refusal tells the uploader
// what the bytes looked like, and still matches ErrUnsupported.
func TestUnsupportedNamesWhatItSaw(t *testing.T) {
	_, err := Process([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"))
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "image/gif") {
		t.Fatalf("err = %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != "unsupported_type" {
		t.Fatalf("errors.As = %v", e)
	}
}
