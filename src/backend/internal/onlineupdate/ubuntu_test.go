package onlineupdate

import (
	"context"
	"strings"
	"testing"
)

// portableRunner decorates existing deterministic image fixtures with the runtime contract.
type portableRunner struct {
	commandRunner
	contract string
}

// Run returns an image fixture for the supplied contract without invoking Docker.
func (runner *portableRunner) Run(ctx context.Context, name string, args, environment []string) ([]byte, error) {
	data, err := runner.commandRunner.Run(ctx, name, args, environment)
	if strings.Contains(strings.Join(args, " "), "image inspect") && runner.contract != "" {
		data = []byte(strings.Replace(string(data), `"Labels":{`, `"Labels":{"io.db-relay.runtime":"`+runner.contract+`",`, 1))
	}
	return data, err
}

// TestUbuntuReleaseContract rejects old OpenWrt images even when digest and version match.
func TestUbuntuReleaseContract(t *testing.T) {
	for _, contract := range []string{"", "openwrt-v1", RuntimeContract} {
		runner := &portableRunner{contract: contract}
		host := &UbuntuHost{Config: Config{HostMode: "ubuntu", ProjectDir: "/www/wwwroot/db-relay"}, Runner: runner}
		err := host.VerifyPrepared(context.Background(), testRelease())
		if (err == nil) != (contract == RuntimeContract) {
			t.Fatalf("contract %q verification: %v", contract, err)
		}
		if contract != RuntimeContract && len(runner.calls) != 1 {
			t.Fatal("rejected image was tagged")
		}
	}
}

// TestUbuntuRuntimePaths excludes fixed UUID/OpenWrt paths and inherited Docker environment.
func TestUbuntuRuntimePaths(t *testing.T) {
	config := Config{HostMode: "ubuntu", ProjectDir: "/www/wwwroot/db-relay"}
	if config.RuntimeRoot() != "/www/wwwroot/db-relay/data/updater" {
		t.Fatal("incorrect private state path")
	}
	environment := strings.Join((&DockerHost{Config: config}).environment(Release{}), "\n")
	if strings.Contains(environment, "/mnt/") || strings.Contains(environment, "LAN_INTERFACE") || !strings.Contains(environment, "COMPOSE_DISABLE_ENV_FILE=1") {
		t.Fatal("Ubuntu inherited OpenWrt policy")
	}
	if (Config{}).RuntimeRoot() != Root {
		t.Fatal("legacy compatibility broken")
	}
}

// TestUbuntuRejectsUnsafeConfiguration checks policy before any filesystem or Docker operation.
func TestUbuntuRejectsUnsafeConfiguration(t *testing.T) {
	for _, config := range []Config{
		{HostMode: "ubuntu", ComposeProject: "db-relay", RelayService: "redis"},
		{HostMode: "ubuntu", ComposeProject: "db-relay;sh", RelayService: "db-relay"},
		{HostMode: "ubuntu", ComposeProject: "db-relay", RelayService: "db-relay", ProjectDir: "/"},
		{HostMode: "ubuntu", ComposeProject: "db-relay", RelayService: "db-relay", ProjectDir: "/trusted/project", ComposeFile: "/other/compose.yaml"},
	} {
		if validateUbuntuConfig(config) == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
}

// TestUbuntuBootstrapVersionPolicy allows only the approved local prerelease to reach stable.
// t reports policy regressions; no filesystem, registry or host operations are performed.
func TestUbuntuBootstrapVersionPolicy(t *testing.T) {
	for _, scenario := range []struct {
		candidate, current string
		upgrade, valid     bool
	}{
		{"1.2.3", "1.2.3-ubuntu", true, true},
		{"1.2.4", "1.2.3-ubuntu", true, true},
		{"1.2.2", "1.2.3-ubuntu", false, true},
		{"1.2.3", "1.2.3", false, true},
		{"1.2.4", "1.2.3", true, true},
		{"1.2.3-ubuntu", "1.2.2", false, false},
		{"1.2.3", "1.2.3-rc1", false, false},
		{"1.2.4", "1.2.3-ubuntu.1", false, false},
		{"1.2.4", "1.2.3-ubuntu+build", false, false},
		{"1.2.3", "1.2.2-ubuntu", false, false},
		{"1.2.4", "v1.2.3-ubuntu", false, false},
	} {
		upgrade, valid := ReleaseNewer(scenario.candidate, scenario.current)
		if upgrade != scenario.upgrade || valid != scenario.valid {
			t.Fatalf("%+v: got upgrade=%v valid=%v", scenario, upgrade, valid)
		}
	}
	// The exception applies only to current identities, not published release manifests.
	release := testRelease()
	release.Version = "1.2.3-ubuntu"
	if validateRelease(release) == nil {
		t.Fatal("bootstrap build accepted as stable release")
	}
}

// TestUbuntuBootstrapStagedUpgrade exercises discovery, download, activation and rollback.
// t receives failures; deterministic existing host/store fixtures prevent real Docker effects.
func TestUbuntuBootstrapStagedUpgrade(t *testing.T) {
	host := &stagedTestHost{fakeHost: &fakeHost{current: "1.2.3-ubuntu"}}
	release := testRelease()
	release.Version = "1.2.3"
	service, err := NewService(&memoryStore{}, host, &fakeSource{release: release}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if status := service.Status(); status.State != "available" || !status.CanApply || status.CurrentVersion != "1.2.3-ubuntu" {
		t.Fatalf("bootstrap discovery failed: %+v", status)
	}
	if err = service.Download(release.Version, release.Digest, "更新 "+release.Version); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if service.Status().State != "downloaded" || host.current != "1.2.3-ubuntu" {
		t.Fatal("download altered the running bootstrap identity")
	}
	if err = service.Activate(release.Version, release.Digest, "更新 "+release.Version); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if service.Status().State != "succeeded" || host.current != "1.2.3" {
		t.Fatalf("bootstrap activation failed: %+v", service.Status())
	}
	if err = service.Rollback("回退转发程序"); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if host.current != "1.2.3-ubuntu" {
		t.Fatal("rollback lost bootstrap version")
	}
}

// TestUbuntuBootstrapRejectsDowngrade blocks both immediate and staged mutations before pull.
// t receives failures; candidate 1.2.2 is the older formal release reported by the user.
func TestUbuntuBootstrapRejectsDowngrade(t *testing.T) {
	host := &stagedTestHost{fakeHost: &fakeHost{current: "1.2.3-ubuntu"}}
	release := testRelease()
	release.Version = "1.2.2"
	service, err := NewService(&memoryStore{}, host, &fakeSource{release: release}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if service.Status().CanApply || service.Status().State != "idle" {
		t.Fatal("older release offered")
	}
	if service.Download(release.Version, release.Digest, "更新 "+release.Version) == nil || service.Apply(release.Version, release.Digest, "更新 "+release.Version) == nil {
		t.Fatal("older release accepted")
	}
	service.Wait()
	if len(host.calls) != 0 {
		t.Fatal("downgrade touched host")
	}
}

// TestUbuntuVerbs checks unknown helper commands cannot enter a subprocess.
func TestUbuntuVerbs(t *testing.T) {
	runner := &commandRunner{}
	host := &UbuntuHost{Runner: runner}
	if host.Script(context.Background(), "down", Release{}) == nil || len(runner.calls) != 0 {
		t.Fatal("arbitrary Compose operation accepted")
	}
}
