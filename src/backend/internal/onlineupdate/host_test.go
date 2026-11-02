package onlineupdate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// commandRunner records the exact fixed executable/arguments with deterministic Docker fixtures.
type commandRunner struct {
	calls         []string
	failPull      bool
	failPreflight bool
	wrongImage    bool
}

// Run supplies bounded fake output without invoking any host command or Docker daemon.
func (runner *commandRunner) Run(_ context.Context, name string, args, environment []string) ([]byte, error) {
	command := name + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, command)
	for _, entry := range environment {
		if strings.HasPrefix(entry, "DOCKER_HOST=") || strings.Contains(entry, "TOKEN=") {
			return nil, errors.New("unsafe environment")
		}
	}
	if strings.Contains(command, "updater-preflight.sh") && runner.failPreflight {
		return nil, errors.New("no space")
	}
	if strings.Contains(command, " pull ") && runner.failPull {
		return nil, errors.New("registry unavailable")
	}
	if strings.Contains(command, "image inspect") {
		if runner.wrongImage {
			return []byte(`{"Os":"linux","Architecture":"arm64","RepoDigests":[]}`), nil
		}
		return []byte(`{"Os":"linux","Architecture":"amd64","Config":{"Labels":{"org.opencontainers.image.version":"1.2.0"}},"RepoDigests":["` + Image + `@` + testRelease().Digest + `"]}`), nil
	}
	return nil, nil
}

// TestDockerCommandPolicy verifies preflight-before-pull, immutable references, digest checks and tagging.
func TestDockerCommandPolicy(t *testing.T) {
	for _, test := range []struct {
		name                   string
		preflight, pull, wrong bool
		count                  int
	}{{"success", false, false, false, 4}, {"space", true, false, false, 1}, {"pull", false, true, false, 2}, {"platform", false, false, true, 3}} {
		t.Run(test.name, func(t *testing.T) {
			runner := &commandRunner{failPreflight: test.preflight, failPull: test.pull, wrongImage: test.wrong}
			host := &DockerHost{Config: Config{DeployDir: "/trusted/deploy", LANInterface: "br-lan", LANBindIP: "192.168.1.1"}, Runner: runner, trustCheck: func(string) error { return nil }}
			err := host.Pull(context.Background(), testRelease())
			if (err == nil) != (test.name == "success") || len(runner.calls) != test.count {
				t.Fatalf("wrong result: %v %v", err, runner.calls)
			}
			if runner.calls[0] != "/bin/sh /trusted/deploy/updater-preflight.sh" {
				t.Fatal("preflight missing")
			}
			if len(runner.calls) > 1 && runner.calls[1] != "/usr/bin/docker --host unix:///var/run/docker.sock pull "+Image+"@"+testRelease().Digest {
				t.Fatal("untrusted pull command")
			}
			if test.name == "success" && !strings.Contains(runner.calls[3], " tag "+Image+"@"+testRelease().Digest+" "+Image+":1.2.0") {
				t.Fatal("immutable image not tagged")
			}
			before := len(runner.calls)
			if host.Script(context.Background(), "prune", Release{}) == nil || len(runner.calls) != before {
				t.Error("arbitrary command accepted")
			}
		})
	}
}

// TestIntentBeforeApply verifies the persisted intent is available when replacement begins.
func TestIntentBeforeApply(t *testing.T) {
	host := &fakeHost{current: "1.0.0"}
	store := &memoryStore{}
	service := newTestService(t, host, &fakeSource{release: testRelease()}, store)
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	release := testRelease()
	for _, confirmation := range []string{"yes", "更新 2.0.0", "更新 1.2.0 "} {
		if service.Apply(release.Version, release.Digest, confirmation) == nil {
			t.Fatal("invalid confirmation accepted")
		}
	}
	if err := service.Apply(release.Version, release.Digest, "更新 "+release.Version); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	record, err := store.Load()
	if err != nil || record.PreviousVersion != "1.0.0" || record.Status.State != "succeeded" {
		t.Fatal("durable state missing")
	}
}

// TestNoDowngradeAtApply rechecks the running OCI version instead of trusting the cached check.
func TestNoDowngradeAtApply(t *testing.T) {
	host := &fakeHost{current: "1.0.0"}
	service := newTestService(t, host, &fakeSource{release: testRelease()}, &memoryStore{})
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	host.current = "2.0.0"
	release := testRelease()
	if err := service.Apply(release.Version, release.Digest, "更新 "+release.Version); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if len(host.calls) != 0 || service.Status().State != "failed" {
		t.Fatal("downgrade attempted")
	}
}
