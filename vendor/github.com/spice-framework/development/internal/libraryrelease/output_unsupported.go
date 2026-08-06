//go:build !linux && !darwin && !windows

package libraryrelease

import (
	"fmt"
	"runtime"
)

func renameNoReplace(string, string) error {
	return fmt.Errorf(
		"atomic no-replace release commit is unsupported on %s/%s",
		runtime.GOOS,
		runtime.GOARCH,
	)
}
