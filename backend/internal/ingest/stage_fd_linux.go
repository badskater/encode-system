//go:build linux

package ingest

import (
	"fmt"
	"os"
	"strconv"
)

// openedFilePath resolves the final path of an ALREADY-OPENED file via
// /proc/self/fd (Linux). This is the post-open containment anchor (Q2):
// the readlink reflects the inode the handle actually holds, so a path
// swap after the pre-open check cannot redirect the copy. A file deleted
// after open carries a " (deleted)" suffix and fails the containment
// prefix check — refused, which is the conservative outcome.
func openedFilePath(f *os.File) (string, error) {
	p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		return "", fmt.Errorf("resolve opened file: %w", err)
	}
	return p, nil
}
