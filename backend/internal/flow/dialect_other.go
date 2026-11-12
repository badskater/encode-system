//go:build !linux

package flow

// DialectForPlatform on non-Linux builds always returns PowerShellDialect.
// Bash rendering for Linux nodes requires the Linux-built controller: the
// bash built-in step sources are Linux-only (see dialect_bash.go), and the
// only non-Linux binary this repo ships is the Windows agent, which executes
// rendered scripts and never imports flow. A non-Linux controller build is
// therefore not a supported way to serve Linux nodes; "linux" falls back to
// the historical fleet default here instead of referencing BashDialect.
func DialectForPlatform(platform string) Dialect {
	return PowerShellDialect{}
}
