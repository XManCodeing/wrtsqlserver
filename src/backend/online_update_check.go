package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"opernwrtdocker/backend/internal/onlineupdate"
)

// onlineCheckStatus extends the host schema without granting installation capabilities.
type onlineCheckStatus struct {
	onlineupdate.Status
	CheckOnly bool `json:"checkOnly"`
}

// onlineReleaseChecker keeps only a process-local public release snapshot, never host state.
type onlineReleaseChecker struct {
	mu     sync.Mutex
	source onlineupdate.ReleaseSource
	status onlineCheckStatus
}

var publicOnlineChecker = newOnlineReleaseChecker(version)

// newOnlineReleaseChecker accepts the running build version and returns an anonymous fixed-source checker.
// GitHub.Latest supplies the shared release, image, platform, redirect and bounded-response validation.
func newOnlineReleaseChecker(current string) *onlineReleaseChecker {
	github := onlineupdate.NewGitHub()
	github.Token = func() (string, error) { return "", nil }
	github.Client.Timeout = 12 * time.Second
	github.Client.Transport = &http.Transport{Proxy: nil, MaxResponseHeaderBytes: 8 << 10, DisableKeepAlives: true}
	return &onlineReleaseChecker{source: github, status: onlineCheckStatus{
		Status: onlineupdate.Status{CurrentVersion: current, State: "idle", Phase: "idle",
			Message: "可检测公开正式版本；下载、重启和回退需要连接宿主更新服务。"},
		CheckOnly: true,
	}}
}

// snapshot returns a detached cached status; it performs no network requests or host operations.
func (checker *onlineReleaseChecker) snapshot() onlineCheckStatus {
	checker.mu.Lock()
	defer checker.mu.Unlock()
	return checker.snapshotLocked()
}

// snapshotLocked returns a release copy while the caller owns mu, preventing shared pointer mutation.
func (checker *onlineReleaseChecker) snapshotLocked() onlineCheckStatus {
	status := checker.status
	if status.Latest != nil {
		release := *status.Latest
		status.Latest = &release
	}
	return status
}

// start begins one explicit bounded check and returns its accepted snapshot plus whether it started.
// Concurrent requests reuse the in-flight result instead of multiplying outbound GitHub requests.
func (checker *onlineReleaseChecker) start() (onlineCheckStatus, bool) {
	checker.mu.Lock()
	defer checker.mu.Unlock()
	if checker.status.State == "checking" {
		return checker.snapshotLocked(), false
	}
	checker.status.State, checker.status.Phase = "checking", "check"
	checker.status.Message = "正在检测公开正式版本"
	checker.status.Latest = nil
	checker.status.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	log.Print("public online release check started")
	go checker.check()
	return checker.snapshotLocked(), true
}

// check resolves only the fixed public source using existing GitHub validation and a total deadline.
// Its terminal snapshot never enables apply, download, activation or rollback.
func (checker *onlineReleaseChecker) check() {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	release, err := checker.source.Latest(ctx)
	checker.mu.Lock()
	defer checker.mu.Unlock()
	checker.status.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	checker.status.Phase = "ready"
	if err != nil {
		checker.status.State, checker.status.Phase = "failed", "check"
		checker.status.Message = "检测失败：公开发布信息不可用、请求受限或未通过校验，请稍后重试。"
		log.Print("public online release check failed")
		return
	}
	checker.status.Latest = &release
	newer, comparable := onlineReleaseNewer(release.Version, checker.status.CurrentVersion)
	switch {
	case !comparable:
		checker.status.State = "idle"
		checker.status.Message = "已读取最新正式版本，当前构建版本无法比较；不会执行更新。"
	case newer:
		checker.status.State = "available"
		checker.status.Message = "发现新版本；连接宿主更新服务后才能下载并更新。"
	default:
		checker.status.State = "idle"
		checker.status.Message = "未发现比当前构建更新的正式版本。"
	}
	log.Print("public online release check completed")
}

// onlineReleaseNewer shares the host updater policy for stable releases and the approved bootstrap.
// candidate/current are version identities; results indicate upgrade eligibility and safe comparability.
func onlineReleaseNewer(candidate, current string) (bool, bool) {
	return onlineupdate.ReleaseNewer(candidate, current)
}

// handleReadOnlyOnline serves cached status/fixed source and explicit checks when no socket is configured.
// writer receives the capability contract; request/action have already passed authentication and route validation.
func handleReadOnlyOnline(writer http.ResponseWriter, request *http.Request, action string) {
	switch {
	case action == "/status":
		writeJSON(writer, http.StatusOK, publicOnlineChecker.snapshot())
	case action == "/source" && request.Method == http.MethodGet:
		writeJSON(writer, http.StatusOK, onlineupdate.DefaultSource())
	case action == "/check":
		if _, valid := onlineRequestBody(writer, request, action); !valid {
			return
		}
		status, started := publicOnlineChecker.start()
		if !started {
			apiError(writer, http.StatusConflict, "检测正在执行，请等待结果。")
			return
		}
		writeJSON(writer, http.StatusAccepted, status)
	default:
		apiError(writer, http.StatusServiceUnavailable, "缺少宿主更新服务；当前仅支持检测公开正式版本，更新来源只读。")
	}
}
