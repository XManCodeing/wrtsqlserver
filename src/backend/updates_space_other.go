//go:build !linux && !darwin

package main

import "errors"

// availableUpdateSpace fails closed on unsupported systems; directory is never written by this function.
func availableUpdateSpace(directory string) (uint64, error) {
	return 0, errors.New("update staging filesystem checks are unsupported")
}
