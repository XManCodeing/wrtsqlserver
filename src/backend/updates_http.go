package main

import (
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"opernwrtdocker/backend/internal/updatepkg"
)

const updateUploadTimeout = 5 * time.Minute

// handleUpdates dispatches authenticated update preparation routes; it never applies an update.
func (app *App) handleUpdates(writer http.ResponseWriter, request *http.Request) {
	manager := app.updates
	if request.URL.Path == "/api/updates" && request.Method == http.MethodGet {
		writeJSON(writer, 200, manager.status())
		return
	}
	if request.URL.Path != "/api/updates/package" {
		apiError(writer, 404, "更新接口不存在")
		return
	}
	if request.Method != http.MethodPost && request.Method != http.MethodDelete {
		writer.Header().Set("Allow", "POST, DELETE")
		apiError(writer, 405, "更新包接口仅支持上传与删除")
		return
	}
	if !manager.enabled {
		apiError(writer, 503, manager.reason)
		return
	}
	select {
	case manager.slot <- struct{}{}:
		defer func() { <-manager.slot }()
	default:
		writer.Header().Set("Retry-After", "5")
		apiError(writer, 429, "另一个更新包操作正在进行中")
		return
	}
	if request.Method == http.MethodDelete {
		manager.deletePackage(writer)
		return
	}
	manager.uploadPackage(writer, request)
}

// deletePackage removes only the fixed prepared package while the caller owns the mutation semaphore.
func (manager *updateManager) deletePackage(writer http.ResponseWriter) {
	err := os.Remove(filepath.Join(manager.directory, updatePackageName))
	if err != nil && !os.IsNotExist(err) {
		apiError(writer, 500, "无法删除已准备的更新包")
		return
	}
	manager.mu.Lock()
	manager.summary = nil
	manager.mu.Unlock()
	manager.syncDirectory()
	log.Print("prepared update package removed")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusNoContent)
}

// failureReader preserves transport and size errors otherwise hidden by strict format validation.
type failureReader struct {
	reader  io.Reader
	failure error
}

// Read delegates to the bounded body and records non-EOF failures for HTTP classification.
func (reader *failureReader) Read(data []byte) (int, error) {
	count, err := reader.reader.Read(data)
	if err != nil && err != io.EOF {
		reader.failure = err
	}
	return count, err
}

// uploadPackage applies a finite per-request deadline, total byte bound and disk preflight before staging.
func (manager *updateManager) uploadPackage(writer http.ResponseWriter, request *http.Request) {
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != "application/octet-stream" {
		apiError(writer, 415, "请以 application/octet-stream 上传原始更新包")
		return
	}
	if request.ContentLength > updatepkg.DefaultMaxBytes {
		apiError(writer, 413, "更新包超过 256 MiB 限制")
		return
	}
	controller := http.NewResponseController(writer)
	if err := controller.SetReadDeadline(time.Now().Add(updateUploadTimeout)); err != nil {
		apiError(writer, 503, "当前服务不支持安全上传读取超时控制")
		return
	}
	if err := controller.SetWriteDeadline(time.Now().Add(updateUploadTimeout + 20*time.Second)); err != nil {
		apiError(writer, 503, "当前服务不支持安全上传响应超时控制")
		return
	}
	defer func() {
		if err := controller.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
			log.Print("update read deadline reset failed")
		}
		if err := controller.SetWriteDeadline(time.Now().Add(20 * time.Second)); err != nil {
			log.Print("update write deadline reset failed")
		}
	}()
	needed := request.ContentLength
	if needed < 0 {
		needed = updatepkg.DefaultMaxBytes
	}
	free, err := manager.freeSpace(manager.directory)
	if err != nil {
		apiError(writer, 503, "无法检查暂存磁盘空间")
		return
	}
	if free < uint64(needed)+updateReserveBytes {
		apiError(writer, 507, "暂存磁盘空间不足，必须保留 64 MiB 可用空间")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, updatepkg.DefaultMaxBytes)
	reader := &failureReader{reader: request.Body}
	summary, err := manager.savePackage(reader)
	if err != nil {
		log.Print("update package upload or validation failed")
		var tooLarge *http.MaxBytesError
		var timeout net.Error
		switch {
		case errors.As(reader.failure, &tooLarge):
			apiError(writer, 413, "更新包超过 256 MiB 限制")
		case errors.As(reader.failure, &timeout) && timeout.Timeout():
			apiError(writer, 408, "更新包上传超时")
		case errors.Is(err, errUpdateSpace):
			apiError(writer, 507, "暂存磁盘空间不足")
		default:
			apiError(writer, 400, "更新包无效或无法保存：请检查签名、格式、平台、摘要和磁盘状态")
		}
		return
	}
	writeJSON(writer, 200, summary)
}
