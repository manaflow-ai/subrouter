//go:build !darwin && !linux && !windows

package fsutil

import (
	"errors"
	"runtime"
)

// RenameNoReplace fails closed on platforms without a supported atomic
// no-replace rename primitive.
func RenameNoReplace(_, _ string) error {
	return errors.New("atomic no-replace rename is unsupported on " + runtime.GOOS)
}
