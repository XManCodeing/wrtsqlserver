package main

import (
	"bytes"
	"fmt"
	"net"
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
	output.Reset()
	if err := inspect([]string{"-c", "%h", path}, &output); err != nil || output.String() != "2\n" {
		t.Fatalf("link count: %q %v", output.String(), err)
	}
}

// TestSocketMetadata covers the exact format required by updater socket permission checks.
func TestSocketMetadata(t *testing.T) {
	directory, err := os.MkdirTemp("", "stat-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "s")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err = inspect([]string{"-c", "%u:%g:%a", path}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != fmt.Sprintf("%d:%d:660\n", os.Getuid(), os.Getgid()) {
		t.Fatalf("unexpected socket metadata: %q", output.String())
	}
}
