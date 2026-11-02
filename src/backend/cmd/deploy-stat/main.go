// Command deploy-stat supplies the small read-only stat subset needed on OpenWrt.
// Build with CGO_ENABLED=0 GOOS=linux GOARCH=amd64; no installation is required.
package main

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// inspect prints only the requested metadata for one non-symlink path.
// args accepts -c, a whitelisted format, and a path; errors never print partial data.
func inspect(args []string, output io.Writer) error {
	if len(args) != 3 || args[0] != "-c" {
		return fmt.Errorf("usage: deploy-stat -c FORMAT PATH")
	}
	info, err := os.Lstat(args[2])
	if err != nil {
		return fmt.Errorf("cannot inspect path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symlink rejected")
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("unsupported filesystem metadata")
	}
	// Include special mode bits so setuid/setgid/sticky paths cannot pass ordinary checks.
	mode := uint32(metadata.Mode) & 07777
	var result string
	switch args[1] {
	case "%d":
		result = fmt.Sprint(metadata.Dev)
	case "%u":
		result = fmt.Sprint(metadata.Uid)
	case "%a":
		result = fmt.Sprintf("%o", mode)
	case "%u:%a":
		result = fmt.Sprintf("%d:%o", metadata.Uid, mode)
	case "%u:%a:%h":
		result = fmt.Sprintf("%d:%o:%d", metadata.Uid, mode, metadata.Nlink)
	default:
		return fmt.Errorf("unsupported metadata format")
	}
	_, err = fmt.Fprintln(output, result)
	return err
}

// main returns a nonzero status on any invalid input or filesystem failure.
func main() {
	if err := inspect(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "deploy-stat:", err)
		os.Exit(1)
	}
}
