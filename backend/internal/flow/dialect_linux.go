//go:build linux

package flow

import "github.com/badskater/encode-system/backend/internal/model"

// DialectForPlatform selects the script dialect for a node's reported platform.
// "linux" → BashDialect; anything else (including "" for legacy rows and old
// agents that never reported a platform) → PowerShellDialect, the historical
// fleet default. The controller calls this at dispatch time so a mixed
// Windows/Linux farm renders the correct script per node.
func DialectForPlatform(platform string) Dialect {
	if platform == model.PlatformLinux {
		return BashDialect{}
	}
	return PowerShellDialect{}
}
