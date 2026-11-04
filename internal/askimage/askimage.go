// Package askimage is THE rule for what an officer's ask image may be: the
// types HQ accepts (sniffed from the bytes, never taken from a name or a
// header) and its size cap. It is a leaf package so that both sides of an
// upload use the one rule: muster-server (internal/officers, internal/api)
// and the instance's own pod (internal/podagent, `ccd ask-image`).
package askimage

import "net/http"

// MaxBytes is HQ's bound on one ask image: 5 MiB.
const MaxBytes = 5 << 20

// Types are the image types HQ accepts, as http.DetectContentType names them.
var Types = []string{"image/png", "image/jpeg", "image/webp"}

// SniffLen is how many leading bytes Sniff needs (http.DetectContentType's).
const SniffLen = 512

// Sniff is the content type of data's leading bytes and whether it is one of
// Types.
func Sniff(head []byte) (string, bool) {
	ct := http.DetectContentType(head)
	for _, t := range Types {
		if t == ct {
			return ct, true
		}
	}
	return ct, false
}

// Record is HQ's record of one uploaded ask image, as HQ answers
// POST /officers/{id}/ask-images and as muster's upload route passes it on.
type Record struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}
