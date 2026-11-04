package podagent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
)

// TESTS-FIRST STUB: the API the tests compile against, with none of the
// behaviour. The implementation commit replaces this file.

// AskImagePath is muster's route an officer instance uploads an ask image to.
const AskImagePath = "/agent/officer/ask-images"

// ErrOutsideWorkspace is a path that resolves outside the workspace root.
var ErrOutsideWorkspace = errors.New("the path is outside your workspace")

var errTooLarge = errors.New("stub: too large")

// AskImageUpload is HQ's record of the uploaded image.
type AskImageUpload struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// ResolveWorkspaceFile is a stub: it confines nothing.
func ResolveWorkspaceFile(root, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return p, nil
}

// capReader is a stub: it caps nothing.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) { return c.r.Read(p) }

// UploadAskImage is a stub: it uploads nothing.
func UploadAskImage(ctx context.Context, client *http.Client, base, token, root, p string) (AskImageUpload, error) {
	return AskImageUpload{}, errors.New("not implemented")
}
