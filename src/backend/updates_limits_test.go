package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// uploadTimeoutError implements a transport read timeout without waiting or running a service.
type uploadTimeoutError struct{}

// Error returns a static test error description with no request contents.
func (uploadTimeoutError) Error() string { return "test upload timeout" }

// Timeout marks this failure as an expired network read deadline.
func (uploadTimeoutError) Timeout() bool { return true }

// Temporary provides compatibility with the net.Error interface.
func (uploadTimeoutError) Temporary() bool { return true }

// timeoutUploadReader fails immediately, simulating a client stalled beyond the read deadline.
type timeoutUploadReader struct{}

// Read returns an explicit timeout for every requested buffer.
func (timeoutUploadReader) Read(data []byte) (int, error) { return 0, uploadTimeoutError{} }

// TestUpdateTimeoutAndUnsupportedDeadlines checks 408 and fail-closed behavior with no deadline-capable transport.
func TestUpdateTimeoutAndUnsupportedDeadlines(t *testing.T) {
	app := testApp(t)
	manager, archive, _ := updateFixture(t)
	app.updates = manager
	cookie, csrf := loginTest(t, app)
	response := uploadRequest(app, cookie, csrf, timeoutUploadReader{}, -1)
	if response.Code != http.StatusRequestTimeout {
		t.Fatal(response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "http://localhost/api/updates/package", bytes.NewReader(archive))
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-CSRF-Token", csrf)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatal(recorder.Code)
	}
	entries, err := os.ReadDir(manager.directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed upload left data", err)
	}
}

// TestUpdateChunkedReaderBound confirms byte-limit errors survive the format validator for HTTP classification.
func TestUpdateChunkedReaderBound(t *testing.T) {
	manager, archive, _ := updateFixture(t)
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(archive))
	request.ContentLength = -1
	bounded := &failureReader{reader: http.MaxBytesReader(httptest.NewRecorder(), request.Body, 600)}
	if _, err := manager.savePackage(bounded); err == nil {
		t.Fatal("byte limit bypassed")
	}
	var tooLarge *http.MaxBytesError
	if !errors.As(bounded.failure, &tooLarge) {
		t.Fatal("byte limit error lost", bounded.failure)
	}
	if manager.status().Package != nil {
		t.Fatal("bounded failure advertised")
	}
}

// TestUpdateRecoverySymlinkAndReservePreservesPrevious verifies reserve failure and unsafe restart files never replace trust.
func TestUpdateRecoverySymlinkAndReservePreservesPrevious(t *testing.T) {
	manager, archive, keyPath := updateFixture(t)
	previous, err := manager.savePackage(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	manager.freeSpace = func(string) (uint64, error) { return 0, nil }
	if _, err := manager.savePackage(bytes.NewReader(archive)); !errors.Is(err, errUpdateSpace) {
		t.Fatal(err)
	}
	if actual := manager.status().Package; actual == nil || *actual != *previous {
		t.Fatal("reserve failure changed summary")
	}
	path := filepath.Join(manager.directory, updatePackageName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keyPath, path); err != nil {
		t.Fatal(err)
	}
	if restored := newUpdateManager(keyPath, manager.directory); restored.status().Package != nil {
		t.Fatal("restored symlink")
	}
}

// deadlineCaptureWriter records all deadline changes so the upload-specific finite extension can be asserted.
type deadlineCaptureWriter struct {
	*httptest.ResponseRecorder
	reads, writes []time.Time
}

// SetReadDeadline records each per-request read deadline instead of accessing sockets.
func (writer *deadlineCaptureWriter) SetReadDeadline(value time.Time) error {
	writer.reads = append(writer.reads, value)
	return nil
}

// SetWriteDeadline records each per-request write deadline instead of accessing sockets.
func (writer *deadlineCaptureWriter) SetWriteDeadline(value time.Time) error {
	writer.writes = append(writer.writes, value)
	return nil
}

// TestUpdateFiniteDeadlineExtension ensures uploads override 15 seconds without disabling deadlines globally.
func TestUpdateFiniteDeadlineExtension(t *testing.T) {
	manager, archive, _ := updateFixture(t)
	request := httptest.NewRequest(http.MethodPost, "/api/updates/package", bytes.NewReader(archive))
	request.Header.Set("Content-Type", "application/octet-stream")
	writer := &deadlineCaptureWriter{ResponseRecorder: httptest.NewRecorder()}
	started := time.Now()
	manager.uploadPackage(writer, request)
	if writer.Code != 200 || len(writer.reads) != 2 || len(writer.writes) != 2 {
		t.Fatal(writer.Code, writer.Body.String())
	}
	if duration := writer.reads[0].Sub(started); duration < 4*time.Minute || duration > 6*time.Minute {
		t.Fatal("invalid upload deadline", duration)
	}
	if duration := writer.writes[0].Sub(started); duration < 5*time.Minute || duration > 6*time.Minute {
		t.Fatal("invalid write deadline", duration)
	}
	if duration := writer.reads[1].Sub(started); duration <= 0 || duration > 30*time.Second {
		t.Fatal("read deadline not restored", duration)
	}
	if _, err := io.Copy(io.Discard, request.Body); err != nil {
		t.Fatal(err)
	}
}
