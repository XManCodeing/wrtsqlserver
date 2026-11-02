package onlineupdate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"sync"
	"time"
)

// ReleaseSource obtains only the latest approved stable release.
type ReleaseSource interface {
	Latest(context.Context) (Release, error)
}

// Service serializes asynchronous operations and persists their intent before execution.
type Service struct {
	mu        sync.Mutex
	record    Record
	busy      bool
	broken    bool
	store     Store
	host      Host
	source    ReleaseSource
	logger    *log.Logger
	portStore PortStore
	ports     PortStatus
}

// NewService restores state; interrupted or unrecognized transactions require manual action.
func NewService(store Store, host Host, source ReleaseSource, logger *log.Logger) (*Service, error) {
	service := &Service{store: store, host: host, source: source, logger: logger}
	record, err := store.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("updater state unavailable or unsafe; manual review required")
	}
	if errors.Is(err, os.ErrNotExist) {
		record.Status = Status{State: "idle", Phase: "idle", Enabled: true, Message: "尚未检测更新"}
	}
	if record.Source == (Source{}) {
		record.Source = DefaultSource()
	}
	normalized, sourceErr := normalizeSource(record.Source)
	if sourceErr != nil {
		return nil, errors.New("persisted update source violates policy")
	}
	record.Source = normalized
	if record.Status.Latest != nil && validateReleaseFor(*record.Status.Latest, record.Source) != nil {
		return nil, errors.New("persisted release violates policy; manual review required")
	}
	service.record = record
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	current, inspectErr := host.Current(ctx)
	if inspectErr == nil {
		service.record.Status.CurrentVersion = current
	}
	hasTransaction := host.HasTransaction()
	interrupted := activeState(record.Status.State) || record.Status.State == "manual_recovery"
	if interrupted || hasTransaction && (!record.Status.CanRollback && record.Status.State != "rolled_back" || inspectErr != nil) {
		service.record.Status.State = "manual_recovery"
		service.record.Status.Phase = "recovery"
		service.record.Status.Message = "发现未完成操作或无法确认事务运行状态；不会自动重试，请核查事务后明确回退"
		service.record.Status.CanRollback = hasTransaction
	} else if inspectErr != nil {
		service.record.Status.State = "failed"
		service.record.Status.Phase = "version"
		service.record.Status.CurrentVersion = ""
		service.record.Status.Latest = nil
		service.record.Status.CanRollback = false
		service.record.Status.Message = "尚未确认受管转发容器正在运行；容器就绪后可手动重新检测"
	}
	service.record.Status.Enabled = true
	if err = service.persistLocked(); err != nil {
		return nil, err
	}
	return service, nil
}

// Status returns a detached snapshot and never performs network or host operations.
func (service *Service) Status() Status {
	service.mu.Lock()
	defer service.mu.Unlock()
	status := service.record.Status
	if status.Latest != nil {
		release := *status.Latest
		status.Latest = &release
	}
	return status
}

// persistLocked writes before effects; a persistence failure permanently blocks mutations.
func (service *Service) persistLocked() error {
	status := &service.record.Status
	status.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	status.CanApply = !service.busy && !service.broken && service.ports.State != "manual_recovery" && status.State != "manual_recovery" && status.Latest != nil && newer(status.Latest.Version, status.CurrentVersion)
	if err := service.store.Save(service.record); err != nil {
		service.broken = true
		service.busy = false
		status.State = "manual_recovery"
		status.Phase = "persistence"
		status.CanApply = false
		status.Enabled = false
		status.Message = "状态无法安全落盘；已停止更新操作，需要宿主人工检查"
		service.log("state persistence failed")
		return errors.New("状态无法安全保存，操作已拒绝")
	}
	return nil
}

// log emits only static safe operational summaries, never command output or credentials.
func (service *Service) log(message string) {
	if service.logger != nil {
		service.logger.Print(message)
	}
}

// beginLocked reserves an operation and durably records its intent before launching work.
func (service *Service) beginLocked(state, phase, message string) error {
	if service.busy || service.broken || service.ports.State == "manual_recovery" && phase != "rollback" {
		return errors.New("已有操作正在执行或需要宿主恢复")
	}
	var identifier [16]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		return errors.New("无法创建安全操作标识")
	}
	service.busy = true
	service.record.Status.OperationID = hex.EncodeToString(identifier[:])
	service.record.Status.State = state
	service.record.Status.Phase = phase
	service.record.Status.Message = message
	if err := service.persistLocked(); err != nil {
		service.busy = false
		return err
	}
	service.log("operation started: " + phase)
	return nil
}

