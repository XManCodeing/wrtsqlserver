package main

import (
	"bytes"
	"crypto/ed25519"
	"io"
	"os"
	"path/filepath"
	"testing"

	"opernwrtdocker/backend/internal/updatepkg"
)

// TestExplicitKeygenAndPack verifies no implicit key generation, overwrite protection and signed CLI output.
func TestExplicitKeygenAndPack(t *testing.T) {
	root := t.TempDir()
	privatePath, publicPath := filepath.Join(root, "private.key"), filepath.Join(root, "public.key")
	imagePath, packagePath := filepath.Join(root, "image.tar"), filepath.Join(root, "package.tar")
	if err := run(nil, io.Discard); err == nil {
		t.Fatal("missing subcommand accepted")
	}
	if err := os.WriteFile(imagePath, []byte("opaque pre-saved image"), 0600); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"pack", "--private-key", privatePath, "--image", imagePath, "--version", "1.2.3", "--out", packagePath}
	if err := run(arguments, io.Discard); err == nil {
		t.Fatal("missing signing key accepted")
	}
	if _, err := os.Stat(privatePath); !os.IsNotExist(err) {
		t.Fatal("implicit key generation")
	}
	keyArguments := []string{"keygen", "--private-key", privatePath, "--public-key", publicPath}
	if err := run(keyArguments, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run(keyArguments, io.Discard); err == nil {
		t.Fatal("existing keys overwritten")
	}
	private, err := os.ReadFile(privatePath)
	if err != nil || len(private) != 64 {
		t.Fatal(err)
	}
	info, err := os.Stat(privatePath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private key permissions")
	}
	public, err := os.ReadFile(publicPath)
	if err != nil || len(public) != 32 {
		t.Fatal(err)
	}
	if err := run(arguments, io.Discard); err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := updatepkg.Verify(bytes.NewReader(archive), ed25519.PublicKey(public), updatepkg.DefaultMaxBytes)
	if err != nil || manifest.ImageTag != "local/db-relay:1.2.3" {
		t.Fatal(manifest, err)
	}
	if err := run(arguments, io.Discard); err == nil {
		t.Fatal("existing package overwritten")
	}
}

// TestSigningKeyPermissionsAndFailureCleanup verifies unsafe keys and invalid versions fail without output artifacts.
func TestSigningKeyPermissionsAndFailureCleanup(t *testing.T) {
	root := t.TempDir()
	privatePath, publicPath := filepath.Join(root, "private.key"), filepath.Join(root, "public.key")
	if err := run([]string{"keygen", "--private-key", privatePath, "--public-key", publicPath}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(privatePath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(privatePath); err == nil {
		t.Fatal("publicly readable key accepted")
	}
	if err := os.Chmod(privatePath, 0600); err != nil {
		t.Fatal(err)
	}
	private, err := readPrivate(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(root, "partial.tar")
	if err := writePackage(outputPath, updatepkg.Manifest{}, private, bytes.NewReader(nil), io.Discard); err == nil {
		t.Fatal("invalid manifest accepted")
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatal("partial output retained")
	}
}
