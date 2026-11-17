package podagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZacxDev/muster/internal/askimage"
	"github.com/ZacxDev/muster/internal/workspacefile"
)

// --- an officer instance's workspace file, uploaded as an ask image ----------
//
// muster-server cannot read an instance's workspace, so the instance's own pod
// sends the file: `ccd ask-image <path>` streams it to muster's
// POST AskImagePath with the agent's own token (X-Muster-Token), muster
// forwards it to HQ with the officer credential, and the HQ attachment id comes
// back for officer_ask_person's image_ids.
//
// 🔴 THE PATH IS CONFINED HERE, IN THE POD, because only the pod can see the
// filesystem: the path is resolved against the workspace root (a relative one
// joined to it, an absolute one taken as is), symlinks are resolved
// (filepath.EvalSymlinks, the root's own included), and the result must lie
// inside the resolved root. `..`, an absolute path elsewhere and a symlink out
// are all refused, before the file is opened. The bytes are the other half:
// muster-server sniffs and caps them again, whatever sent them.

// AskImagePath is muster's route an officer instance uploads an ask image to.
const AskImagePath = "/agent/officer/ask-images"

// ErrOutsideWorkspace is a path that resolves outside the workspace root
// (internal/workspacefile, the one rule).
var ErrOutsideWorkspace = workspacefile.ErrOutside

// ResolveWorkspaceFile is p resolved inside root (workspacefile.Resolve): p
// relative is joined to root; every symlink on either is resolved; the result
// must be root itself's descendant (ErrOutsideWorkspace otherwise) and a
// regular file.
func ResolveWorkspaceFile(root, p string) (string, error) { return workspacefile.Resolve(root, p) }

// errTooLarge is an image over askimage.MaxBytes, found while streaming it.
var errTooLarge = fmt.Errorf("the image is over %d MiB", askimage.MaxBytes>>20)

// capReader passes at most left bytes and fails on the next one: the cap is
// held WHILE the file streams, so a file that grows after its Stat cannot
// send more.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, errTooLarge
	}
	return n, err
}

// UploadAskImage sends workspace file p (ResolveWorkspaceFile against root) to
// muster at base as an ask image, with the agent's own token, and returns
// HQ's record of it. The type is sniffed from the file's leading bytes and
// must be one of askimage.Types; the size is capped at askimage.MaxBytes,
// before the upload (Stat) and while it streams. client nil is NoRedirects.
func UploadAskImage(ctx context.Context, client *http.Client, base, token, root, p string) (askimage.Record, error) {
	if client == nil {
		client = NoRedirects
	}
	if base == "" || token == "" {
		return askimage.Record{}, fmt.Errorf("%s and %s must be set (this pod reaches muster with them)", EnvConnectURL, EnvAgentToken)
	}
	real, err := ResolveWorkspaceFile(root, p)
	if err != nil {
		return askimage.Record{}, err
	}
	f, err := os.Open(real)
	if err != nil {
		return askimage.Record{}, fmt.Errorf("cannot read %s: %v", p, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return askimage.Record{}, fmt.Errorf("cannot read %s: %v", p, err)
	}
	if st.Size() > askimage.MaxBytes {
		return askimage.Record{}, errTooLarge
	}
	head := make([]byte, askimage.SniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return askimage.Record{}, fmt.Errorf("cannot read %s: %v", p, err)
	}
	head = head[:n]
	ct, ok := askimage.Sniff(head)
	if !ok {
		return askimage.Record{}, fmt.Errorf("%s is %s, not a PNG, JPEG or WebP image", p, ct)
	}
	body := &capReader{r: io.MultiReader(bytes.NewReader(head), f), left: askimage.MaxBytes}
	u := strings.TrimRight(base, "/") + AskImagePath + "?filename=" + url.QueryEscape(filepath.Base(real))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return askimage.Record{}, err
	}
	req.ContentLength = st.Size()
	req.Header.Set("X-Muster-Token", token)
	req.Header.Set("Content-Type", ct)
	resp, err := client.Do(req)
	if errors.Is(err, errTooLarge) {
		return askimage.Record{}, errTooLarge
	}
	if err != nil {
		return askimage.Record{}, fmt.Errorf("reaching muster: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return askimage.Record{}, fmt.Errorf("muster refused the image (%d): %s", resp.StatusCode, e.Error)
		}
		return askimage.Record{}, fmt.Errorf("muster refused the image (%d)", resp.StatusCode)
	}
	var up askimage.Record
	if err := json.Unmarshal(raw, &up); err != nil || up.ID == "" {
		return askimage.Record{}, errors.New("muster's answer carries no attachment id")
	}
	return up, nil
}
