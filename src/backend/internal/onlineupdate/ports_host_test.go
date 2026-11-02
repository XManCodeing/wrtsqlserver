package onlineupdate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// portRunner accepts only inspect and fixed script calls and exposes no host side effects.
type portRunner struct {
	data         []byte
	environments [][]string
	fail         bool
}

func (runner *portRunner) Run(_ context.Context, name string, args, environment []string) ([]byte, error) {
	runner.environments = append(runner.environments, environment)
	if name == "/usr/bin/docker" {
		return runner.data, nil
	}
	if runner.fail {
		return nil, errors.New("unsafe layout")
	}
	return nil, nil
}

// TestPortBindingInspection rejects EXPOSE-only, NAT/wildcard/remapped/UDP and unmanaged layouts.
func TestPortBindingInspection(t *testing.T) {
	fixtures := []struct {
		bindings string
		valid    bool
	}{
		{`{"8080/tcp":[{"HostIp":"192.168.10.1","HostPort":"18080"}],"13306/tcp":[{"HostIp":"192.168.10.1","HostPort":"13306"}]}`, true},
		{`{"8080/tcp":[{"HostIp":"192.168.10.1","HostPort":"18080"}]}`, true},
		{`{"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"18080"}]}`, false},
		{`{"3306/tcp":null}`, false},
		{`{"8080/tcp":[{"HostIp":"192.168.10.1","HostPort":"18080"}],"3306/udp":[{"HostIp":"192.168.10.1","HostPort":"3306"}]}`, false},
	}
	for _, fixture := range fixtures {
		runner := &portRunner{data: []byte(`{"labels":{"io.db-relay.deploy.managed":"relay-v1"},"running":true,"bindings":` + fixture.bindings + `}`)}
		host := &DockerHost{Config: Config{LANBindIP: "192.168.10.1", DeployDir: "/trusted"}, Runner: runner, trustCheck: func(string) error { return nil }}
		ports, _, err := host.InspectPorts(context.Background())
		if (err == nil) != fixture.valid {
			t.Fatalf("%s: %v", fixture.bindings, err)
		}
		if fixture.valid && strings.Contains(fixture.bindings, `"3306/tcp"`) && len(ports) != 0 {
			t.Fatal("EXPOSE treated as publication")
		}
	}
}

// TestPortStatusLegacySchema ensures port state never adds fields or phases to strict old JSON.
func TestPortStatusLegacySchema(t *testing.T) {
	service, _, _ := portService(t)
	encoded, err := json.Marshal(service.record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "appliedPorts") || strings.Contains(string(encoded), "schemaVersion") {
		t.Fatal("port state contaminated legacy JSON")
	}
}
