//go:build !linux && !windows

package ingest

import (
	"fmt"
	"os"
)

// openedFilePath fails closed on platforms without a wired post-open
// final-path API (the controller ships Linux; the agent Windows).
// Staging refuses rather than trusting the pre-open containment check
// alone — the TOCTOU guard (Q2) must hold on every supported platform.
func openedFilePath(f *os.File) (string, error) {
	return "", fmt.Errorf("post-open path verification unsupported on this platform")
}
