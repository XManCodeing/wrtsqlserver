package onlineupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RuntimeContract is the mandatory image label value for Ubuntu-compatible releases.
// Published OCI versions remain stable; current identities may retain the approved bootstrap prerelease.
const RuntimeContract = "portable-v1"

var composeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// RuntimeRoot returns host-specific private state storage for trusted configuration.
func (config Config) RuntimeRoot() string {
	if config.HostMode == "ubuntu" {
		return filepath.Join(config.ProjectDir, "data/updater")
	}
	return Root
}

// validateUbuntuConfig checks root-owned paths without OpenWrt/UUID/Passwall assumptions.
// config comes from private JSON; errors block all Docker and filesystem mutations.
func validateUbuntuConfig(config Config) error {
	if !composeNamePattern.MatchString(config.ComposeProject) || !composeNamePattern.MatchString(config.RelayService) || strings.HasPrefix(config.RelayService, "redis") {
		return errors.New("explicit Compose project and non-redis relay service required")
	}
	if !filepath.IsAbs(config.ProjectDir) || filepath.Clean(config.ProjectDir) != config.ProjectDir || config.ProjectDir == "/" {
		return errors.New("absolute canonical project directory required")
	}
	if filepath.Dir(config.ComposeFile) != config.ProjectDir || strings.HasPrefix(filepath.Base(config.ComposeFile), ".env") {
		return errors.New("Compose file must be directly inside project directory")
	}
	for _, path := range []string{config.ProjectDir, config.ComposeFile, config.DeployDir, filepath.Join(config.DeployDir, "ubuntu-update.py"), config.RuntimeRoot(), filepath.Join(config.RuntimeRoot(), "docker")} {
		if err := trustedPath(path); err != nil {
			return errors.New("untrusted Ubuntu deployment path")
		}
	}
	for _, path := range []string{config.ComposeFile, filepath.Join(config.DeployDir, "ubuntu-update.py")} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("Ubuntu deployment file unavailable")
		}
	}
	for _, path := range []string{config.RuntimeRoot(), filepath.Join(config.RuntimeRoot(), "docker")} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return errors.New("Ubuntu updater state and Docker config require root 0700")
		}
	}
	return nil
}

// UbuntuHost adapts the existing staged-update service to one explicitly selected Compose service.
// The helper owns consistent backups and recovery; no Docker socket enters the business container.
type UbuntuHost struct {
	Config Config
	Runner Runner
}

// dockerHost shares immutable pull/progress/OCI verification with the existing implementation.
func (host *UbuntuHost) dockerHost() *DockerHost {
	return &DockerHost{Config: host.Config, Runner: host.Runner}
}

// helper invokes a fixed root-trusted Python entrypoint using validated host-only arguments.
// mode is internal; release identity has already passed the source policy.
func (host *UbuntuHost) helper(ctx context.Context, mode string, release Release) ([]byte, error) {
	if err := validateUbuntuConfig(host.Config); err != nil {
		return nil, err
	}
	args := []string{filepath.Join(host.Config.DeployDir, "ubuntu-update.py"), mode,
		"--project-dir", host.Config.ProjectDir, "--compose-file", host.Config.ComposeFile,
		"--project", host.Config.ComposeProject, "--service", host.Config.RelayService,
		"--initial-version", host.Config.InitialVersion}
	if release.Version != "" {
		args = append(args, "--image", release.Image+"@"+release.Digest, "--version", release.Version)
	}
	return host.Runner.Run(ctx, "/usr/bin/python3", args, host.dockerHost().environment(Release{}))
}

// Current returns the managed running container version, with an explicit source-build fallback.
func (host *UbuntuHost) Current(ctx context.Context) (string, error) {
	data, err := host.helper(ctx, "current", Release{})
	if err != nil {
		return "", errors.New("cannot inspect Ubuntu relay identity")
	}
	version := strings.TrimSpace(string(data))
	if _, _, err = currentVersionParts(version); err != nil {
		return "", err
	}
	return version, nil
}

// Preflight verifies Ubuntu deployment policy without modifying containers or updater state.
func (host *UbuntuHost) Preflight(ctx context.Context) error {
	_, err := host.helper(ctx, "preflight", Release{})
	return err
}

// Script accepts only existing service transaction verbs and validates release identity.
func (host *UbuntuHost) Script(ctx context.Context, mode string, release Release) error {
	switch mode {
	case "check", "apply", "rollback", "finalize":
	default:
		return errors.New("unsupported Ubuntu update command")
	}
	if release.Version != "" {
		source, err := contextSource(ctx)
		if err != nil {
			return err
		}
		if err = validateReleaseFor(release, source); err != nil {
			return err
		}
	}
	_, err := host.helper(ctx, mode, release)
	return err
}

// Pull downloads and verifies an approved digest without stopping the current service.
func (host *UbuntuHost) Pull(ctx context.Context, release Release) error {
	return host.PullWithProgress(ctx, release, nil)
}

// PullWithProgress reuses the measured Docker downloader and Ubuntu preflight.
func (host *UbuntuHost) PullWithProgress(ctx context.Context, release Release, report func(DownloadProgress)) error {
	return host.dockerHost().PullWithProgress(ctx, release, report)
}

// VerifyPrepared reuses digest, platform, version and portable-contract validation.
func (host *UbuntuHost) VerifyPrepared(ctx context.Context, release Release) error {
	return host.dockerHost().VerifyPrepared(ctx, release)
}

// Marker reads only known durable private completion markers from this deployment.
func (host *UbuntuHost) Marker(name string) bool {
	if name != "applied" && name != "rolled-back" {
		return false
	}
	data, err := privateRead(filepath.Join(host.Config.RuntimeRoot(), "transaction", name), 16)
	return err == nil && strings.TrimSpace(string(data)) == "yes"
}

// HasTransaction conservatively preserves any retained or unsafe transaction entry.
func (host *UbuntuHost) HasTransaction() bool {
	return transactionExists(filepath.Join(host.Config.RuntimeRoot(), "transaction"))
}
