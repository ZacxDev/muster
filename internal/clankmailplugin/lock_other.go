//go:build !unix

package clankmailplugin

import (
	"errors"
	"os"
)

// lockDir: the single-writer lock needs flock; the plugin runs on Linux.
func lockDir(path string) (*os.File, error) {
	return nil, errors.New("muster-clankmail needs a unix system (flock) for its state lock")
}
