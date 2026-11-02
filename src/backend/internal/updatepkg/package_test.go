package updatepkg

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// fixture returns a small manifest, raw image, and fresh signing key for package boundary tests.
func fixture(t *testing.T) (Manifest, []byte, ed25519.PrivateKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	image := []byte("opaque docker-save bytes; deliberately not extracted")
	digest := sha256.Sum256(image)
	return Manifest{FormatVersion: 1, Version: "1.2.3", Platform: "linux/amd64", ImageSHA256: hex.EncodeToString(digest[:]), ImageSize: int64(len(image)), ImageTag: "local/db-relay:1.2.3"}, image, private
}

// makeArchive constructs arbitrary signed member layouts without using the production format writer.
func makeArchive(t *testing.T, raw, image []byte, private ed25519.PrivateKey, mutate func([]*tar.Header) []*tar.Header) []byte {
	t.Helper()
	payloads := [][]byte{raw, ed25519.Sign(private, raw), image}
	headers := []*tar.Header{}
	for index, name := range []string{"manifest.json", "manifest.sig", "image.tar"} {
		headers = append(headers, &tar.Header{Name: name, Size: int64(len(payloads[index])), Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatUSTAR})
	}
	if mutate != nil {
		headers = mutate(headers)
	}
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for index, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			payload := payloads[index%3]
			if _, err := writer.Write(payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestRoundTrip validates byte-preserving signatures, exact image digests and the canonical writer.
func TestRoundTrip(t *testing.T) {
	manifest, image, private := fixture(t)
	var buffer bytes.Buffer
	if err := Write(&buffer, manifest, private, bytes.NewReader(image)); err != nil {
		t.Fatal(err)
	}
	actual, err := Verify(bytes.NewReader(buffer.Bytes()), private.Public().(ed25519.PublicKey), DefaultMaxBytes)
	if err != nil || actual != manifest {
		t.Fatal(actual, err)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(bytes.NewReader(makeArchive(t, raw, image, private, nil)), private.Public().(ed25519.PublicKey), DefaultMaxBytes); err != nil {
		t.Fatal(err)
	}
}

// TestRejectMaliciousMembers exercises traversal, links, duplicate/extra entries, PAX and trailing payloads.
func TestRejectMaliciousMembers(t *testing.T) {
	manifest, image, private := fixture(t)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func([]*tar.Header) []*tar.Header{
		"traversal": func(headers []*tar.Header) []*tar.Header { headers[0].Name = "../manifest.json"; return headers },
		"absolute":  func(headers []*tar.Header) []*tar.Header { headers[0].Name = "/manifest.json"; return headers },
		"duplicate": func(headers []*tar.Header) []*tar.Header { headers[1].Name = "manifest.json"; return headers },
		"symlink": func(headers []*tar.Header) []*tar.Header {
			headers[0].Typeflag = tar.TypeSymlink
			headers[0].Size = 0
			headers[0].Linkname = "manifest.json"
			return headers
		},
		"hardlink": func(headers []*tar.Header) []*tar.Header {
			headers[0].Typeflag = tar.TypeLink
			headers[0].Size = 0
			headers[0].Linkname = "manifest.json"
			return headers
		},
		"extra": func(headers []*tar.Header) []*tar.Header { return append(headers, headers[0]) },
		"pax": func(headers []*tar.Header) []*tar.Header {
			headers[0].Format = tar.FormatPAX
			headers[0].PAXRecords = map[string]string{"comment": "forbidden"}
			return headers
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(bytes.NewReader(makeArchive(t, raw, image, private, mutate)), private.Public().(ed25519.PublicKey), DefaultMaxBytes); err == nil {
				t.Fatal("accepted malicious archive")
			}
		})
	}
	valid := makeArchive(t, raw, image, private, nil)
	for name, candidate := range map[string][]byte{"trailing": append(append([]byte{}, valid...), 1), "zero-trailing": append(append([]byte{}, valid...), 0), "truncated": valid[:len(valid)-1]} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(bytes.NewReader(candidate), private.Public().(ed25519.PublicKey), DefaultMaxBytes); err == nil {
				t.Fatal("accepted malformed archive")
			}
		})
	}
}

// TestRejectManifestAndSignature checks strict JSON, signed field policies and image/signature tampering.
func TestRejectManifestAndSignature(t *testing.T) {
	manifest, image, private := fixture(t)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"unknown":   []byte(strings.Replace(string(raw), `"formatVersion":1`, `"formatVersion":1,"unexpected":1`, 1)),
		"duplicate": []byte(strings.Replace(string(raw), `"formatVersion":1`, `"formatVersion":1,"formatVersion":1`, 1)),
		"null":      []byte("null"), "trailing": append(append([]byte{}, raw...), []byte("{}")...),
		"platform":  bytes.Replace(raw, []byte("linux/amd64"), []byte("linux/arm64"), 1),
		"tag":       bytes.Replace(raw, []byte("local/db-relay:"), []byte("attacker/image:"), 1),
		"hash":      bytes.Replace(raw, []byte(manifest.ImageSHA256), []byte(strings.Repeat("0", 64)), 1),
		"oversized": bytes.Repeat([]byte(" "), MaxManifestBytes+1),
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(bytes.NewReader(makeArchive(t, candidate, image, private, nil)), private.Public().(ed25519.PublicKey), DefaultMaxBytes); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
	valid := makeArchive(t, raw, image, private, nil)
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(bytes.NewReader(valid), other.Public().(ed25519.PublicKey), DefaultMaxBytes); err == nil {
		t.Fatal("accepted wrong key")
	}
	manifest.ImageSize++
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(bytes.NewReader(makeArchive(t, raw, image, private, nil)), private.Public().(ed25519.PublicKey), DefaultMaxBytes); err == nil {
		t.Fatal("accepted size mismatch")
	}
}
