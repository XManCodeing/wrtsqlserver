package main

import (
	"crypto/ed25519"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"opernwrtdocker/backend/internal/updatepkg"
)

const updateReserveBytes uint64 = 64 << 20
const updatePackageName = "package.tar"

// UpdatePackage is the public summary of a fully authenticated, durable package.
type UpdatePackage struct {
	Version    string    `json:"version"`
	Platform   string    `json:"platform"`
	ImageTag   string    `json:"imageTag"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploadedAt"`
}

// UpdateStatus describes preparation-only capability, never privileged execution.
type UpdateStatus struct {
	CurrentVersion string         `json:"currentVersion"`
	UploadEnabled  bool           `json:"uploadEnabled"`
	ApplyEnabled   bool           `json:"applyEnabled"`
	MaxUploadBytes int64          `json:"maxUploadBytes"`
	Reason         string         `json:"reason"`
	Package        *UpdatePackage `json:"package"`
}

// updateManager owns its independent mutation semaphore and a short-lived summary lock.
type updateManager struct {
	mu        sync.RWMutex
	slot      chan struct{}
	directory string
	public    ed25519.PublicKey
	enabled   bool
	reason    string
	summary   *UpdatePackage
	freeSpace func(string) (uint64, error)
}

// newUpdateManager takes explicit environment paths, enabling uploads only after safe initialization.
// Invalid configuration disables uploads with a nonsecret Chinese reason instead of exposing paths.
func newUpdateManager(publicPath, directory string) *updateManager {
	manager := &updateManager{slot: make(chan struct{}, 1), freeSpace: availableUpdateSpace,
		reason: "更新上传未启用：必须同时明确配置公钥文件和暂存目录。"}
	if publicPath == "" || directory == "" {
		return manager
	}
	if !filepath.IsAbs(publicPath) || !filepath.IsAbs(directory) || filepath.Clean(directory) == string(filepath.Separator) {
		manager.reason = "更新上传未启用：公钥文件和暂存目录必须使用有效的绝对路径。"
		return manager
	}
	public, err := readUpdatePublicKey(publicPath)
	if err != nil {
		manager.reason = "更新上传未启用：无法读取有效的 Ed25519 公钥文件。"
		return manager
	}
	if err = prepareUpdateDirectory(directory); err != nil {
		manager.reason = "更新上传未启用：暂存目录不可用或权限不安全。"
		return manager
	}
	manager.public, manager.directory = public, directory
	if _, err = manager.freeSpace(directory); err != nil {
		manager.reason = "更新上传未启用：无法检查暂存磁盘的可用空间。"
		return manager
	}
	manager.enabled = true
	manager.reason = "仅支持上传、校验与准备；首次部署并手动回退验证成功前，不开放执行更新。"
	manager.restore()
	return manager
}

// readUpdatePublicKey reads exactly 32 raw Ed25519 public-key bytes from a regular, nonsymlink file.
func readUpdatePublicKey(path string) (ed25519.PublicKey, error) {
	file, err := openUpdateRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, ed25519.PublicKeySize+1))
	if err != nil || len(data) != ed25519.PublicKeySize {
		return nil, errors.New("invalid public key")
	}
	return ed25519.PublicKey(data), nil
}

// prepareUpdateDirectory creates an explicitly configured private staging directory, rejecting symlink ancestors.
func prepareUpdateDirectory(directory string) error {
	for current := filepath.Clean(directory); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("unsafe staging directory")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	return os.Chmod(directory, 0700)
}

// openUpdateRegular opens only a nonsymlink regular file, checking the opened inode against lstat.
func openUpdateRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, errors.New("file changed during open")
	}
	return file, nil
}

// updateSummary maps a verified manifest and durable timestamp to nonsecret API metadata.
func updateSummary(manifest updatepkg.Manifest, uploaded time.Time) *UpdatePackage {
	return &UpdatePackage{Version: manifest.Version, Platform: manifest.Platform, ImageTag: manifest.ImageTag,
		SHA256: manifest.ImageSHA256, Size: manifest.ImageSize, UploadedAt: uploaded.UTC()}
}

