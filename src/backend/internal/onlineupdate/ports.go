package onlineupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"strings"
	"time"
)

// PortStatus is independent of the legacy strict update document. Ports exclude management/Redis.
type PortStatus struct {
	Supported     bool   `json:"supported"`
	SchemaVersion int    `json:"schemaVersion"`
	Revision      string `json:"revision"`
	AppliedPorts  []int  `json:"appliedPorts"`
	ActualPorts   []int  `json:"actualPorts"`
	RulesReady    bool   `json:"rulesReady"`
	State         string `json:"state"`
	OperationID   string `json:"operationId"`
	Message       string `json:"message"`
	CanApply      bool   `json:"canApply"`
	NeedsFinalize bool   `json:"needsFinalize"`
	RequiredPorts []int  `json:"requiredPorts,omitempty"`
}

// PortRequest carries a bounded desired set and an explicit rollback-window acknowledgement.
type PortRequest struct {
	Ports            []int  `json:"ports"`
	Revision         string `json:"revision"`
	Confirmation     string `json:"confirmation"`
	FinalizePrevious bool   `json:"finalizePrevious"`
}

// PortHost extends the host without forcing legacy integrations to implement port management.
type PortHost interface {
	InspectPorts(context.Context) ([]int, bool, error)
	PortScript(context.Context, string, []int) error
}

// PortStore persists a separate root-private document; implementations must save atomically.
type PortStore interface {
	LoadPorts() (PortStatus, error)
	SavePorts(PortStatus) error
}

// PortFileStore stores only control state, never files in the application's /data directory.
type PortFileStore struct{ Path string }

// LoadPorts returns a strictly decoded bounded document or a filesystem/validation error.
func (store PortFileStore) LoadPorts() (PortStatus, error) {
	var status PortStatus
	data, err := privateRead(store.Path, 16384)
	if err == nil {
		err = decodeStrict(strings.NewReader(string(data)), 16384, &status)
	}
	return status, err
}

// SavePorts atomically persists status and fsyncs its private parent directory.
func (store PortFileStore) SavePorts(status PortStatus) error {
	return savePrivateJSON(store.Path, status)
}

// ValidatePorts returns a sorted detached set; duplicates and reserved ports are rejected.
func ValidatePorts(ports []int) ([]int, error) {
	if ports == nil || len(ports) > 64 {
		return nil, errors.New("端口集合必须提供，且最多64个")
	}
	result := append([]int{}, ports...)
	sort.Ints(result)
	for index, port := range result {
		if port < 1024 || port > 65535 || port == 6379 || port == 8080 || port == 18080 || index > 0 && result[index-1] == port {
			return nil, errors.New("业务端口必须为1024..65535且不能重复或使用6379、8080、18080")
		}
	}
	return result, nil
}

