//go:build !unix

package previewgw

import "errors"

// lockFile: the token file's lock needs flock; the gateway runs on Linux.
func lockFile(string, bool) (func(), error) {
	return nil, errors.New("muster-preview needs a unix system (flock) for its host-token file")
}
