package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"opernwrtdocker/backend/internal/updatepkg"
)

// updateFixture creates isolated keys and a valid opaque image package; returns manager, archive and key path.
func updateFixture(t *testing.T) (*updateManager, []byte, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "public.key")
	if err = os.WriteFile(keyPath, public, 0600); err != nil {
		t.Fatal(err)
	}
	manager := newUpdateManager(keyPath, filepath.Join(root, "staging"))
	if !manager.enabled {
		t.Fatal(manager.reason)
	}
	image := []byte("opaque saved image bytes")
	digest := sha256.Sum256(image)
	manifest := updatepkg.Manifest{FormatVersion: 1, Version: "1.2.3", Platform: "linux/amd64", ImageSHA256: hex.EncodeToString(digest[:]), ImageSize: int64(len(image)), ImageTag: "local/db-relay:1.2.3"}
	var archive bytes.Buffer
	if err = updatepkg.Write(&archive, manifest, private, bytes.NewReader(image)); err != nil {
		t.Fatal(err)
	}
	return manager, archive.Bytes(), keyPath
}

// deadlineRecorder simulates supported per-request deadlines without starting an external service.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

// SetReadDeadline records the finite deadline requested by the upload handler.
func (recorder *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	recorder.read = deadline
	return nil
}

// SetWriteDeadline records the finite response deadline requested by the upload handler.
func (recorder *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	recorder.write = deadline
	return nil
}

// uploadRequest invokes authorized upload requests with a deadline-capable writer and configurable body length.
func uploadRequest(app *App, cookie *http.Cookie, csrf string, body io.Reader, length int64) *deadlineRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://localhost/api/updates/package", body)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-CSRF-Token", csrf)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	request.ContentLength = length
	recorder := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	app.ServeHTTP(recorder, request)
	return recorder
}

