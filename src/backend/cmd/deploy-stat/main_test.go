package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInspect verifies modes, link counts and fail-closed argument/path handling on temporary files.
func TestInspect(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "private")
	if err := os.WriteFile(path, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := inspect([]string{"-c", "%a", path}, &output); err != nil || output.String() != "600\n" {
		t.Fatalf("mode: %q %v", output.String(), err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{}, {"-c", "%s", path}, {"-c", "%a", link}, {"-c", "%a", path + "missing"}} {
		output.Reset()
		if err := inspect(args, &output); err == nil || output.Len() != 0 {
			t.Fatalf("invalid request accepted: %v", args)
		}
	}
	if err := os.Link(path, filepath.Join(directory, "hardlink")); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := inspect([]string{"-c", "%u:%a:%h", path}, &output); err != nil || !strings.HasSuffix(output.String(), ":600:2\n") {
		t.Fatalf("hardlink: %q %v", output.String(), err)
	}
}
