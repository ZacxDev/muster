//go:build unix

package clankmailplugin

import (
	"errors"
	"os"
	"syscall"
)

// lockDir takes an exclusive, non-blocking flock on path for the life of the
// returned file. The kernel drops it when the process dies, so a crash leaves
// no stale lock behind.
func lockDir(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another muster-clankmail holds this STATE_DIR (state.lock): one writer only")
	}
	return f, nil
}