// Check explicitly requests a fresh release lookup; it is never called by status polling.
func (service *Service) Check() error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.record.Status.State == "manual_recovery" {
		return errors.New("请先处理未完成事务")
	}
	if err := service.beginLocked("checking", "check", "正在检测正式版本"); err != nil {
		return err
	}
	go service.check()
	return nil
}

// check refreshes the running version and cached release using a bounded independent context.
func (service *Service) check() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	current, err := service.host.Current(ctx)
	if err != nil {
		service.mu.Lock()
		service.busy = false
		service.record.Status.CurrentVersion = ""
		service.record.Status.Latest = nil
		if service.host.HasTransaction() {
			service.record.Status.CanRollback = true
			service.completeLocked("manual_recovery", "version", "无法确认保留事务的运行容器；请核查事务后明确回退")
		} else {
			service.completeLocked("failed", "version", "尚未确认受管转发容器正在运行；容器就绪后可手动重新检测")
		}
		service.mu.Unlock()
		return
	}
	ctx = sourceContext(ctx, service.Source())
	release, err := service.source.Latest(ctx)
	service.mu.Lock()
	defer service.mu.Unlock()
	service.busy = false
	if err != nil {
		service.record.Status.Latest = nil
		service.completeLocked("failed", "check", "检测失败；请检查宿主配置及私有仓库读取权限")
		return
	}
	if validateReleaseFor(release, service.record.Source) != nil {
		service.record.Status.Latest = nil
		service.completeLocked("failed", "check", "发布信息未通过安全校验")
		return
	}
	service.record.Status.CurrentVersion = current
	service.record.Status.Latest = &release
	if newer(release.Version, current) {
		service.completeLocked("available", "ready", "发现可用正式版本")
	} else {
		service.completeLocked("idle", "ready", "当前已是最新版本")
	}
}

// Apply validates cached identity and exact human confirmation; no arbitrary version is accepted.
func (service *Service) Apply(version, digest, confirmation string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	status := service.record.Status
	if status.State == "manual_recovery" || status.Latest == nil || version != status.Latest.Version || digest != status.Latest.Digest || confirmation != "更新 "+version || !newer(version, status.CurrentVersion) {
		return errors.New("更新确认或缓存版本不匹配")
	}
	release := *status.Latest
	if validateReleaseFor(release, service.record.Source) != nil {
		return errors.New("发布信息无效")
	}
	if err := service.beginLocked("downloading", "download", "正在进行磁盘预检和镜像下载"); err != nil {
		return err
	}
	go service.apply(release)
	return nil
}

// stage persists a phase transition; false means callers must stop without further effects.
func (service *Service) stage(phase, message string) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.record.Status.Phase = phase
	service.record.Status.Message = message
	if phase != "download" {
		service.record.Status.State = "applying"
	}
	return service.persistLocked() == nil
}

// finish releases the operation reservation and persists a safe public summary.
func (service *Service) finish(state, phase, message string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.busy = false
	service.completeLocked(state, phase, message)
}

// completeLocked updates terminal state while preserving rollback metadata.
func (service *Service) completeLocked(state, phase, message string) {
	service.record.Status.State = state
	service.record.Status.Phase = phase
	service.record.Status.Message = message
	if service.persistLocked() != nil {
		service.log("operation completion could not be persisted")
		return
	}
	service.log("operation completed: " + state)
}

// apply separates download, update and recovery deadlines and never applies after pull failure.
func (service *Service) apply(release Release) {
	downloadCtx, cancelDownload := context.WithTimeout(sourceContext(context.Background(), service.Source()), 10*time.Minute)
	current, err := service.host.Current(downloadCtx)
	if err != nil || !newer(release.Version, current) {
		cancelDownload()
		service.mu.Lock()
		service.record.Status.CurrentVersion = current
		service.mu.Unlock()
		service.finish("failed", "version", "无法确认升级版本比当前运行版本更新")
		return
	}
	err = service.host.Pull(downloadCtx, release)
	cancelDownload()
	if err != nil {
		service.finish("failed", "download", "下载或磁盘预检失败；未替换运行容器")
		return
	}
	ctx, cancel := context.WithTimeout(sourceContext(context.Background(), service.Source()), 3*time.Minute)
	defer cancel()
	if !service.prepareTransaction(ctx, release, current) {
		return
	}
	if err = service.host.Script(ctx, "check", release); err != nil {
		service.finish("failed", "preflight", "部署检查失败；未替换运行容器")
		return
	}
	if !service.stage("apply", "正在替换转发容器并检查健康状态") {
		return
	}
	err = service.host.Script(ctx, "apply", release)
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		service.mu.Lock()
		service.record.Status.CanRollback = service.host.HasTransaction()
		service.mu.Unlock()
		service.finish("manual_recovery", "timeout", "更新执行超时；不假定容器或回退状态，请核查事务后明确回退")
		return
	}
	if err != nil {
		service.recoverFailure()
		return
	}
	confirmed, verifyErr := service.host.Current(ctx)
	if verifyErr != nil || confirmed != release.Version || ctx.Err() != nil || !service.host.Marker("applied") {
		service.mu.Lock()
		service.record.Status.CanRollback = service.host.HasTransaction()
		service.mu.Unlock()
		service.finish("manual_recovery", "verify", "缺少受信成功标记；请核查事务后回退")
		return
	}
	service.mu.Lock()
	service.record.Status.CurrentVersion = release.Version
	service.record.Status.CanRollback = true
	service.mu.Unlock()
	service.finish("succeeded", "complete", "更新成功，已保留旧容器与一致性备份，可明确回退")
}

