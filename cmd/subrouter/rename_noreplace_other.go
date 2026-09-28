//go:build !darwin && !linux

package main

import "errors"

// renameNoReplace is unavailable here, so shared-home entries are never
// adopted into the source on this platform.
func renameNoReplace(from, to string) error {
	return errors.New("no-replace rename is not supported on this platform")
}
