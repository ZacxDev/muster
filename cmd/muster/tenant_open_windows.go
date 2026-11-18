//go:build windows

package main

import (
	"errors"
	"os"
)

// 🔴 TENANT FILES ARE REFUSED ON WINDOWS, NOT READ WITH WEAKER CHECKS. A tenant
// is a per-tenant muster-mirror on a multi-tenant host, and muster-mirror runs
// only where tmux does (Linux, macOS). Windows has no O_NOFOLLOW, and Go reports
// every Windows file as mode 0666, so the "not readable by group or others"
// check readTenantFile makes could not hold there: reading the file anyway would
// be a check that cannot fail. On Windows the CLI reaches a server with
// --api-url / MUSTER_API_URL instead.
var errTenantFilesUnsupported = errors.New("tenant files are not supported on Windows (they belong to a muster-mirror host, which needs tmux: Linux or macOS)")

func openTenantFile(string) (*os.File, bool, error) {
	return nil, false, errTenantFilesUnsupported
}
