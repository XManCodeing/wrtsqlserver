// update-pack creates signing keys only on explicit request, or packages an existing docker-save image.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"opernwrtdocker/backend/internal/updatepkg"
)

// main invokes the explicit subcommand and reports safe failures without key material.
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "update-pack:", err)
		os.Exit(1)
	}
}

// run dispatches keygen or pack from arguments, writing only nonsecret result messages to output.
func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: update-pack keygen --private-key PATH --public-key PATH | pack --private-key PATH --image PATH --version VERSION --out PATH")
	}
	switch arguments[0] {
	case "keygen":
		return generateKeys(arguments[1:], output)
	case "pack":
		return pack(arguments[1:], output)
	default:
		return errors.New("unknown subcommand: use keygen or pack")
	}
}

// writeExclusive creates a new mode-0600 key file, never overwriting an existing path.
func writeExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("cannot create key file (existing files are not overwritten)")
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return errors.New("cannot write key file")
	}
	if err = file.Sync(); err != nil {
		return errors.New("cannot sync key file")
	}
	if err = file.Close(); err != nil {
		return errors.New("cannot close key file")
	}
	complete = true
	return nil
}

// generateKeys creates raw Ed25519 private/public files only for the explicit keygen subcommand.
func generateKeys(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	privatePath := flags.String("private-key", "", "new raw private-key file (64 bytes, mode 0600)")
	publicPath := flags.String("public-key", "", "new raw public-key file (32 bytes)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *privatePath == "" || *publicPath == "" || filepath.Clean(*privatePath) == filepath.Clean(*publicPath) {
		return errors.New("keygen requires distinct --private-key and --public-key paths")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("cannot generate signing key")
	}
	if err = writeExclusive(*privatePath, private); err != nil {
		return err
	}
	if err = writeExclusive(*publicPath, public); err != nil {
		if removeErr := os.Remove(*privatePath); removeErr != nil {
			return errors.New("public key creation failed; private key cleanup failed")
		}
		return err
	}
	_, err = fmt.Fprintln(output, "Signing key files created. Keep the private key offline; configure only the public key on the server.")
	return err
}

// readPrivate reads a raw mode-0600 key from path, rejecting symlinks and malformed Ed25519 material.
func readPrivate(path string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("signing key must be a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open signing key")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("signing key changed during open")
	}
	raw, err := io.ReadAll(io.LimitReader(file, ed25519.PrivateKeySize+1))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid signing key length")
	}
	canonical := ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize])
	if !ed25519.PrivateKey(raw).Equal(canonical) {
		return nil, errors.New("invalid signing key material")
	}
	return ed25519.PrivateKey(raw), nil
}

// imageManifest streams a regular saved image to compute its hash and rewinds it for packaging.
func imageManifest(image *os.File, version string) (updatepkg.Manifest, error) {
	manifest := updatepkg.Manifest{FormatVersion: 1, Version: version, Platform: "linux/amd64", ImageTag: "local/db-relay:" + version}
	info, err := image.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > updatepkg.DefaultMaxBytes {
		return manifest, errors.New("image must be a nonempty regular file within the package limit")
	}
	manifest.ImageSize = info.Size()
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(image, info.Size()+1))
	if err != nil || count != info.Size() {
		return manifest, errors.New("cannot hash image file or image size changed")
	}
	manifest.ImageSHA256 = hex.EncodeToString(hash.Sum(nil))
	if _, err = image.Seek(0, io.SeekStart); err != nil {
		return manifest, errors.New("cannot rewind image file")
	}
	return manifest, manifest.Validate(updatepkg.DefaultMaxBytes)
}

// pack signs an existing image file with an explicit key and creates a new canonical archive without Docker.
func pack(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("pack", flag.ContinueOnError)
	keyPath := flags.String("private-key", "", "existing raw Ed25519 private-key file")
	imagePath := flags.String("image", "", "existing docker save image.tar file")
	version := flags.String("version", "", "semantic version (for example 1.2.3)")
	outPath := flags.String("out", "", "new uncompressed package.tar file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *keyPath == "" || *imagePath == "" || *version == "" || *outPath == "" {
		return errors.New("pack requires --private-key, --image, --version and --out")
	}
	private, err := readPrivate(*keyPath)
	if err != nil {
		return err
	}
	image, err := os.Open(*imagePath)
	if err != nil {
		return errors.New("cannot open saved image")
	}
	defer image.Close()
	manifest, err := imageManifest(image, *version)
	if err != nil {
		return err
	}
	return writePackage(*outPath, manifest, private, image, output)
}

// writePackage creates output exclusively and removes incomplete files if any validation or persistence fails.
func writePackage(path string, manifest updatepkg.Manifest, private ed25519.PrivateKey, image io.Reader, output io.Writer) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create package (existing files are not overwritten)")
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			if err := os.Remove(path); err != nil {
				fmt.Fprintln(os.Stderr, "partial package cleanup failed")
			}
		}
	}()
	if err = updatepkg.Write(file, manifest, private, image); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || info.Size() > updatepkg.DefaultMaxBytes {
		return errors.New("complete package exceeds 256 MiB")
	}
	if err = file.Sync(); err != nil {
		return errors.New("cannot sync package")
	}
	if err = file.Close(); err != nil {
		return errors.New("cannot close package")
	}
	complete = true
	_, err = fmt.Fprintln(output, "Signed update package created; no image was loaded or executed.")
	return err
}
