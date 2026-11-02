package onlineupdate

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
)

// portList renders validated integer ports as a fixed comma-separated shell environment value.
func portList(ports []int) string {
	values := make([]string, len(ports))
	for index, port := range ports {
		values[index] = strconv.Itoa(port)
	}
	return strings.Join(values, ",")
}

// InspectPorts trusts actual PortBindings, not image EXPOSE metadata; unknown bindings fail closed.
func (host *DockerHost) InspectPorts(ctx context.Context) ([]int, bool, error) {
	data, err := host.docker(ctx, "inspect", "--format", `{"labels":{{json .Config.Labels}},"running":{{json .State.Running}},"bindings":{{json .HostConfig.PortBindings}}}`, "db-relay")
	if err != nil {
		return nil, false, err
	}
	var container struct {
		Labels   map[string]string `json:"labels"`
		Running  bool              `json:"running"`
		Bindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"bindings"`
	}
	if json.Unmarshal(data, &container) != nil || !container.Running || container.Labels["io.db-relay.deploy.managed"] != "relay-v1" {
		return nil, false, errors.New("unmanaged or unavailable relay")
	}
	ports := []int{}
	management := false
	for exposed, bindings := range container.Bindings {
		if len(bindings) != 1 || bindings[0].HostIP != host.Config.LANBindIP {
			return nil, false, errors.New("unknown binding layout")
		}
		binding := bindings[0]
		if exposed == "8080/tcp" && binding.HostPort == "18080" {
			management = true
			continue
		}
		port, parseErr := strconv.Atoi(binding.HostPort)
		if parseErr != nil || strconv.Itoa(port)+"/tcp" != exposed {
			return nil, false, errors.New("noncanonical port binding")
		}
		ports = append(ports, port)
	}
	ports, err = ValidatePorts(ports)
	if err != nil || !management {
		return nil, false, errors.New("invalid managed port layout")
	}
	// The shell additionally checks historical layout and all mounts before reporting support.
	if err = host.PortScript(ctx, "ports-status", ports); err != nil {
		return nil, false, err
	}
	ready := host.PortScript(ctx, "ports-rules", ports) == nil
	return ports, ready, nil
}

// PortScript invokes only fixed script modes and validates all environment data independently.
func (host *DockerHost) PortScript(ctx context.Context, mode string, ports []int) error {
	switch mode {
	case "ports-status", "ports-rules", "ports-check", "ports-apply":
	default:
		return errors.New("unsupported port operation")
	}
	normalized, err := ValidatePorts(ports)
	if err != nil {
		return err
	}
	if err = host.trustedDeployment(); err != nil {
		return errors.New("deployment trust check failed")
	}
	environment := append(host.environment(Release{}), "RELAY_PORTS="+portList(normalized))
	_, err = host.Runner.Run(ctx, "/bin/sh", []string{filepath.Join(host.Config.DeployDir, "relay-update.sh"), mode}, environment)
	return err
}
