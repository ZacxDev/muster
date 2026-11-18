//go:build linux

package previewhost

// Supported is nil on Linux: /proc/net/tcp{,6} and /proc/<pid>/fd are where
// the listener reports and the foreign_listener check read.
func Supported() error { return nil }
