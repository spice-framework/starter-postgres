//go:build darwin

package libraryrelease

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	darwinATFDCWD     = ^uintptr(1) // AT_FDCWD (-2) on Darwin.
	darwinRenameatxNP = 488
	darwinRenameExcl  = 0x4
)

// renameNoReplace uses Darwin's atomic renameatx_np(RENAME_EXCL) operation.
// Unsupported kernels fail the release rather than fall back to replacement.
func renameNoReplace(staging string, output string) error {
	stagingPointer, err := syscall.BytePtrFromString(staging)
	if err != nil {
		return err
	}
	outputPointer, err := syscall.BytePtrFromString(output)
	if err != nil {
		return err
	}
	// #nosec G103 -- the pointers remain live for the fixed renameatx_np syscall.
	_, _, errno := syscall.Syscall6(
		darwinRenameatxNP,
		darwinATFDCWD,
		uintptr(unsafe.Pointer(stagingPointer)),
		darwinATFDCWD,
		uintptr(unsafe.Pointer(outputPointer)),
		darwinRenameExcl,
		0,
	)
	runtime.KeepAlive(stagingPointer)
	runtime.KeepAlive(outputPointer)
	if errno != 0 {
		return &os.LinkError{Op: "rename-noreplace", Old: staging, New: output, Err: errno}
	}
	return nil
}
