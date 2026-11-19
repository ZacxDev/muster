//go:build windows

package main

import (
	"errors"
	"os"
)

// 🔴 TENANT FILES ARE REFUSED ON WINDOWS, NOT READ WITH WEAKER CHECKS. A tenant
// is client-side: one CLI, several muster servers, each an env file holding
// its URL and credential (~/.muster/tenants/<name>.env, tenant.go). Unix reads
// one only through O_NOFOLLOW and only when no group or other can read it.
// Windows has no O_NOFOLLOW, and Go reports every Windows file as mode 0666 or
// 0444, so neither check could hold there: reading the file anyway would be a
// credential check that cannot fail. The default ~/.muster/muster.env does not
// come through here and works on Windows; docs/cli.md says tenants do not.
var errTenantFilesUnsupported = errors.New("tenant files are not supported on Windows: use ~/.muster/muster.env, or --api-url / MUSTER_API_URL (docs/cli.md, \"Several servers (tenants)\")")

func openTenantFile(string) (*os.File, bool, error) {
	return nil, false, errTenantFilesUnsupported
}
