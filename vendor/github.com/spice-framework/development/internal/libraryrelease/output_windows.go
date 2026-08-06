//go:build windows

package libraryrelease

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// renameNoReplace invokes MoveFileExW without MOVEFILE_REPLACE_EXISTING. Both
// paths share a parent, so Windows performs one same-volume directory move.
func renameNoReplace(staging string, output string) error {
	stagingPointer, err := syscall.UTF16PtrFromString(staging)
	if err != nil {
		return err
	}
	outputPointer, err := syscall.UTF16PtrFromString(output)
	if err != nil {
		return err
	}
	moveFileEx := syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")
	// #nosec G103 -- the pointers remain live for the fixed MoveFileExW call.
	success, _, callErr := moveFileEx.Call(
		uintptr(unsafe.Pointer(stagingPointer)),
		uintptr(unsafe.Pointer(outputPointer)),
		0,
	)
	runtime.KeepAlive(stagingPointer)
	runtime.KeepAlive(outputPointer)
	if success == 0 {
		if errors.Is(callErr, syscall.Errno(0)) {
			callErr = syscall.EINVAL
		}
		return &os.LinkError{
			Op: "rename-noreplace", Old: staging, New: output, Err: callErr,
		}
	}
	return nil
}
