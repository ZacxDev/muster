// Package branding is the instance's logo and display name: what an admin
// uploads on the Account page (docs/branding.md), the images muster serves
// from /brand/, and the row that keeps them (migration 0105,
// instance_branding).
//
// This file is the image half: Process turns an upload into the three PNGs
// muster serves, or refuses it with one of the errors below.
//
// 🔴 THE FORMAT IS DECIDED BY THE BYTES, NEVER BY A NAME. The filename and the
// part's Content-Type are the uploader's claims and are not read at all. The
// first bytes must be a PNG, JPEG or WebP signature, and each is decoded by
// ITS OWN decoder (png, jpeg, webp) — never image.Decode, whose registry is
// whatever the binary happens to import, so a GIF or BMP decoder linked in by
// some other package would widen what is accepted without a line changing
// here.
//
// 🔴 THE SIZE IS CHECKED BEFORE ANYTHING IS DECODED. A 1 MiB file can declare
// a 65535×65535 image whose pixels compress to nothing; decoding it would ask
// for 16 GiB. So the header is read first (DecodeConfig, which allocates no
// pixels), and the width, height and pixel count are refused there.
//
// 🔴 WHAT IS STORED IS RE-ENCODED, NEVER THE UPLOAD. The decoded pixels are
// drawn onto a fresh canvas and written as a new PNG, so nothing of the file
// but its pixels survives: no EXIF, no text chunks, no ICC profile, no bytes
// appended after the image (a PNG/HTML polyglot loses its HTML).
package branding

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"
	"runtime/debug"
	"sync"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// The limits Process enforces.
//
// 🔴 MaxPixels IS THE MEMORY BOUND, AND IT IS SET BY MEASUREMENT. Decoding and
// scaling cost memory per pixel, whatever the file's size: at the old cap of
// 16,000,000 pixels a progressive CMYK JPEG of a few hundred KB peaked at
// 560-580 MB RSS, past the server's 512 MiB limit. At 2048×2048 (4,194,304)
// the worst accepted input measured ~119 MB peak RSS (the figures are on
// processMu). A logo is stored at most LogoMaxSide (1024) on a side, so this
// still takes a 2× source.
//
// ⚠ MaxSide STAYS ABOVE √MaxPixels SO THE PIXEL GUARD IS REACHABLE. Were the
// sides capped at 2048 too, no image could exceed MaxPixels and the guard
// would never run. At 4096 a wide 4096×1024 banner is accepted, and a
// 4096×1025 or 2049×2048 image passes the side check and is refused here
// (TestThePixelCapIs2048Squared).
const (
	// MaxUploadBytes is the largest upload accepted: 1 MiB.
	MaxUploadBytes = 1 << 20
	// MaxSide is the widest and tallest image accepted, in pixels.
	MaxSide = 4096
	// MaxPixels is the most pixels accepted (width × height): 2048×2048.
	MaxPixels = 2048 * 2048
	// LogoMaxSide is the longest side of the stored logo: a larger upload is
	// scaled down to fit (never up). The header draws it about 28px tall; this
	// keeps a crisp copy for high-density screens without storing megabytes.
	LogoMaxSide = 1024
	// Icon192 and Icon512 are the square icons' sides.
	Icon192 = 192
	Icon512 = 512
)

// Error is one reason an upload or a name is refused. Code is stable (the API
// returns it, and a client may branch on it); Msg is for a person.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// The refusals. Each is a distinct value, so a caller (and a test) can tell
// which guard refused: errors.Is(err, ErrSVG).
var (
	ErrEmpty       = &Error{"empty", "the file is empty"}
	ErrTooLarge    = &Error{"too_large", "the file is larger than 1 MiB"}
	ErrSVG         = &Error{"svg_refused", "SVG is not accepted: upload a PNG, JPEG or WebP"}
	ErrUnsupported = &Error{"unsupported_type", "the file is not a PNG, JPEG or WebP image"}
	ErrCorrupt     = &Error{"corrupt", "the image could not be read: it is truncated or damaged"}
	ErrDimensions  = &Error{"too_wide", "the image is wider or taller than 4096 pixels"}
	ErrPixels      = &Error{"too_many_pixels", "the image has more than 4,194,304 pixels (2048×2048)"}
)