// restore revalidates the persisted package from scratch; untrusted or partial packages are never advertised.
func (manager *updateManager) restore() {
	file, err := openUpdateRegular(filepath.Join(manager.directory, updatePackageName))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		log.Print("update package recovery rejected unsafe file")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > updatepkg.DefaultMaxBytes {
		log.Print("update package recovery rejected size")
		return
	}
	manifest, err := updatepkg.Verify(io.LimitReader(file, updatepkg.DefaultMaxBytes+1), manager.public, updatepkg.DefaultMaxBytes)
	if err != nil {
		log.Print("update package recovery validation failed")
		return
	}
	manager.summary = updateSummary(manifest, info.ModTime())
	log.Print("update package recovery validation succeeded")
}

// status returns a value copy under the dedicated lock, without acquiring App.mu.
func (manager *updateManager) status() UpdateStatus {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	status := UpdateStatus{CurrentVersion: version, UploadEnabled: manager.enabled, MaxUploadBytes: updatepkg.DefaultMaxBytes, Reason: manager.reason}
	if manager.summary != nil {
		copy := *manager.summary
		status.Package = &copy
	}
	return status
}

// stageWriter bounds each disk write with a fresh filesystem reserve check.
type stageWriter struct {
	file    *os.File
	failure error
	manager *updateManager
}

var errUpdateSpace = errors.New("insufficient update staging space")

// Write preserves reserve space before writing data, returning a safe disk error on failure.
func (writer *stageWriter) Write(data []byte) (int, error) {
	free, err := writer.manager.freeSpace(writer.manager.directory)
	if err != nil {
		return 0, errors.New("cannot check update staging space")
	}
	if free < updateReserveBytes+uint64(len(data)) {
		writer.failure = errUpdateSpace
		return 0, errUpdateSpace
	}
	return writer.file.Write(data)
}

// savePackage streams input into a private temporary file while verifying it, atomically replacing only on success.
// Existing valid data survives all validation failures; caller owns the mutation semaphore.
func (manager *updateManager) savePackage(reader io.Reader) (*UpdatePackage, error) {
	file, err := os.CreateTemp(manager.directory, ".upload-*")
	if err != nil {
		return nil, errors.New("cannot create update temporary file")
	}
	temporary := file.Name()
	defer func() {
		file.Close()
		if err := os.Remove(temporary); err != nil && !os.IsNotExist(err) {
			log.Print("update temporary cleanup failed")
		}
	}()
	staging := &stageWriter{file: file, manager: manager}
	manifest, err := updatepkg.Verify(io.TeeReader(reader, staging), manager.public, updatepkg.DefaultMaxBytes)
	if staging.failure != nil {
		return nil, staging.failure
	}
	if err != nil {
		return nil, err
	}
	if err = file.Sync(); err != nil {
		return nil, errors.New("cannot sync update package")
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("cannot inspect update package")
	}
	if err = file.Close(); err != nil {
		return nil, errors.New("cannot close update package")
	}
	if err = os.Rename(temporary, filepath.Join(manager.directory, updatePackageName)); err != nil {
		return nil, errors.New("cannot commit update package")
	}
	summary := updateSummary(manifest, info.ModTime())
	manager.mu.Lock()
	manager.summary = summary
	manager.mu.Unlock()
	manager.syncDirectory()
	log.Print("update package validated and prepared; execution disabled")
	return summary, nil
}

// syncDirectory flushes rename/delete metadata and logs a nonsecret failure if the filesystem refuses.
func (manager *updateManager) syncDirectory() {
	directory, err := os.Open(manager.directory)
	if err != nil {
		log.Print("update directory sync open failed")
		return
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		log.Print("update directory sync failed")
	}
}
