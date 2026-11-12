//go:build windows

package ingest

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32DLL                   = syscall.NewLazyDLL("kernel32.dll")
	procGetFinalPathNameByHandleW = kernel32DLL.NewProc("GetFinalPathNameByHandleW")
)

// openedFilePath resolves the final path of an ALREADY-OPENED file via
// GetFinalPathNameByHandleW (the Windows equivalent of the Linux
// /proc/self/fd readlink). This is the post-open containment anchor
// (Q2): the handle's final path reflects the inode actually held, so a
// path swap after the pre-open check cannot redirect the copy.
func openedFilePath(f *os.File) (string, error) {
	buf := make([]uint16, 32768)
	n, _, callErr := procGetFinalPathNameByHandleW.Call(
		f.Fd(), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	if n == 0 {
		return "", fmt.Errorf("GetFinalPathNameByHandle: %w", callErr)
	}
	if n > uintptr(len(buf)) {
		return "", fmt.Errorf("GetFinalPathNameByHandle: path too long (%d chars)", n)
	}
	p := syscall.UTF16ToString(buf[:n])
	// Normalize the device-path prefix the API returns:
	//   \\?\C:\dir\file      → C:\dir\file
	//   \\?\UNC\server\share → \\server\share
	if strings.HasPrefix(p, `\\?\UNC\`) {
		p = `\\` + p[len(`\\?\UNC\`):]
	} else if strings.HasPrefix(p, `\\?\`) {
		p = p[len(`\\?\`):]
	}
	return p, nil
}
