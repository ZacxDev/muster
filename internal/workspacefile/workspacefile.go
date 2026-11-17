// Package workspacefile confines a path an agent names to the directory it
// works in: THE ONE rule every caller that reads an agent-named file off a
// filesystem muster-server cannot see applies — an officer pod's ask image
// (internal/podagent, `ccd ask-image`), a dispatched pod's ask attachment
// (`ccd ask-attach`) and a mirrored session's attention_ask attachments
// (internal/mirrorhost's MCP proxy, against the session's working directory).
//
// 🔴 THE PATH IS RESOLVED BEFORE IT IS OPENED. A relative path is joined to the
// root, an absolute one taken as is; every symlink on either is resolved
// (filepath.EvalSymlinks, the root's own included); the result must lie inside
// the resolved root and be a regular file. `..`, an absolute path elsewhere and
// a symlink out are all refused (ErrOutside).
package workspacefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutside is a path that resolves outside the root.
var ErrOutside = errors.New("the path is outside your workspace")

// Resolve is p resolved inside root, or an error: p relative is joined to
// root; every symlink on either is resolved; the result must be root itself's
// descendant (ErrOutside otherwise) and a regular file.
func Resolve(root, p string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("the workspace root %q is not an absolute path", root)
	}
	if strings.TrimSpace(p) == "" {
		return "", errors.New("no path given")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("the workspace root: %v", err)
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, full)
	}
	// Lexically first: a clear refusal for `..` and a foreign absolute path,
	// whether or not the target exists.
	if !Within(filepath.Clean(root), filepath.Clean(full)) && !Within(realRoot, filepath.Clean(full)) {
		return "", ErrOutside
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %v", p, err)
	}
	if !Within(realRoot, real) {
		return "", ErrOutside
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %v", p, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	return real, nil
}

// Within reports whether p is root or a descendant of it (both cleaned).
func Within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