// TestUpdateAuthAndContract verifies default disablement, auth/CSRF, upload metadata, delete and no apply route.
func TestUpdateAuthAndContract(t *testing.T) {
	app := testApp(t)
	app.updates = newUpdateManager("", "")
	cookie, csrf := loginTest(t, app)
	if response := requestAPI(app, "GET", "/api/updates", "", nil, ""); response.Code != 401 {
		t.Fatal(response.Code)
	}
	var status UpdateStatus
	response := requestAPI(app, "GET", "/api/updates", "", cookie, "")
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.UploadEnabled || status.ApplyEnabled || status.Package != nil || status.Reason == "" || status.MaxUploadBytes != 256<<20 {
		t.Fatal(status)
	}
	manager, archive, _ := updateFixture(t)
	app.updates = manager
	if response := uploadRequest(app, nil, "", bytes.NewReader(archive), int64(len(archive))); response.Code != 401 {
		t.Fatal(response.Code)
	}
	if response := uploadRequest(app, cookie, "", bytes.NewReader(archive), int64(len(archive))); response.Code != 403 {
		t.Fatal(response.Code)
	}
	uploaded := uploadRequest(app, cookie, csrf, bytes.NewReader(archive), int64(len(archive)))
	if uploaded.Code != 200 || uploaded.read.IsZero() || uploaded.write.IsZero() {
		t.Fatal(uploaded.Code, uploaded.Body.String())
	}
	var summary UpdatePackage
	if err := json.Unmarshal(uploaded.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Version != "1.2.3" || summary.Platform != "linux/amd64" || summary.UploadedAt.IsZero() {
		t.Fatal(summary)
	}
	if response := requestAPI(app, "POST", "/api/updates/apply", "", cookie, csrf); response.Code != 404 {
		t.Fatal(response.Code)
	}
	if response := requestAPI(app, "DELETE", "/api/updates/package", "", cookie, ""); response.Code != 403 {
		t.Fatal(response.Code)
	}
	if response := requestAPI(app, "DELETE", "/api/updates/package", "", cookie, csrf); response.Code != 204 {
		t.Fatal(response.Code)
	}
	if manager.status().Package != nil {
		t.Fatal("delete left summary")
	}
}

// TestUpdatePersistenceAndFailure proves invalid uploads preserve valid packages and startup revalidates disk bytes.
func TestUpdatePersistenceAndFailure(t *testing.T) {
	manager, archive, keyPath := updateFixture(t)
	original, err := manager.savePackage(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.savePackage(bytes.NewReader(append(append([]byte{}, archive...), 1))); err == nil {
		t.Fatal("accepted trailing garbage")
	}
	restored := newUpdateManager(keyPath, manager.directory)
	if restored.summary == nil || *restored.summary != *original {
		t.Fatal("valid package not recovered")
	}
	entries, err := os.ReadDir(manager.directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != updatePackageName {
		t.Fatal(entries, err)
	}
	info, err := os.Stat(manager.directory)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("unsafe staging permissions")
	}
	if err = os.WriteFile(filepath.Join(manager.directory, updatePackageName), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if recovered := newUpdateManager(keyPath, manager.directory); recovered.summary != nil {
		t.Fatal("recovered tampered package")
	}
}

// blockingUpdateReader signals the first read, then waits until released to exercise in-flight concurrency.
type blockingUpdateReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	reader  io.Reader
}

// Read blocks only its first call, then delegates to reader without accessing application locks.
func (reader *blockingUpdateReader) Read(data []byte) (int, error) {
	reader.once.Do(func() { close(reader.entered); <-reader.release })
	return reader.reader.Read(data)
}

// TestUpdateConcurrentUploadAndStatus ensures 429 for competing mutations and unblocked App.mu/status during streaming.
func TestUpdateConcurrentUploadAndStatus(t *testing.T) {
	app := testApp(t)
	manager, archive, _ := updateFixture(t)
	app.updates = manager
	cookie, csrf := loginTest(t, app)
	reader := &blockingUpdateReader{entered: make(chan struct{}), release: make(chan struct{}), reader: bytes.NewReader(archive)}
	completed := make(chan *deadlineRecorder, 1)
	go func() { completed <- uploadRequest(app, cookie, csrf, reader, int64(len(archive))) }()
	<-reader.entered
	if response := uploadRequest(app, cookie, csrf, bytes.NewReader(archive), int64(len(archive))); response.Code != 429 {
		t.Fatal(response.Code)
	}
	if response := requestAPI(app, "DELETE", "/api/updates/package", "", cookie, csrf); response.Code != 429 {
		t.Fatal(response.Code)
	}
	if !app.mu.TryLock() {
		t.Fatal("upload held app lock")
	}
	app.mu.Unlock()
	if response := requestAPI(app, "GET", "/api/updates", "", cookie, ""); response.Code != 200 {
		t.Fatal(response.Code)
	}
	close(reader.release)
	if response := <-completed; response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
}

// TestUpdateSpaceAndRequestBounds checks preflight, in-stream reserve loss, size/type and fail-closed deadlines.
func TestUpdateSpaceAndRequestBounds(t *testing.T) {
	app := testApp(t)
	manager, archive, _ := updateFixture(t)
	app.updates = manager
	cookie, csrf := loginTest(t, app)
	if response := uploadRequest(app, cookie, csrf, bytes.NewReader(nil), updatepkg.DefaultMaxBytes+1); response.Code != 413 {
		t.Fatal(response.Code)
	}
	if response := requestAPI(app, "POST", "/api/updates/package", "", cookie, csrf); response.Code != 415 {
		t.Fatal(response.Code)
	}
	manager.freeSpace = func(string) (uint64, error) { return 0, nil }
	if response := uploadRequest(app, cookie, csrf, bytes.NewReader(archive), int64(len(archive))); response.Code != 507 {
		t.Fatal(response.Code)
	}
	checks := 0
	manager.freeSpace = func(string) (uint64, error) {
		checks++
		if checks > 2 {
			return 0, nil
		}
		return 1 << 40, nil
	}
	if response := uploadRequest(app, cookie, csrf, bytes.NewReader(archive), -1); response.Code != 507 {
		t.Fatal(response.Code, response.Body.String())
	}
	if manager.status().Package != nil {
		t.Fatal("failed package advertised")
	}
	entries, err := os.ReadDir(manager.directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("temporary file leaked", err)
	}
}

// TestUpdateConfigurationRejectsUnsafePaths checks explicit configuration and stage/public-key symlinks.
func TestUpdateConfigurationRejectsUnsafePaths(t *testing.T) {
	manager, _, keyPath := updateFixture(t)
	for _, paths := range [][2]string{{"", manager.directory}, {keyPath, ""}, {keyPath, "relative"}, {keyPath, "/"}} {
		if newUpdateManager(paths[0], paths[1]).enabled {
			t.Fatal("unsafe config enabled")
		}
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(manager.directory, link); err != nil {
		t.Fatal(err)
	}
	if newUpdateManager(keyPath, link).enabled {
		t.Fatal("symlink stage enabled")
	}
	publicLink := filepath.Join(t.TempDir(), "key")
	if err := os.Symlink(keyPath, publicLink); err != nil {
		t.Fatal(err)
	}
	if newUpdateManager(publicLink, manager.directory).enabled {
		t.Fatal("symlink key enabled")
	}
}
