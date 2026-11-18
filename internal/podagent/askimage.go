package podagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/ZacxDev/muster/internal/askattach"
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
//
// The upload itself is the ONE workspace-upload client (askattach.Send), which
// an ask's attachment (`ccd ask-attach`) uses too: only the route, the cap and
// the type rule are this file's.

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

// errTooLarge is an image over askimage.MaxBytes.
var errTooLarge = fmt.Errorf("the image is over %d MiB", askimage.MaxBytes>>20)

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
	raw, err := askattach.Send(ctx, client, askattach.Spec{
		Target:   askattach.Target{Base: base, Path: AskImagePath, Header: http.Header{"X-Muster-Token": {token}}},
		MaxBytes: askimage.MaxBytes, Noun: "the image",
		TooLarge: func(string) error { return errTooLarge },
		Type: func(p string, _ int64, head []byte) (string, error) {
			ct, ok := askimage.Sniff(head)
			if !ok {
				return "", fmt.Errorf("%s is %s, not a PNG, JPEG or WebP image", p, ct)
			}
			return ct, nil
		}}, root, p)
	if err != nil {
		return askimage.Record{}, err
	}
	var up askimage.Record
	if err := json.Unmarshal(raw, &up); err != nil || up.ID == "" {
		return askimage.Record{}, errors.New("muster's answer carries no attachment id")
	}
	return up, nil
}