// Images is an accepted upload as muster stores and serves it: the logo and
// two square icons, each a PNG, and the ETag that names all three.
type Images struct {
	Logo    []byte
	Icon192 []byte
	Icon512 []byte
	// ETag is the first 16 hex digits of the sha256 over the three PNGs. It
	// changes whenever any of them does, so it is the cache key everywhere:
	// the /brand/ responses' ETag, the ?v= on every link to them, and the
	// field the apps compare.
	ETag string
}

type format int

const (
	formatPNG format = iota + 1
	formatJPEG
	formatWebP
)

// processMu serialises Process: one upload at a time, so the figures below
// are the whole cost, not one of several. Uploads are an admin's rare action.
//
// Peak RSS of a fresh process running Process once on a 2048×2048 image
// (MaxPixels), default GOGC, no GOMEMLIMIT: progressive CMYK JPEG ~119 MB;
// 16-bit RGBA PNG, 8-bit RGBA PNG, baseline and progressive RGB JPEG, lossless
// and lossy WebP ~116 MB each. A 4096×1024 image: ~64 MB, ~83 MB for a
// progressive JPEG, ~119 MB for CMYK. (Measured by getrusage; the method and
// the before/after table are in the commit that set MaxPixels.)
//
// Two phases make up that peak, and they are kept apart:
//   - decoding: the pixels, plus a progressive JPEG's coefficients (4 bytes
//     per pixel per colour component: ~64 MB for a 2048² CMYK one), plus the
//     RGBA copy;
//   - scaling: the RGBA copy, plus the scaler's scratch buffer (32 bytes per
//     destination column per source row: 1024×2048×32 = 64 MB for the logo).
//
// Without a break between them the same 2048² inputs measured 119-187 MB
// (CMYK the worst); Process hands the first phase's memory back
// (debug.FreeOSMemory: a whole-process collection, which an admin's rare
// upload can afford) before the second begins, so the peak is roughly the
// larger phase, not their sum. The icons are scaled from the stored logo (at
// most LogoMaxSide), not the full-size copy, which halves their scratch
// buffers for a 2048² upload.
var processMu sync.Mutex

// Process validates data and returns the images to store, or one of the
// errors above. The checks run in this order, each before any work the next
// one guards: size, signature, header (DecodeConfig), sides, pixel count,
// decode, re-encode.
func Process(data []byte) (Images, error) {
	if len(data) == 0 {
		return Images{}, ErrEmpty
	}
	if len(data) > MaxUploadBytes {
		return Images{}, ErrTooLarge
	}
	f, err := sniff(data)
	if err != nil {
		return Images{}, err
	}
	cfg, err := decodeConfig(f, data)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return Images{}, ErrCorrupt
	}
	if cfg.Width > MaxSide || cfg.Height > MaxSide {
		return Images{}, ErrDimensions
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return Images{}, ErrPixels
	}

	processMu.Lock()
	defer processMu.Unlock()
	rgba, err := decodeRGBA(f, data, cfg)
	if err != nil {
		return Images{}, err
	}
	// The decoder's image and state are unreachable now; return their memory
	// before the scaler asks for its own (processMu's comment has the figures).
	debug.FreeOSMemory()
	logo := fit(rgba, LogoMaxSide)
	var out Images
	if out.Logo, err = encodePNG(logo); err != nil {
		return Images{}, err
	}
	// The icons are scaled from the logo, not from rgba: for an upload within
	// LogoMaxSide they are the same image, and for a larger one the scaler's
	// scratch buffer is sized by the logo's height, not the upload's.
	if out.Icon192, err = encodePNG(square(logo, Icon192)); err != nil {
		return Images{}, err
	}
	if out.Icon512, err = encodePNG(square(logo, Icon512)); err != nil {
		return Images{}, err
	}
	out.ETag = ETagOf(out.Logo, out.Icon192, out.Icon512)
	return out, nil
}