// ConfigurePorts restores a versioned independent state; interrupted effects are never retried.
func (service *Service) ConfigurePorts(store PortStore) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	host, ok := service.host.(PortHost)
	if !ok {
		return errors.New("host does not support ports")
	}
	status, err := store.LoadPorts()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("端口状态不可安全读取")
	}
	if err == nil {
		if status.SchemaVersion != 1 || status.Revision == "" || !status.Supported {
			return errors.New("未知端口状态版本")
		}
		if _, validateErr := ValidatePorts(status.AppliedPorts); validateErr != nil {
			return validateErr
		}
		if _, validateErr := ValidatePorts(status.ActualPorts); validateErr != nil {
			return validateErr
		}
		switch status.State {
		case "idle", "applying", "failed", "manual_recovery":
		default:
			return errors.New("未知端口事务状态")
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		status = PortStatus{Supported: true, SchemaVersion: 1, State: "idle", Revision: "initial", AppliedPorts: []int{}, ActualPorts: []int{}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	actual, ready, inspectErr := host.InspectPorts(ctx)
	if status.State == "applying" || status.State == "manual_recovery" {
		status.State = "manual_recovery"
		status.Message = "端口事务中断，保留现场，需要宿主人工恢复"
	} else if inspectErr != nil {
		status.State = "failed"
		status.Message = "无法确认受管容器端口及LAN规则；禁止应用"
	} else {
		status.ActualPorts = actual
		status.RulesReady = ready
		if status.Revision == "initial" {
			status.AppliedPorts = append([]int{}, actual...)
			status.Revision = portRevision(actual, "initial")
		} else if portList(status.AppliedPorts) != portList(actual) {
			status.State = "manual_recovery"
			status.Message = "持久端口状态与实际发布不一致；保留现场，需要人工核查"
		}
	}
	service.portStore = store
	service.ports = status
	if status.State == "manual_recovery" {
		service.record.Status.CanApply = false
		service.record.Status.CanRollback = service.host.HasTransaction()
	}
	return service.savePortsLocked()
}

// portRevision ties optimistic concurrency to a committed operation and canonical port set.
func portRevision(ports []int, operation string) string {
	digest := sha256.Sum256([]byte(operation + ":" + portList(ports)))
	return hex.EncodeToString(digest[:])
}

// savePortsLocked makes persistence failure a coordinator-wide mutation barrier.
func (service *Service) savePortsLocked() error {
	if err := service.portStore.SavePorts(service.ports); err != nil {
		service.broken = true
		service.ports.State = "manual_recovery"
		service.ports.CanApply = false
		service.log("port state persistence failed; all operations blocked")
		return errors.New("端口状态无法安全保存，需人工恢复")
	}
	return nil
}

// Ports returns current host observations; reads never rewrite firewall rules or publish ports.
func (service *Service) Ports() PortStatus {
	service.mu.Lock()
	defer service.mu.Unlock()
	result := service.ports
	if service.busy && result.State != "manual_recovery" {
		result.State = "applying"
		result.Message = "宿主事务正在执行，配置写入暂时冻结"
	}
	result.AppliedPorts = append([]int{}, result.AppliedPorts...)
	result.ActualPorts = append([]int{}, result.ActualPorts...)
	if baseline, ok := service.host.(interface{ RequiredPorts() []int }); ok {
		result.RequiredPorts = baseline.RequiredPorts()
	}
	if service.portStore == nil {
		return result
	}
	host := service.host.(PortHost)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	actual, ready, err := host.InspectPorts(ctx)
	if err == nil {
		result.ActualPorts = actual
		result.RulesReady = ready
	} else {
		result.RulesReady = false
		result.Message = "宿主端口布局核验失败"
	}
	if err == nil && !ready && !service.busy {
		result.Message = "LAN规则尚未显式迁移或顺序核验失败，禁止应用端口"
	}
	retained := service.host.HasTransaction()
	result.NeedsFinalize = retained && (service.host.Marker("applied") || service.host.Marker("rolled-back"))
	result.CanApply = err == nil && ready && !service.busy && !service.broken && result.State != "manual_recovery" && service.record.Status.State != "manual_recovery" && (!retained || result.NeedsFinalize)
	return result
}

// ApplyPorts reserves the same coordinator as updates before any asynchronous host mutation.
func (service *Service) ApplyPorts(request PortRequest) error {
	ports, err := ValidatePorts(request.Ports)
	if err != nil {
		return err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.portStore == nil || service.ports.State == "manual_recovery" || service.record.Status.State == "manual_recovery" {
		return errors.New("端口能力不可用或存在未完成事务")
	}
	if request.Revision != service.ports.Revision || request.Confirmation != "应用端口变更" {
		return errors.New("端口修订已过期或确认不匹配")
	}
	if service.host.HasTransaction() && (!request.FinalizePrevious || (!service.host.Marker("applied") && !service.host.Marker("rolled-back"))) {
		return errors.New("需显式结束旧回退窗口；未完成事务不能归档")
	}
	if err = service.beginLocked("applying", "apply", "正在应用端口变更"); err != nil {
		return err
	}
	service.ports.State = "applying"
	service.ports.OperationID = service.record.Status.OperationID
	service.ports.Message = "正在预检、同镜像重建和核验LAN规则"
	if err = service.savePortsLocked(); err != nil {
		service.busy = false
		return err
	}
	go service.applyPorts(ports, request.FinalizePrevious)
	return nil
}

// applyPorts uses the existing shell transaction/rollback and never pulls a new image.
func (service *Service) applyPorts(ports []int, finalize bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	host := service.host.(PortHost)
	err := host.PortScript(ctx, "ports-check", ports)
	preflightFailed := err != nil
	if err == nil && service.host.HasTransaction() && finalize {
		err = service.host.Script(ctx, "finalize", Release{})
	}
	if err == nil {
		err = host.PortScript(ctx, "ports-check", ports)
	}
	if err == nil {
		err = host.PortScript(ctx, "ports-apply", ports)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.busy = false
	if err != nil {
		service.ports.State = "failed"
		service.ports.Message = "端口应用失败；未变更或已恢复原容器及规则"
		if ctx.Err() != nil || !preflightFailed && service.host.HasTransaction() && !service.host.Marker("rolled-back") {
			service.ports.State = "manual_recovery"
			service.ports.Message = "恢复未确认成功，保留现场，请宿主人工检查"
		}
	} else {
		actual, ready, verifyErr := host.InspectPorts(ctx)
		if verifyErr != nil || !ready || portList(actual) != portList(ports) || !service.host.Marker("applied") {
			service.ports.State = "manual_recovery"
			service.ports.Message = "缺少可信端口或LAN成功状态，需人工恢复"
		} else {
			service.ports.State = "idle"
			service.ports.Message = "端口已应用；原容器和一致性备份已保留"
			service.ports.AppliedPorts = ports
			service.ports.ActualPorts = actual
			service.ports.RulesReady = true
			service.ports.Revision = portRevision(ports, service.ports.OperationID)
		}
	}
	if service.savePortsLocked() != nil {
		return
	}
	service.record.Status.CanRollback = service.host.HasTransaction()
	state := "idle"
	if service.ports.State != "idle" {
		state = service.ports.State
	}
	service.completeLocked(state, "complete", service.ports.Message)
}
