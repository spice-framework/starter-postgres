//go:build linux

package libraryrelease

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const linuxRenameNoReplace = 1

// renameNoReplace uses the kernel's atomic RENAME_NOREPLACE operation. There
// is deliberately no os.Rename fallback because it may replace an empty output
// directory after the caller's absence check.
func renameNoReplace(staging string, output string) error {
	trap, err := linuxRenameat2Trap()
	if err != nil {
		return err
	}
	stagingPointer, err := syscall.BytePtrFromString(staging)
	if err != nil {
		return err
	}
	outputPointer, err := syscall.BytePtrFromString(output)
	if err != nil {
		return err
	}
	// #nosec G103 -- the pointers remain live for the fixed renameat2 syscall.
	_, _, errno := syscall.Syscall6(
		trap,
		^uintptr(99), // AT_FDCWD (-100).
		uintptr(unsafe.Pointer(stagingPointer)),
		^uintptr(99), // AT_FDCWD (-100).
		uintptr(unsafe.Pointer(outputPointer)),
		linuxRenameNoReplace,
		0,
	)
	runtime.KeepAlive(stagingPointer)
	runtime.KeepAlive(outputPointer)
	if errno != 0 {
		return &os.LinkError{Op: "rename-noreplace", Old: staging, New: output, Err: errno}
	}
	return nil
}

func linuxRenameat2Trap() (uintptr, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 316, nil
	case "arm64":
		return 276, nil
	default:
		return 0, fmt.Errorf(
			"atomic no-replace release commit is unsupported on linux/%s",
			runtime.GOARCH,
		)
	}
}
