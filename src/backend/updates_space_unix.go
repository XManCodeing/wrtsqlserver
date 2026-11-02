//go:build linux || darwin

package main

import "syscall"

// availableUpdateSpace returns unprivileged available bytes for directory on Linux and macOS.
func availableUpdateSpace(directory string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(directory, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