// prepareTransaction archives only after explicit apply and preserves the prior version durably.
func (service *Service) prepareTransaction(ctx context.Context, release Release, current string) bool {
	if service.host.HasTransaction() {
		if !service.host.Marker("applied") && !service.host.Marker("rolled-back") {
			service.finish("manual_recovery", "recovery", "存在未完成事务，禁止覆盖；请人工检查并回退")
			return false
		}
		if !service.stage("finalize", "正在归档上次已完成事务，保留数据备份") {
			return false
		}
		if err := service.host.Script(ctx, "finalize", release); err != nil {
			service.finish("manual_recovery", "finalize", "归档失败，未开始新更新；请人工检查保留事务")
			return false
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.record.PreviousVersion = current
	service.record.Status.CurrentVersion = current
	service.record.Status.CanRollback = false
	return service.persistLocked() == nil
}

// recoverFailure distinguishes shell auto-recovery from an interrupted or unsafe transaction.
func (service *Service) recoverFailure() {
	if service.host.Marker("rolled-back") {
		service.markRolledBack()
		return
	}
	if !service.host.HasTransaction() {
		service.finish("failed", "apply", "部署未完成，尚未创建回退事务")
		return
	}
	service.mu.Lock()
	service.record.Status.CanRollback = true
	service.mu.Unlock()
	service.finish("manual_recovery", "recovery", "更新失败且未确认自动回退成功；请核查事务后明确回退，禁止重复更新")
}

// Rollback requires a deliberate confirmation and retained transaction; it is never automatic on restart.
func (service *Service) Rollback(confirmation string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if confirmation != "回退转发程序" || !service.record.Status.CanRollback {
		return errors.New("回退确认无效或无可回退事务")
	}
	if err := service.beginLocked("applying", "rollback", "正在恢复上个转发版本和一致性数据备份"); err != nil {
		return err
	}
	go service.rollback()
	return nil
}

// rollback has its own three-minute budget and requires the script's durable completion marker.
func (service *Service) rollback() {
	ctx, cancel := context.WithTimeout(sourceContext(context.Background(), service.Source()), 3*time.Minute)
	defer cancel()
	if err := service.host.Script(ctx, "rollback", Release{}); err != nil || ctx.Err() != nil || !service.host.Marker("rolled-back") {
		service.finish("manual_recovery", "rollback", "回退未确认成功；保留全部事务数据，请宿主人工检查")
		return
	}
	service.markRolledBack()
}

// markRolledBack restores public version metadata only after a trusted recovery marker.
func (service *Service) markRolledBack() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	current, err := service.host.Current(ctx)
	cancel()
	service.mu.Lock()
	if err == nil {
		service.record.Status.CurrentVersion = current
	} else {
		service.record.Status.CurrentVersion = ""
		service.mu.Unlock()
		service.finish("manual_recovery", "version", "回退标记已保存，但无法确认受管容器正在运行；请宿主人工检查")
		return
	}
	if service.portStore != nil {
		portHost := service.host.(PortHost)
		portCtx, portCancel := context.WithTimeout(context.Background(), 15*time.Second)
		actual, ready, portErr := portHost.InspectPorts(portCtx)
		portCancel()
		if portErr != nil {
			service.ports.State = "manual_recovery"
			service.ports.Message = "回退容器端口无法核验"
		} else {
			service.ports.State = "idle"
			service.ports.AppliedPorts = actual
			service.ports.ActualPorts = actual
			service.ports.RulesReady = ready
			service.ports.Revision = portRevision(actual, service.record.Status.OperationID)
		}
		if service.savePortsLocked() != nil {
			service.mu.Unlock()
			return
		}
	}
	service.record.Status.CanRollback = false
	service.mu.Unlock()
	service.finish("rolled_back", "complete", "已回退到上个转发版本；新版本数据与事务备份仍保留")
}
