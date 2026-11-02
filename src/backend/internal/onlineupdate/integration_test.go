package onlineupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInitialContainerAbsenceRecovers permits manual retry after first relay creation.
func TestInitialContainerAbsenceRecovers(t *testing.T) {
	host := &fakeHost{current: "1.0.0", currentErr: true}
	source := &fakeSource{release: testRelease()}
	service := newTestService(t, host, source, &memoryStore{})
	if service.Status().State != "failed" || service.Status().CanApply {
		t.Fatal("first startup must not require manual recovery")
	}
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if source.calls != 0 || service.Status().CurrentVersion != "" {
		t.Fatal("absent container treated as running")
	}
	host.currentErr = false
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if service.Status().State != "available" || !service.Status().CanApply || source.calls != 1 {
		t.Fatal("initial creation did not recover")
	}
}

// TestStoppedContainerNeverSucceeds verifies durable markers cannot disguise a stopped relay.
func TestStoppedContainerNeverSucceeds(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		host := &fakeHost{current: "1.0.0", stopAfterApply: !rollback, stopAfterRollback: rollback}
		service := newTestService(t, host, &fakeSource{release: testRelease()}, &memoryStore{})
		if err := service.Check(); err != nil {
			t.Fatal(err)
		}
		service.Wait()
		release := testRelease()
		if err := service.Apply(release.Version, release.Digest, "更新 "+release.Version); err != nil {
			t.Fatal(err)
		}
		service.Wait()
		if rollback {
			if err := service.Rollback("回退转发程序"); err != nil {
				t.Fatal(err)
			}
			service.Wait()
		}
		if service.Status().State != "manual_recovery" {
			t.Fatalf("stopped relay reported success: %+v", service.Status())
		}
	}
}

// timeoutHost simulates a killed shell with an already-visible marker; success must not be inferred.
type timeoutHost struct{ *fakeHost }

func (host timeoutHost) Script(ctx context.Context, mode string, release Release) error {
	if mode == "apply" {
		host.transaction = true
		host.applied = true
		return context.DeadlineExceeded
	}
	return host.fakeHost.Script(ctx, mode, release)
}

// TestApplyTimeoutRemainsManual rejects an ambiguous timeout even when an applied marker exists.
func TestApplyTimeoutRemainsManual(t *testing.T) {
	host := timeoutHost{&fakeHost{current: "1.0.0"}}
	service, err := NewService(&memoryStore{}, host, &fakeSource{release: testRelease()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	release := testRelease()
	if err = service.Apply(release.Version, release.Digest, "更新 "+release.Version); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if service.Status().State != "manual_recovery" || service.Status().Phase != "timeout" || !service.Status().CanRollback {
		t.Fatal("timeout reported success")
	}
}

// inspectRunner emits precisely the compact non-secret inspect response used by Current.
type inspectRunner struct{ response string }

func (runner inspectRunner) Run(context.Context, string, []string, []string) ([]byte, error) {
	return []byte(runner.response), nil
}

// TestCurrentRequiresManagedRunning rejects unmanaged/stopped containers and tests initial fallback.
func TestCurrentRequiresManagedRunning(t *testing.T) {
	for _, test := range []struct {
		response string
		valid    bool
	}{
		{`{"running":true,"labels":{"io.db-relay.deploy.managed":"relay-v1","org.opencontainers.image.version":"1.0.0"}}`, true},
		{`{"running":true,"labels":{"io.db-relay.deploy.managed":"relay-v1"}}`, true},
		{`{"running":false,"labels":{"io.db-relay.deploy.managed":"relay-v1"}}`, false},
		{`{"running":true,"labels":{"org.opencontainers.image.version":"1.0.0"}}`, false},
		{`{"running":true,"labels":{"io.db-relay.deploy.managed":"evil"}}`, false},
	} {
		host := &DockerHost{Config: Config{InitialVersion: "1.0.0"}, Runner: inspectRunner{test.response}}
		version, err := host.Current(context.Background())
		if (err == nil) != test.valid || test.valid && version != "1.0.0" {
			t.Fatal("current running identity policy failed")
		}
	}
}

// TestPreflightFailsBeforeCommands proves failed trust checks prevent even shell execution.
func TestPreflightFailsBeforeCommands(t *testing.T) {
	runner := &commandRunner{}
	host := &DockerHost{Runner: runner, trustCheck: func(string) error { return errors.New("untrusted helper") }}
	if host.Preflight(context.Background()) == nil || len(runner.calls) != 0 {
		t.Fatal("untrusted helper executed")
	}
	host.trustCheck = func(string) error { return nil }
	runner.failPreflight = true
	if host.Preflight(context.Background()) == nil || len(runner.calls) != 1 {
		t.Fatal("preflight failure ignored")
	}
}

// TestUnixHTTPStatusIntegration uses an actual Unix listener with an injected temporary path.
// The client matches the application's Unix HTTP transport and validates the public schema.
func TestUnixHTTPStatusIntegration(t *testing.T) {
	// macOS Unix socket paths are short; never use the fixed production path in tests.
	directory, err := os.MkdirTemp("/tmp", "relay-unix-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	service := newTestService(t, &fakeHost{current: "1.0.0"}, &fakeSource{release: testRelease()}, &memoryStore{})
	server := &http.Server{Handler: service.Handler(), ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		server.Close()
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := client.Get("http://relay-updater/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var fields map[string]json.RawMessage
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&fields) != nil {
		t.Fatal("Unix status HTTP failed")
	}
	for _, field := range []string{"state", "currentVersion", "latest", "phase", "message", "operationId", "updatedAt", "canApply", "canRollback", "enabled"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("missing status field %s", field)
		}
	}
	if string(fields["currentVersion"]) != `"1.0.0"` || string(fields["latest"]) != "null" {
		t.Fatal("unexpected public state")
	}
}
