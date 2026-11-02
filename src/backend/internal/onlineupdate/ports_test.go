package onlineupdate

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// portMemoryStore models independent port persistence and disk failure without filesystem effects.
type portMemoryStore struct {
	status PortStatus
	exists bool
	fail   bool
}

func (store *portMemoryStore) LoadPorts() (PortStatus, error) {
	if !store.exists {
		return PortStatus{}, os.ErrNotExist
	}
	return store.status, nil
}
func (store *portMemoryStore) SavePorts(status PortStatus) error {
	if store.fail {
		return errors.New("disk full")
	}
	store.status = status
	store.exists = true
	return nil
}

// portFakeHost serializes fake host observations while async apply runs; never executes system commands.
type portFakeHost struct {
	*fakeHost
	mu            sync.Mutex
	ports         []int
	originalPorts []int
	gate          chan struct{}
	failure       bool
	recovery      bool
	missing       bool
}

func (host *portFakeHost) InspectPorts(context.Context) ([]int, bool, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.missing {
		return nil, false, errors.New("initial container missing")
	}
	return append([]int{}, host.ports...), true, nil
}
func (host *portFakeHost) HasTransaction() bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.transaction
}
func (host *portFakeHost) Marker(name string) bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.fakeHost.Marker(name)
}
func (host *portFakeHost) Script(ctx context.Context, mode string, release Release) error {
	host.mu.Lock()
	defer host.mu.Unlock()
	err := host.fakeHost.Script(ctx, mode, release)
	if err == nil && mode == "rollback" && host.originalPorts != nil {
		host.ports = append([]int{}, host.originalPorts...)
	}
	return err
}
func (host *portFakeHost) PortScript(_ context.Context, mode string, ports []int) error {
	if mode == "ports-apply" && host.gate != nil {
		<-host.gate
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	host.calls = append(host.calls, mode)
	if mode == "ports-apply" {
		host.transaction = true
		if host.failure {
			host.rolledBack = host.recovery
			return errors.New("failed")
		}
		host.originalPorts = append([]int{}, host.ports...)
		host.ports = append([]int{}, ports...)
		host.applied = true
	}
	return nil
}

// portService creates a configured coordinator with mock host and independent memory stores.
func portService(t *testing.T) (*Service, *portFakeHost, *portMemoryStore) {
	t.Helper()
	host := &portFakeHost{fakeHost: &fakeHost{current: "1.0.0"}, ports: []int{13306}}
	service, err := NewService(&memoryStore{}, host, &fakeSource{release: testRelease()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &portMemoryStore{}
	if err = service.ConfigurePorts(store); err != nil {
		t.Fatal(err)
	}
	return service, host, store
}

// TestMissingInitialContainerKeepsCapabilities serves capability/status while first container is absent.
func TestMissingInitialContainerKeepsCapabilities(t *testing.T) {
	host := &portFakeHost{fakeHost: &fakeHost{currentErr: true}, missing: true}
	service, err := NewService(&memoryStore{}, host, &fakeSource{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.ConfigurePorts(&portMemoryStore{}); err != nil {
		t.Fatal(err)
	}
	status := service.Ports()
	if !status.Supported || status.CanApply || status.State != "failed" {
		t.Fatal(status)
	}
	for _, path := range []string{"/capabilities", "/ports", "/status"} {
		writer := httptest.NewRecorder()
		service.Handler().ServeHTTP(writer, httptest.NewRequest("GET", path, nil))
		if writer.Code != 200 {
			t.Fatalf("missing container blocked %s: %d", path, writer.Code)
		}
	}
}

// TestPortPolicy covers reserved, low, duplicate, nil, zero and maximum business sets.
func TestPortPolicy(t *testing.T) {
	for _, ports := range [][]int{nil, {6379}, {8080}, {18080}, {1023}, {65536}, {3306, 3306}, make([]int, 65)} {
		if _, err := ValidatePorts(ports); err == nil {
			t.Fatalf("accepted %v", ports)
		}
	}
	for _, ports := range [][]int{{}, {65535, 1024}, {3306, 13306}} {
		if _, err := ValidatePorts(ports); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPortsSharedCoordinator rejects stale requests and every concurrent legacy host mutation.
func TestPortsSharedCoordinator(t *testing.T) {
	service, host, _ := portService(t)
	initial := service.Ports()
	request := PortRequest{Ports: []int{3306, 13306}, Revision: "stale", Confirmation: "应用端口变更"}
	if service.ApplyPorts(request) == nil {
		t.Fatal("stale accepted")
	}
	request.Revision = initial.Revision
	host.gate = make(chan struct{})
	if err := service.ApplyPorts(request); err != nil {
		t.Fatal(err)
	}
	if service.Check() == nil || service.ApplyPorts(request) == nil {
		t.Fatal("concurrent operation accepted")
	}
	close(host.gate)
	service.Wait()
	status := service.Ports()
	if status.State != "idle" || status.Revision == initial.Revision || !status.NeedsFinalize {
		t.Fatalf("bad completion: %+v", status)
	}
	request.Revision = status.Revision
	if service.ApplyPorts(request) == nil {
		t.Fatal("silently finalized rollback window")
	}
	request.FinalizePrevious = true
	if err := service.ApplyPorts(request); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	for _, call := range host.calls {
		if call == "pull" {
			t.Fatal("port operation pulled image")
		}
	}
}

// TestPortFailureAndRestart keeps failed recovery and interrupted state blocked across restart.
func TestPortFailureAndRestart(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		service, host, store := portService(t)
		host.failure = true
		host.recovery = recovered
		request := PortRequest{Ports: []int{3306, 13306}, Revision: service.Ports().Revision, Confirmation: "应用端口变更"}
		if err := service.ApplyPorts(request); err != nil {
			t.Fatal(err)
		}
		service.Wait()
		want := "manual_recovery"
		if recovered {
			want = "failed"
		}
		if service.Ports().State != want {
			t.Fatal(service.Ports())
		}
		store.status.State = "applying"
		if err := service.ConfigurePorts(store); err != nil {
			t.Fatal(err)
		}
		if service.Ports().State != "manual_recovery" || service.Check() == nil {
			t.Fatal("interruption retried")
		}
	}
}

// TestPortHTTP checks capability/versioned JSON and strict request boundaries without a socket.
func TestPortHTTP(t *testing.T) {
	service, _, _ := portService(t)
	for _, path := range []string{"/ports", "/capabilities"} {
		writer := httptest.NewRecorder()
		service.Handler().ServeHTTP(writer, httptest.NewRequest("GET", path, nil))
		if writer.Code != 200 {
			t.Fatal(writer.Body.String())
		}
	}
	writer := httptest.NewRecorder()
	service.Handler().ServeHTTP(writer, httptest.NewRequest("POST", "/ports/apply", strings.NewReader(`{"ports":[],"revision":"x","confirmation":"应用端口变更","unexpected":true}`)))
	if writer.Code != 409 {
		t.Fatal("unknown fields accepted")
	}
}

// TestPortPersistenceFailure prevents host mutation when durable intent cannot be saved.
func TestPortPersistenceFailure(t *testing.T) {
	service, host, store := portService(t)
	revision := service.Ports().Revision
	store.fail = true
	if service.ApplyPorts(PortRequest{Ports: []int{}, Revision: revision, Confirmation: "应用端口变更"}) == nil {
		t.Fatal("disk failure accepted")
	}
	if len(host.calls) != 0 || service.Check() == nil {
		t.Fatal("effects after persistence failure")
	}
}
