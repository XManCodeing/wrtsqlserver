// Package updatepkg implements a signed, non-extracting update package format.
package updatepkg

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const MaxManifestBytes = 16 << 10
const DefaultMaxBytes int64 = 256 << 20

// Manifest binds an exact image stream to its version and target platform.
type Manifest struct {
	FormatVersion int    `json:"formatVersion"`
	Version       string `json:"version"`
	Platform      string `json:"platform"`
	ImageSHA256   string `json:"imageSHA256"`
	ImageSize     int64  `json:"imageSize"`
	ImageTag      string `json:"imageTag"`
}

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*)?$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Validate checks manifest fields against maxBytes; it returns a safe format error.
func (manifest Manifest) Validate(maxBytes int64) error {
	if manifest.FormatVersion != 1 || manifest.Platform != "linux/amd64" {
		return errors.New("unsupported update format or platform")
	}
	if len(manifest.Version) > 80 || !versionPattern.MatchString(manifest.Version) || manifest.ImageTag != "local/db-relay:"+manifest.Version {
		return errors.New("invalid update version or image tag")
	}
	if manifest.ImageSize <= 0 || manifest.ImageSize > maxBytes || !digestPattern.MatchString(manifest.ImageSHA256) {
		return errors.New("invalid image size or SHA256")
	}
	return nil
}

// decodeManifest rejects unknown, duplicate, missing and trailing JSON fields in raw bytes.
func decodeManifest(raw []byte, maxBytes int64) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return manifest, errors.New("manifest must be an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return manifest, errors.New("invalid manifest field")
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return manifest, errors.New("duplicate manifest field")
		}
		seen[name] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return manifest, errors.New("invalid manifest value")
		}
	}
	if len(seen) != 6 {
		return manifest, errors.New("manifest fields are incomplete")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&manifest); err != nil {
		return manifest, errors.New("invalid or unknown manifest fields")
	}
	if err := strict.Decode(new(any)); err != io.EOF {
		return manifest, errors.New("trailing manifest data")
	}
	return manifest, manifest.Validate(maxBytes)
}

// readHeader reads exactly one canonical USTAR regular-file header for name and bounds its size.
func readHeader(reader io.Reader, name string, maximum int64) (int64, error) {
	block := make([]byte, 512)
	if _, err := io.ReadFull(reader, block); err != nil {
		return 0, errors.New("truncated package header")
	}
	if block[156] != '0' || string(block[257:263]) != "ustar\x00" || !zero(block[345:500]) {
		return 0, errors.New("only USTAR regular files are allowed")
	}
	header, err := tar.NewReader(bytes.NewReader(block)).Next()
	if err != nil || header.Name != name || header.Linkname != "" || header.Size <= 0 || header.Size > maximum {
		return 0, fmt.Errorf("invalid package member: expected %s", name)
	}
	return header.Size, nil
}

// zero returns whether all bytes in data are zero, for canonical padding validation.
func zero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

// readPadding consumes zero-filled padding for a member of size bytes, returning malformed data errors.
func readPadding(reader io.Reader, size int64) error {
	padding := make([]byte, (512-size%512)%512)
	if _, err := io.ReadFull(reader, padding); err != nil || !zero(padding) {
		return errors.New("invalid package padding")
	}
	return nil
}

// readSmall reads a bounded fixed-name member without extracting it to disk.
func readSmall(reader io.Reader, name string, maximum int64) ([]byte, error) {
	size, err := readHeader(reader, name, maximum)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, size)
	if _, err = io.ReadFull(reader, raw); err != nil {
		return nil, errors.New("truncated package member")
	}
	return raw, readPadding(reader, size)
}

// Verify streams exactly three ordered members, authenticates raw manifest bytes and hashes image bytes.
// The caller must additionally bound the complete request; no image contents are extracted or executed.
func Verify(reader io.Reader, public ed25519.PublicKey, maxBytes int64) (Manifest, error) {
	var manifest Manifest
	raw, err := readSmall(reader, "manifest.json", MaxManifestBytes)
	if err != nil {
		return manifest, err
	}
	signature, err := readSmall(reader, "manifest.sig", ed25519.SignatureSize)
	if err != nil {
		return manifest, err
	}
	if len(public) != ed25519.PublicKeySize || !ed25519.Verify(public, raw, signature) {
		return manifest, errors.New("invalid update signature")
	}
	manifest, err = decodeManifest(raw, maxBytes)
	if err != nil {
		return manifest, err
	}
	size, err := readHeader(reader, "image.tar", maxBytes)
	if err != nil {
		return manifest, err
	}
	if size != manifest.ImageSize {
		return manifest, errors.New("image size mismatch")
	}
	hash := sha256.New()
	if _, err := io.CopyN(hash, reader, size); err != nil {
		return manifest, errors.New("truncated image stream")
	}
	if hex.EncodeToString(hash.Sum(nil)) != manifest.ImageSHA256 {
		return manifest, errors.New("image SHA256 mismatch")
	}
	if err := readPadding(reader, size); err != nil {
		return manifest, err
	}
	ending := make([]byte, 1024)
	if _, err := io.ReadFull(reader, ending); err != nil || !zero(ending) {
		return manifest, errors.New("invalid package end marker")
	}
	var extra [1]byte
	if count, err := reader.Read(extra[:]); count != 0 || err != io.EOF {
		return manifest, errors.New("extra package members or trailing data")
	}
	return manifest, nil
}

// Write emits the canonical signed package to writer using manifest and an already-saved image stream.
// It never invokes Docker; callers remove partial output on error.
func Write(writer io.Writer, manifest Manifest, private ed25519.PrivateKey, image io.Reader) error {
	if err := manifest.Validate(DefaultMaxBytes); err != nil {
		return err
	}
	if len(private) != ed25519.PrivateKeySize {
		return errors.New("invalid signing key")
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	archive := tar.NewWriter(writer)
	for _, member := range []struct {
		name string
		data []byte
	}{{"manifest.json", raw}, {"manifest.sig", ed25519.Sign(private, raw)}} {
		if err = archive.WriteHeader(&tar.Header{Name: member.name, Size: int64(len(member.data)), Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return err
		}
		if _, err = archive.Write(member.data); err != nil {
			return err
		}
	}
	if err = archive.WriteHeader(&tar.Header{Name: "image.tar", Size: manifest.ImageSize, Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err = io.CopyN(io.MultiWriter(archive, hash), image, manifest.ImageSize); err != nil {
		return err
	}
	var extra [1]byte
	if count, readErr := image.Read(extra[:]); count != 0 || readErr != io.EOF {
		return errors.New("image changed while packaging")
	}
	if hex.EncodeToString(hash.Sum(nil)) != manifest.ImageSHA256 {
		return errors.New("image changed while packaging")
	}
	return archive.Close()
}
