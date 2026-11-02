// Package onlineupdate provides a host-only, Unix-socket relay update controller.
package onlineupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const (
	Repository      = "XManCodeing/wrtsqlserver"
	Image           = "ghcr.io/xmancodeing/wrtsqlserver"
	Platform        = "linux/amd64"
	Root            = "/mnt/docker-usb/relay-updater"
	SocketPath      = Root + "/run/control.sock"
	DockerConfig    = Root + "/docker"
	TokenPath       = Root + "/github-token"
	StatePath       = Root + "/status.json"
	TransactionPath = "/mnt/docker-usb/db-relay/transaction"
)

// Config contains only trusted host configuration, never credentials.
type Config struct {
	DeployDir      string `json:"deployDir"`
	LANInterface   string `json:"lanInterface"`
	LANBindIP      string `json:"lanBindIP"`
	LANConfirmed   bool   `json:"lanConfirmed"`
	InitialVersion string `json:"initialVersion"`
}

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var interfacePattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.:-]{0,63}$`)

// versionParts parses a bounded stable semantic version; prereleases are rejected.
func versionParts(version string) ([3]uint64, error) {
	var parts [3]uint64
	if len(version) > 60 || !versionPattern.MatchString(version) {
		return parts, errors.New("invalid stable version")
	}
	for index, value := range strings.Split(version, ".") {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return parts, errors.New("version out of range")
		}
		parts[index] = parsed
	}
	return parts, nil
}

// newer reports whether candidate is strictly newer than current, rejecting invalid versions.
func newer(candidate, current string) bool {
	candidateParts, err := versionParts(candidate)
	if err != nil {
		return false
	}
	currentParts, err := versionParts(current)
	if err != nil {
		return false
	}
	for index := range candidateParts {
		if candidateParts[index] != currentParts[index] {
			return candidateParts[index] > currentParts[index]
		}
	}
	return false
}

// decodeStrict reads at most limit bytes, rejecting unknown fields and trailing JSON.
func decodeStrict(reader io.Reader, limit int64, destination any) error {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return errors.New("read failed")
	}
	if int64(len(data)) > limit {
		return errors.New("document too large")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(destination); err != nil {
		return errors.New("invalid JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

// trustedPath rejects symlinks and non-root/writable ancestors of an absolute path.
func trustedPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("unsafe path")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("untrusted ownership or permissions")
		}
		if current == "/" {
			break
		}
	}
	return nil
}

// privateRead validates a root-owned single-link 0600 regular file before bounded reading.
func privateRead(path string, limit int64) ([]byte, error) {
	if err := trustedPath(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != 0 || stat.Nlink != 1 {
		return nil, errors.New("private file permissions required")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("private file read failed")
	}
	return data, nil
}

// LoadConfig validates the trusted JSON host configuration and deployment scripts.
func LoadConfig(path string) (Config, error) {
	var config Config
	data, err := privateRead(path, 4096)
	if err != nil {
		return config, errors.New("cannot read secure host configuration")
	}
	if err = decodeStrict(strings.NewReader(string(data)), 4096, &config); err != nil {
		return config, err
	}
	if _, err = versionParts(config.InitialVersion); err != nil {
		return config, err
	}
	address := net.ParseIP(config.LANBindIP)
	if !config.LANConfirmed || !interfacePattern.MatchString(config.LANInterface) || address == nil || address.To4() == nil || !address.IsPrivate() || address.String() != config.LANBindIP {
		return config, errors.New("confirmed canonical private LAN IPv4 and interface required")
	}
	if err = validateDeployment(config.DeployDir); err != nil {
		return config, err
	}
	if err = trustedPath(DockerConfig); err != nil {
		return config, errors.New("private Docker configuration directory required")
	}
	info, err := os.Stat(DockerConfig)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return config, errors.New("Docker configuration must be root 0700")
	}
	return config, nil
}

// validateDeployment checks the trusted directory and all directly executed/sourced helpers.
func validateDeployment(directory string) error {
	for _, name := range []string{"", "common.sh", "relay-update.sh", "updater-preflight.sh", "updater-start.sh", "ports-common.sh", "lan-rules.sh", "legacy-lan-validate.awk", "lan-manage.sh", "lan-restore.sh", "updater-service.sh", "relay-updater", "deploy-stat"} {
		if name == "deploy-stat" {
			if _, err := os.Lstat(filepath.Join(directory, name)); os.IsNotExist(err) && systemStatAvailable() {
				continue
			}
		}
		path := filepath.Join(directory, name)
		if err := trustedPath(path); err != nil {
			return fmt.Errorf("untrusted deployment helper: %s", name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return errors.New("deployment helper unavailable")
		}
		if name == "" && !info.IsDir() || name != "" && !info.Mode().IsRegular() {
			return errors.New("invalid deployment helper type")
		}
		if (name == "relay-updater" || name == "deploy-stat") && info.Mode().Perm()&0100 == 0 {
			return errors.New("deployment binary is not executable")
		}
		if name != "" && info.Sys().(*syscall.Stat_t).Nlink != 1 {
			return errors.New("hardlinked deployment helper rejected")
		}
	}
	return nil
}

// systemStatAvailable checks only the same fixed system PATH used by the scripts.
// Trusted system symlinks (for example BusyBox applets) are resolved before validation.
func systemStatAvailable() bool {
	for _, directory := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		resolved, err := filepath.EvalSymlinks(filepath.Join(directory, "stat"))
		if err != nil || trustedPath(resolved) != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return true
		}
	}
	return false
}

// Release is the small authenticated release manifest exposed to the web client.
type Release struct {
	Version  string `json:"version"`
	Image    string `json:"image"`
	Digest   string `json:"digest"`
	Platform string `json:"platform"`
}

// validateRelease rejects any registry, platform, digest or version outside policy.
func validateRelease(release Release) error {
	return validateReleaseFor(release, DefaultSource())
}
