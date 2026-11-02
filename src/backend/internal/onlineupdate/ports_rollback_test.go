package onlineupdate

import "testing"

// TestPortRollbackRestartKeepsRestoredPolicy checks that coordinator rollback commits
// the restored port revision before restart, avoiding a false manual-recovery mismatch.
func TestPortRollbackRestartKeepsRestoredPolicy(t *testing.T) {
	service, host, portStore := portService(t)
	initial := service.Ports()
	request := PortRequest{Ports: []int{3306, 13306}, Revision: initial.Revision, Confirmation: "应用端口变更"}
	if err := service.ApplyPorts(request); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	applied := service.Ports()
	if applied.State != "idle" || portList(applied.AppliedPorts) != "3306,13306" {
		t.Fatalf("apply failed: %+v", applied)
	}
	if err := service.Rollback("回退转发程序"); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	restored := service.Ports()
	if restored.State != "idle" || portList(restored.AppliedPorts) != "13306" || portList(restored.ActualPorts) != "13306" {
		t.Fatalf("rollback did not restore policy: %+v", restored)
	}
	if restored.Revision == applied.Revision || portStore.status.Revision != restored.Revision {
		t.Fatal("rollback revision was not independently persisted")
	}
	if portList(portStore.status.AppliedPorts) != "13306" || portStore.status.State != "idle" {
		t.Fatalf("stale durable port state after rollback: %+v", portStore.status)
	}
	callsBeforeRestart := len(host.calls)
	restarted, err := NewService(service.store, host, &fakeSource{release: testRelease()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.ConfigurePorts(portStore); err != nil {
		t.Fatal(err)
	}
	afterRestart := restarted.Ports()
	if afterRestart.State != "idle" || !afterRestart.CanApply || !afterRestart.NeedsFinalize || !afterRestart.RulesReady {
		t.Fatalf("restart incorrectly blocked completed rollback: %+v", afterRestart)
	}
	if afterRestart.Revision != restored.Revision || portList(afterRestart.AppliedPorts) != "13306" {
		t.Fatal("restart discarded restored policy or revision")
	}
	if restarted.Status().State != "rolled_back" || restarted.Status().CanRollback {
		t.Fatalf("legacy rollback state not preserved: %+v", restarted.Status())
	}
	if len(host.calls) != callsBeforeRestart {
		t.Fatal("restart unexpectedly replayed host operation")
	}
	request.Revision = applied.Revision
	request.FinalizePrevious = true
	if restarted.ApplyPorts(request) == nil {
		t.Fatal("pre-rollback revision accepted after restart")
	}
	request.Revision = afterRestart.Revision
	request.FinalizePrevious = false
	if restarted.ApplyPorts(request) == nil {
		t.Fatal("retained rollback journal silently overwritten")
	}
}
