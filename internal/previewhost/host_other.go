//go:build !linux

package previewhost

import (
	"errors"
	"runtime"
)

// Supported says why listener reports and the foreign_listener check are off
// here: they read Linux's /proc. On macOS `lsof` would be the source; that is
// UNVERIFIED, so both are off and the host says so, rather than offering a
// check it does not make.
func Supported() error {
	return errors.New("listener reports and the foreign_listener check are UNVERIFIED on " + runtime.GOOS + " and off (they read Linux's /proc)")
}
