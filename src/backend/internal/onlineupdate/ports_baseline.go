package onlineupdate

import (
	"os"
	"path/filepath"
)

// RequiredPorts exposes the approved first-migration baseline without publishing on a read.
// An unreadable marker preserves the baseline; shell preflight separately rejects unsafe metadata.
func (host *DockerHost) RequiredPorts() []int {
	if _, err := os.Lstat(filepath.Join(Root, "ports-migrated")); err != nil {
		return []int{3306, 13306}
	}
	return []int{}
}
