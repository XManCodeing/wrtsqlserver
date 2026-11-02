package onlineupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// stagedTestHost extends the existing fake without changing legacy callers or mocks.
type stagedTestHost struct {
	*fakeHost
	verifyErr bool
	gate      chan struct{}
	entered   chan struct{}
	afterPull func()
}

// VerifyPrepared records local verification and returns injected image loss without pulling.
func (host *stagedTestHost) VerifyPrepared(context.Context, Release) error {
	host.calls = append(host.calls, "verify")
	if host.verifyErr {
		return errors.New("missing image")
	}
	return nil
}

// PullWithProgress emits measured test data and optionally blocks to exercise mutual exclusion.
func (host *stagedTestHost) PullWithProgress(ctx context.Context, release Release, report func(DownloadProgress)) error {
	report(DownloadProgress{DownloadedBytes: 25, TotalBytes: 100})
	if host.entered != nil {
		close(host.entered)
	}
	if host.gate != nil {
		<-host.gate
	}
	if host.afterPull != nil {
		host.afterPull()
	}
	return host.Pull(ctx, release)
}

// stagedFixture returns a checked service and isolated host/store with no network effects.
func stagedFixture(t *testing.T) (*Service, *stagedTestHost, *memoryStore) {
	t.Helper()
	host := &stagedTestHost{fakeHost: &fakeHost{current: "1.0.0"}}
	store := &memoryStore{}
	service, err := NewService(store, host, &fakeSource{release: testRelease()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	return service, host, store
}

// downloadFixture waits for the explicitly requested download and fails on unexpected rejection.
func downloadFixture(t *testing.T, service *Service) {
	t.Helper()
	release := testRelease()
	if err := service.Download(release.Version, release.Digest, "更新 "+release.Version); err != nil {
		t.Fatal(err)
	}
	service.Wait()
}

// TestStagedLifecycle proves download does not finalize/stop, survives restart, and activates without pull.
func TestStagedLifecycle(t *testing.T) {
	service, host, store := stagedFixture(t)
	host.transaction, host.applied = true, true
	service.mu.Lock()
	service.record.Status.CanRollback = true
	service.record.PreviousVersion = "0.9.0"
	service.mu.Unlock()
	downloadFixture(t, service)
	status := service.Status()
	if status.State != "downloaded" || status.Phase != "downloaded" || status.Prepared == nil || !status.StagedUpdates || !status.CanRollback {
		t.Fatalf("bad staged status: %+v", status)
	}
	if strings.Join(host.calls, ",") != "pull,verify" || host.current != "1.0.0" || store.record.PreviousVersion != "0.9.0" {
		t.Fatal("download changed deployment/rollback")
	}
	restored, err := NewService(store, host, &fakeSource{release: testRelease()}, nil)
	if err != nil || restored.Status().Prepared == nil {
		t.Fatalf("restore failed: %v", err)
	}
	release := testRelease()
	if err = restored.Activate(release.Version, release.Digest, "更新 "+release.Version); err != nil {
		t.Fatal(err)
	}
	restored.Wait()
	if restored.Status().State != "succeeded" || restored.Status().Prepared != nil || strings.Count(strings.Join(host.calls, ","), "pull") != 1 {
		t.Fatalf("activation failed: %+v %v", restored.Status(), host.calls)
	}
	if !strings.Contains(strings.Join(host.calls, ","), "finalize,check,apply") {
		t.Fatal("shared deployment transaction not reused")
	}
}

// TestStagedInvalidation rejects lost images, changed versions, source mismatch and stale identities.
func TestStagedInvalidation(t *testing.T) {
	for _, reason := range []string{"image", "version", "source", "digest"} {
		t.Run(reason, func(t *testing.T) {
			service, host, _ := stagedFixture(t)
			downloadFixture(t, service)
			release := testRelease()
			switch reason {
			case "image":
				host.verifyErr = true
			case "version":
				host.current = release.Version
			case "source":
				if _, err := service.SaveSource(alternativeSource()); err != nil {
					t.Fatal(err)
				}
			case "digest":
				release.Digest = "sha256:" + strings.Repeat("b", 64)
			}
			err := service.Activate(release.Version, release.Digest, "更新 "+release.Version)
			if reason == "source" || reason == "digest" {
				if err == nil {
					t.Fatal("stale identity accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			service.Wait()
			if service.Status().State != "failed" || service.Status().Prepared != nil || strings.Contains(strings.Join(host.calls, ","), "apply") {
				t.Fatal("invalid image/version activated")
			}
		})
	}
}

// TestStagedDownloadExclusion checks status progress and every shared mutation reservation while downloading.
func TestStagedDownloadExclusion(t *testing.T) {
	service, host, _ := stagedFixture(t)
	host.entered, host.gate = make(chan struct{}), make(chan struct{})
	release := testRelease()
	if err := service.Download(release.Version, release.Digest, "更新 "+release.Version); err != nil {
		t.Fatal(err)
	}
	<-host.entered
	status := service.Status()
	if status.State != "downloading" || status.DownloadPercent == nil || *status.DownloadPercent != 25 || status.DownloadedBytes != 25 || status.TotalBytes != 100 {
		t.Fatalf("bad progress: %+v", status)
	}
	if service.Check() == nil || service.Download(release.Version, release.Digest, "更新 "+release.Version) == nil || service.Apply(release.Version, release.Digest, "更新 "+release.Version) == nil || service.Activate(release.Version, release.Digest, "更新 "+release.Version) == nil || service.Rollback("回退转发程序") == nil {
		t.Error("concurrent operation accepted")
	}
	if _, err := service.SaveSource(alternativeSource()); err == nil {
		t.Error("source changed during download")
	}
	service.mu.Lock()
	if service.beginLocked("applying", "ports", "ports") == nil {
		t.Error("port operation accepted")
	}
	service.mu.Unlock()
	close(host.gate)
	service.Wait()
}

// TestStagedPersistenceFailure fails closed both before pull and after local verification.
func TestStagedPersistenceFailure(t *testing.T) {
	for _, stage := range []string{"intent", "prepared"} {
		t.Run(stage, func(t *testing.T) {
			service, host, store := stagedFixture(t)
			fail := func() { store.mu.Lock(); store.fail = true; store.mu.Unlock() }
			if stage == "intent" {
				fail()
			} else {
				host.afterPull = fail
			}
			release := testRelease()
			err := service.Download(release.Version, release.Digest, "更新 "+release.Version)
			if stage == "intent" && err == nil {
				t.Fatal("unpersisted intent accepted")
			}
			service.Wait()
			if service.Status().Enabled || service.Activate(release.Version, release.Digest, "更新 "+release.Version) == nil {
				t.Fatal("persistence failure not closed")
			}
			if strings.Contains(strings.Join(host.calls, ","), "apply") {
				t.Fatal("deployment despite failed persistence")
			}
		})
	}
}

// TestStagedRestartRecovery safely clears interrupted downloads and rejects missing persisted images.
func TestStagedRestartRecovery(t *testing.T) {
	service, host, store := stagedFixture(t)
	downloadFixture(t, service)
	host.verifyErr = true
	restored, err := NewService(store, host, &fakeSource{release: testRelease()}, nil)
	if err != nil || restored.Status().Prepared != nil || restored.Status().State != "failed" {
		t.Fatalf("invalid prepared restored: %v", err)
	}
	store.record.Status.State = "downloading"
	store.record.DownloadOnly = true
	before := len(host.calls)
	restored, err = NewService(store, host, &fakeSource{release: testRelease()}, nil)
	if err != nil || restored.Status().State != "failed" || len(host.calls) != before {
		t.Fatal("interrupted download resumed or blocked recovery")
	}
}

// TestStagedRestartAfterRollback preserves a completed rollback journal while a new image is staged.
func TestStagedRestartAfterRollback(t *testing.T) {
	service, host, store := stagedFixture(t)
	host.transaction, host.rolledBack = true, true
	service.mu.Lock()
	service.record.Status.State = "rolled_back"
	service.record.Status.CanRollback = false
	service.mu.Unlock()
	downloadFixture(t, service)
	restored, err := NewService(store, host, &fakeSource{release: testRelease()}, nil)
	if err != nil || restored.Status().State != "downloaded" {
		t.Fatalf("completed journal blocked staged restart: %v", err)
	}
}

// TestStagedHTTP exposes the exact asynchronous contract and rejects unknown JSON fields.
func TestStagedHTTP(t *testing.T) {
	service, _, _ := stagedFixture(t)
	release := testRelease()
	payload, _ := json.Marshal(map[string]string{"version": release.Version, "digest": release.Digest, "confirmation": "更新 " + release.Version})
	for _, path := range []string{"/download", "/activate"} {
		invalid := httptest.NewRecorder()
		service.Handler().ServeHTTP(invalid, httptest.NewRequest("POST", path, strings.NewReader(`{"url":"bad"}`)))
		if invalid.Code != 409 {
			t.Fatal("invalid JSON accepted")
		}
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest("POST", path, strings.NewReader(string(payload))))
		if response.Code != 202 {
			t.Fatalf("route failed: %s %s", path, response.Body.String())
		}
		service.Wait()
	}
}

// TestMeasuredPullProgress never treats a single layer's size as the whole image percentage.
func TestMeasuredPullProgress(t *testing.T) {
	var updates []DownloadProgress
	scanPullProgress(strings.NewReader("abc123: Downloading [==>] 1MB/2MB\rdef456: Downloading [>] 500kB/1MB\n"), func(progress DownloadProgress) { updates = append(updates, progress) })
	if len(updates) != 2 || updates[1].DownloadedBytes != 1500000 || updates[1].TotalBytes != 0 {
		t.Fatalf("incorrect measured output: %+v", updates)
	}
	service, _, _ := stagedFixture(t)
	service.downloadProgress(updates[1])
	if service.Status().DownloadPercent != nil {
		t.Fatal("fabricated image percentage")
	}
}
