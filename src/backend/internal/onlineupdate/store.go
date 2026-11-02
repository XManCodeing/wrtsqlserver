package onlineupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Status is the secret-free public and durable operation snapshot.
type Status struct {
	State          string   `json:"state"`
	CurrentVersion string   `json:"currentVersion"`
	Latest         *Release `json:"latest"`
	Phase          string   `json:"phase"`
	Message        string   `json:"message"`
	OperationID    string   `json:"operationId"`
	UpdatedAt      string   `json:"updatedAt"`
	CanApply       bool     `json:"canApply"`
	CanRollback    bool     `json:"canRollback"`
	Enabled        bool     `json:"enabled"`
}

// Record includes only non-secret internal rollback metadata alongside public status.
type Record struct {
	Source          Source `json:"source"`
	Status          Status `json:"status"`
	PreviousVersion string `json:"previousVersion"`
}

// Store persists snapshots before any external mutation.
type Store interface {
	Load() (Record, error)
	Save(Record) error
}

// FileStore atomically replaces the root-only state file on the host USB filesystem.
type FileStore struct{ Path string }

// Load validates file trust and bounded JSON; absent state is an initial installation.
func (store FileStore) Load() (Record, error) {
	var record Record
	data, err := privateRead(store.Path, 16*1024)
	if err != nil {
		return record, err
	}
	err = decodeStrict(strings.NewReader(string(data)), 16*1024, &record)
	return record, err
}

// Save fsyncs a private temporary file and its parent after atomic rename.
func (store FileStore) Save(record Record) error {
	return savePrivateJSON(store.Path, record)
}

// savePrivateJSON atomically persists a separate versioned document without changing legacy JSON.
// path must be root-private; value must contain no credentials. Errors stop host mutations.
func savePrivateJSON(path string, value any) error {
	store := FileStore{Path: path}
	directory := filepath.Dir(store.Path)
	if err := trustedPath(directory); err != nil {
		return err
	}
	if _, err := os.Lstat(store.Path); err == nil {
		if _, err = privateRead(store.Path, 16*1024); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".status-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(temporary, store.Path); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

// transactionExists treats every error except a missing entry as a retained transaction.
func transactionExists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, os.ErrNotExist)
}

// activeState identifies phases that must never be retried automatically after restart.
func activeState(state string) bool {
	switch state {
	case "checking", "downloading", "applying":
		return true
	}
	return false
}
