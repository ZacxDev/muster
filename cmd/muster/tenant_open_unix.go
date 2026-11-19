//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// openTenantFile opens a tenant file without following a symlink at its last
// component (O_NOFOLLOW), so the checks readTenantFile makes on the opened
// descriptor are about the file it reads. symlink reports that refusal.
func openTenantFile(path string) (f *os.File, symlink bool, err error) {
	f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	return f, errors.Is(err, syscall.ELOOP), err
}
