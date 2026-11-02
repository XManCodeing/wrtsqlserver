package main

import (
	"errors"
	"sort"
)

// publicationPorts merges the host-approved migration baseline with enabled mapping intent.
// It never creates mappings or invents database targets for baseline-only publications.
func publicationPorts(cfg Config, baseline []int) ([]int, error) {
	ports, err := desiredPorts(cfg)
	if err != nil {
		return nil, err
	}
	for _, required := range baseline {
		if required != 3306 && required != 13306 {
			return nil, errors.New("宿主迁移基线包含未批准端口")
		}
		exists := false
		for _, port := range ports {
			if port == required {
				exists = true
				break
			}
		}
		if !exists {
			ports = append(ports, required)
		}
	}
	if len(ports) > 64 {
		return nil, errors.New("含迁移基线的端口集合超过 64 个")
	}
	sort.Ints(ports)
	return ports, nil
}

// validPortStatus validates bounded host fields before exposing capability-dependent controls.
// Management 18080 may appear in actual bindings; Redis may never be owned by this endpoint.
func validPortStatus(status portPanelStatus) bool {
	if len(status.RequiredPorts) > 2 {
		return false
	}
	for _, port := range status.RequiredPorts {
		if port != 3306 && port != 13306 {
			return false
		}
	}
	if status.SchemaVersion != 1 || len(status.Revision) == 0 || len(status.Revision) > 128 || len(status.OperationID) > 128 || len(status.Message) > 4096 {
		return false
	}
	switch status.State {
	case "idle", "applying", "failed", "manual_recovery":
	default:
		return false
	}
	for _, ports := range [][]int{status.AppliedPorts, status.ActualPorts} {
		if len(ports) > 65 {
			return false
		}
		seen := map[int]bool{}
		for _, port := range ports {
			if port < 1024 || port > 65535 || port == 6379 || port == 8080 || seen[port] {
				return false
			}
			seen[port] = true
		}
	}
	return true
}
