package onlineupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Runner executes an internal fixed command with a sanitized environment.
type Runner interface {
	Run(context.Context, string, []string, []string) ([]byte, error)
}

// ExecRunner runs a command group; cancellation terminates every descendant.
type ExecRunner struct{}

// limitedOutput retains bounded command output and discards excess without blocking.
type limitedOutput struct{ data []byte }

// Write returns the consumed byte count while retaining at most 64KiB.
func (output *limitedOutput) Write(data []byte) (int, error) {
	count := len(data)
	remaining := 65536 - len(output.data)
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		output.data = append(output.data, data...)
	}
	return count, nil
}

// Run executes a fixed executable, discarding stderr so credentials cannot leak into logs.
func (ExecRunner) Run(ctx context.Context, name string, args, environment []string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 5 * time.Second
	output := &limitedOutput{}
	command.Stdout = output
	command.Stderr = io.Discard
	err := command.Run()
	if err != nil {
		return nil, errors.New("host command failed")
	}
	return output.data, nil
}

// Host abstracts all host effects, allowing tests without a Docker daemon.
type Host interface {
	Current(context.Context) (string, error)
	Pull(context.Context, Release) error
	Script(context.Context, string, Release) error
	Marker(string) bool
	HasTransaction() bool
}

// DockerHost implements the fixed Docker socket and trusted shell command policy.
type DockerHost struct {
	Config Config
	Runner Runner
	// trustCheck is test-only injection; production always validates root-owned helpers.
	trustCheck func(string) error
}

// trustedDeployment validates deployment paths unless an in-package test substitutes a checker.
func (host *DockerHost) trustedDeployment() error {
	if host.trustCheck != nil {
		return host.trustCheck(host.Config.DeployDir)
	}
	return validateDeployment(host.Config.DeployDir)
}

// environment builds a minimal environment without inherited proxy, Docker context or secrets.
func (host *DockerHost) environment(release Release) []string {
	values := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + Root, "DOCKER_CONFIG=" + DockerConfig, "LAN_INTERFACE=" + host.Config.LANInterface, "LAN_BIND_IP=" + host.Config.LANBindIP, "LAN_INTERFACE_CONFIRMED=yes"}
	if release.Version != "" {
		values = append(values, "RELAY_IMAGE="+release.Image+":"+release.Version, "REQUIRE_ONLINE_UPDATE=yes")
	}
	return values
}

// docker always pins the daemon socket and never accepts web-provided command arguments.
func (host *DockerHost) docker(ctx context.Context, args ...string) ([]byte, error) {
	return host.Runner.Run(ctx, "/usr/bin/docker", append([]string{"--host", "unix:///var/run/docker.sock"}, args...), host.environment(Release{}))
}

// Current reads the running container OCI version; fallback applies only to a missing label.
func (host *DockerHost) Current(ctx context.Context) (string, error) {
	data, err := host.docker(ctx, "inspect", "--format", `{"labels":{{json .Config.Labels}},"running":{{json .State.Running}}}`, "db-relay")
	if err != nil {
		return "", errors.New("cannot inspect current relay version")
	}
	var container struct {
		Labels  map[string]string `json:"labels"`
		Running bool              `json:"running"`
	}
	if json.Unmarshal(data, &container) != nil || !container.Running || container.Labels["io.db-relay.deploy.managed"] != "relay-v1" {
		return "", errors.New("current relay must be managed and running")
	}
	version := container.Labels["org.opencontainers.image.version"]
	if version == "" {
		version = host.Config.InitialVersion
	}
	if _, err = versionParts(version); err != nil {
		return "", errors.New("current relay version is invalid")
	}
	return version, nil
}

// Script validates helpers on every invocation and accepts only the documented subcommands.
func (host *DockerHost) Script(ctx context.Context, mode string, release Release) error {
	switch mode {
	case "check", "apply", "rollback", "finalize":
	default:
		return errors.New("unsupported update command")
	}
	source, sourceErr := contextSource(ctx)
	if sourceErr != nil {
		return sourceErr
	}
	if release.Version != "" {
		if err := validateReleaseFor(release, source); err != nil {
			return err
		}
	}
	if err := host.trustedDeployment(); err != nil {
		return errors.New("deployment trust check failed")
	}
	environment := host.environment(release)
	if mode == "rollback" {
		environment = append(environment, "STALE_LOCK_CONFIRMED=yes")
	}
	_, err := host.Runner.Run(ctx, "/bin/sh", []string{filepath.Join(host.Config.DeployDir, "relay-update.sh"), mode}, environment)
	return err
}

// Preflight validates trusted dependencies and the mounted USB before any host writes.
// It is also repeated before every image pull; the shell check never mutates host state.
func (host *DockerHost) Preflight(ctx context.Context) error {
	if err := host.trustedDeployment(); err != nil {
		return errors.New("deployment trust check failed")
	}
	if _, err := host.Runner.Run(ctx, "/bin/sh", []string{filepath.Join(host.Config.DeployDir, "updater-preflight.sh")}, host.environment(Release{})); err != nil {
		return errors.New("host preflight failed; no updater files or images may be created")
	}
	return nil
}

// Pull preflights disk space, pulls an immutable reference, verifies platform/digest, then tags.
func (host *DockerHost) Pull(ctx context.Context, release Release) error {
	source, sourceErr := contextSource(ctx)
	if sourceErr != nil {
		return sourceErr
	}
	if err := validateReleaseFor(release, source); err != nil {
		return err
	}
	if err := host.Preflight(ctx); err != nil {
		return err
	}
	reference := release.Image + "@" + release.Digest
	if _, err := host.docker(ctx, "pull", reference); err != nil {
		return errors.New("image download failed")
	}
	data, err := host.docker(ctx, "image", "inspect", "--format", `{{json .}}`, reference)
	if err != nil {
		return errors.New("image verification failed")
	}
	var image struct {
		OS           string `json:"Os"`
		Architecture string
		RepoDigests  []string
		Config       struct{ Labels map[string]string }
	}
	if json.Unmarshal(data, &image) != nil || image.OS+"/"+image.Architecture != Platform {
		return errors.New("image platform rejected")
	}
	if image.Config.Labels["org.opencontainers.image.version"] != release.Version {
		return errors.New("image OCI version does not match release")
	}
	found := false
	for _, digest := range image.RepoDigests {
		if digest == reference {
			found = true
		}
	}
	if !found {
		return errors.New("image digest verification failed")
	}
	if _, err = host.docker(ctx, "tag", reference, release.Image+":"+release.Version); err != nil {
		return errors.New("image tagging failed")
	}
	return nil
}

// Marker reads only known root-private transaction completion markers.
func (host *DockerHost) Marker(name string) bool {
	if name != "applied" && name != "rolled-back" {
		return false
	}
	data, err := privateRead(filepath.Join(TransactionPath, name), 16)
	return err == nil && strings.TrimSpace(string(data)) == "yes"
}

// HasTransaction conservatively reports unsafe or existing transaction paths as recovery required.
func (host *DockerHost) HasTransaction() bool { return transactionExists(TransactionPath) }
