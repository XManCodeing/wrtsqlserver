package onlineupdate

import (
	"context"
	"errors"
	"time"
)

// preparedHost verifies a local image without a network pull; old Host mocks remain compatible.
type preparedHost interface {
	VerifyPrepared(context.Context, Release) error
}

// progressHost adds measured pull progress without extending the mandatory Host interface.
type progressHost interface {
	PullWithProgress(context.Context, Release, func(DownloadProgress)) error
}

// clearPreparedLocked invalidates staged identity/progress without changing rollback metadata.
func (service *Service) clearPreparedLocked() {
	service.record.Status.Prepared = nil
	service.record.PreparedSource = Source{}
	service.record.Status.DownloadPercent = nil
	service.record.Status.DownloadedBytes = 0
	service.record.Status.TotalBytes = 0
}

// validateUpdateLocked checks the exact cached identity and legacy machine confirmation field.
// version/digest are user input; a returned release is approved for the current source only.
func (service *Service) validateUpdateLocked(version, digest, confirmation string) (Release, error) {
	status := service.record.Status
	if status.State == "manual_recovery" || status.Latest == nil || version != status.Latest.Version || digest != status.Latest.Digest || confirmation != "更新 "+version || !newer(version, status.CurrentVersion) {
		return Release{}, errors.New("更新确认或缓存版本不匹配")
	}
	release := *status.Latest
	if err := validateReleaseFor(release, service.record.Source); err != nil {
		return Release{}, errors.New("发布信息无效")
	}
	return release, nil
}

// Download reserves an isolated pull; it never finalizes transactions or replaces the running container.
// version/digest identify the checked release; confirmation is supplied by the UI button for compatibility.
func (service *Service) Download(version, digest, confirmation string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	release, err := service.validateUpdateLocked(version, digest, confirmation)
	if err != nil {
		return err
	}
	if service.busy || service.broken {
		return errors.New("已有操作正在执行或需要宿主恢复")
	}
	if _, ok := service.host.(preparedHost); !ok {
		return errors.New("宿主不支持分阶段更新")
	}
	service.clearPreparedLocked()
	service.record.DownloadOnly = true
	if err = service.beginLocked("downloading", "download", "正在下载更新，当前服务保持运行"); err != nil {
		return err
	}
	go service.download(release)
	return nil
}

// download validates the running version and stores prepared identity only after pull verification succeeds.
func (service *Service) download(release Release) {
	ctx, cancel := context.WithTimeout(sourceContext(context.Background(), service.Source()), 10*time.Minute)
	defer cancel()
	current, err := service.host.Current(ctx)
	if err != nil || !newer(release.Version, current) {
		service.finish("failed", "version", "无法确认升级版本比当前运行版本更新")
		return
	}
	ctx = context.WithValue(ctx, pullPhaseKey{}, func() {
		service.mu.Lock()
		defer service.mu.Unlock()
		service.record.Status.Phase = "verify"
		service.record.Status.Message = "正在校验下载的镜像"
	})
	if host, ok := service.host.(progressHost); ok {
		err = host.PullWithProgress(ctx, release, service.downloadProgress)
	} else {
		err = service.host.Pull(ctx, release)
	}
	if err == nil {
		err = service.host.(preparedHost).VerifyPrepared(ctx, release)
	}
	if err != nil {
		service.finish("failed", "download", "下载或镜像校验失败；当前服务未改变")
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.record.Status.Prepared = &release
	service.record.PreparedSource = service.record.Source
	service.record.Status.CurrentVersion = current
	service.record.DownloadOnly = false
	service.busy = false
	service.completeLocked("downloaded", "downloaded", "下载完成，可稍后或立即重启生效")
}

// downloadProgress updates an in-memory snapshot from real transfer data, with no fabricated percentage.
// progress totals must describe the whole transfer; unknown totals produce a JSON null percentage.
func (service *Service) downloadProgress(progress DownloadProgress) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if progress.DownloadedBytes < 0 || progress.TotalBytes < 0 || progress.TotalBytes > 0 && progress.DownloadedBytes > progress.TotalBytes {
		return
	}
	status := &service.record.Status
	status.DownloadedBytes, status.TotalBytes = progress.DownloadedBytes, progress.TotalBytes
	status.DownloadPercent = nil
	if progress.TotalBytes > 0 {
		percent := float64(progress.DownloadedBytes) * 100 / float64(progress.TotalBytes)
		status.DownloadPercent = &percent
	}
}

// Activate requires a prepared release from the same source and reserves the existing apply transaction.
// Identity is checked synchronously; running version and local image are checked again before effects.
func (service *Service) Activate(version, digest, confirmation string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	release, err := service.validateUpdateLocked(version, digest, confirmation)
	if err != nil {
		return err
	}
	if service.record.Status.Prepared == nil || *service.record.Status.Prepared != release || service.record.PreparedSource != service.record.Source {
		return errors.New("下载结果已失效，请重新检测并下载")
	}
	if _, ok := service.host.(preparedHost); !ok {
		return errors.New("宿主不支持分阶段更新")
	}
	if err = service.beginLocked("applying", "verify", "正在核验已下载镜像与当前运行版本"); err != nil {
		return err
	}
	go service.activate(release)
	return nil
}

// activate checks local immutable content without network pull, then reuses deployment and recovery.
func (service *Service) activate(release Release) {
	ctx, cancel := context.WithTimeout(sourceContext(context.Background(), service.Source()), time.Minute)
	current, err := service.host.Current(ctx)
	if err == nil && newer(release.Version, current) {
		err = service.host.(preparedHost).VerifyPrepared(ctx, release)
	} else {
		err = errors.New("running version changed")
	}
	cancel()
	service.mu.Lock()
	service.clearPreparedLocked()
	service.record.Status.CurrentVersion = current
	service.mu.Unlock()
	if err != nil {
		service.finish("failed", "verify", "下载结果或当前版本已改变，请重新检测并下载")
		return
	}
	service.deploy(release, current)
}

// restorePrepared validates persisted downloads after restart without resuming any interrupted mutation.
// ctx is bounded by startup; invalid/missing images are discarded and may be downloaded again.
func (service *Service) restorePrepared(ctx context.Context, current string) {
	status := &service.record.Status
	if service.record.DownloadOnly && status.State == "downloading" {
		service.clearPreparedLocked()
		service.record.DownloadOnly = false
		status.State, status.Phase, status.Message = "failed", "download", "下载被中断，可重新下载；当前服务未改变"
	}
	if status.Prepared == nil {
		return
	}
	host, ok := service.host.(preparedHost)
	if !ok || service.record.PreparedSource != service.record.Source || status.Latest == nil || *status.Latest != *status.Prepared || !newer(status.Prepared.Version, current) || host.VerifyPrepared(sourceContext(ctx, service.record.Source), *status.Prepared) != nil {
		service.clearPreparedLocked()
		if status.State == "downloaded" {
			status.State, status.Phase, status.Message = "failed", "download", "已下载镜像不可用，请重新下载"
		}
	}
}