// decodeRGBA decodes data and copies its pixels onto a fresh RGBA canvas,
// which every output is then drawn from: the decoded pixels and nothing else
// of the file. (RGBA, not the decoder's own type, also because the scaler's
// fast paths take it: a JPEG's YCbCr scaled directly took ~8 s for a 40×20
// image under -race.) It is its own function so the decoder's image is out of
// scope, and collectable, once it returns.
func decodeRGBA(f format, data []byte, cfg image.Config) (*image.RGBA, error) {
	src, err := decode(f, data)
	if err != nil {
		return nil, ErrCorrupt
	}
	// A DEFENSIVE INVARIANT, NOT REGRESSION-COVERED: the standard decoders
	// return exactly the size their DecodeConfig reported, so no input
	// reaches this branch and no test exercises it. It is kept because a
	// decoder whose pixels disagreed with its header would not have been
	// bounded by Process's size checks, and the canvas below would be sized
	// by the header, not the pixels.
	if b := src.Bounds(); b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		return nil, ErrCorrupt
	}
	rgba := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	draw.Draw(rgba, rgba.Bounds(), src, src.Bounds().Min, draw.Src)
	return rgba, nil
}

// ETagOf is the ETag of a stored logo and icons (Images.ETag).
func ETagOf(logo, icon192, icon512 []byte) string {
	h := sha256.New()
	for _, b := range [][]byte{logo, icon192, icon512} {
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// sniff names data's format by its signature, or refuses it.
func sniff(data []byte) (format, error) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return formatPNG, nil
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return formatJPEG, nil
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return formatWebP, nil
	case looksLikeSVG(data):
		return 0, ErrSVG
	}
	return 0, &Error{ErrUnsupported.Code, ErrUnsupported.Msg + " (it looks like " + http.DetectContentType(data) + ")"}
}

// looksLikeSVG reports whether data's head names an <svg> element. It only
// picks the message: SVG is refused by sniff's allowlist either way (no
// signature above matches markup), and this tells the uploader why.
func looksLikeSVG(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.Contains(bytes.ToLower(head), []byte("<svg"))
}

// Is lets errors.Is match an *Error by its Code, so the unsupported-type
// refusal (which carries the sniffed type in its message) still matches
// ErrUnsupported.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

func decodeConfig(f format, data []byte) (image.Config, error) {
	r := bytes.NewReader(data)
	switch f {
	case formatPNG:
		return png.DecodeConfig(r)
	case formatJPEG:
		return jpeg.DecodeConfig(r)
	default:
		return webp.DecodeConfig(r)
	}
}

func decode(f format, data []byte) (image.Image, error) {
	r := bytes.NewReader(data)
	switch f {
	case formatPNG:
		return png.Decode(r)
	case formatJPEG:
		return jpeg.Decode(r)
	default:
		return webp.Decode(r)
	}
}

// fit is src, scaled down (keeping its aspect) so its longer side is at most
// max. It is never scaled up: a small enough src is returned as it is (it is
// already Process's fresh canvas).
func fit(src *image.RGBA, max int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		return src
	}
	w, h = scaled(w, h, max)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
	return dst
}

// square is src scaled (up or down, keeping its aspect) to fit a side×side
// transparent canvas, centred: a wide logo is letterboxed, a tall one
// pillarboxed, and nothing is cropped.
func square(src *image.RGBA, side int) *image.RGBA {
	b := src.Bounds()
	w, h := scaled(b.Dx(), b.Dy(), side)
	dst := image.NewRGBA(image.Rect(0, 0, side, side))
	x, y := (side-w)/2, (side-h)/2
	xdraw.CatmullRom.Scale(dst, image.Rect(x, y, x+w, y+h), src, b, xdraw.Src, nil)
	return dst
}

// scaled is w×h scaled so its longer side is max, keeping the aspect; neither
// side is less than 1.
func scaled(w, h, max int) (int, int) {
	if w >= h {
		nh := h * max / w
		if nh < 1 {
			nh = 1
		}
		return max, nh
	}
	nw := w * max / h
	if nw < 1 {
		nw = 1
	}
	return nw, max
}

func encodePNG(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
