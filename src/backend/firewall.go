package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

const firewallFilename = "firewall-listeners.json"

// firewallListener intentionally contains only the host controller's public inputs.
type firewallListener struct {
	Host    string   `json:"host"`
	Port    int      `json:"port"`
	Sources []string `json:"sources"`
}

type firewallSnapshot struct {
	Version   int                `json:"version"`
	Listeners []firewallListener `json:"listeners"`
}

// syncFirewallLocked publishes actually bound listeners in stable configuration order.
// Callers own app.mu. Failure means publication is uncertain, not that config was rolled back.
func (app *App) syncFirewallLocked() error {
	if !app.hostNetworking {
		return nil
	}
	snapshot := firewallSnapshot{Version: 1, Listeners: []firewallListener{}}
	for _, mapping := range app.cfg.Mappings {
		runtime := app.runtimes[mapping.ID]
		if runtime == nil || runtime.view().Status != "running" {
			continue
		}
		address := runtime.listener.Addr().(*net.TCPAddr)
		if !address.IP.IsLoopback() {
			snapshot.Listeners = append(snapshot.Listeners, firewallListener{address.IP.String(), address.Port, runtime.mapping.AllowedCIDRs})
		}
	}
	if err := app.store.writeFirewall(snapshot); err != nil {
		log.Printf("firewall listener snapshot publication failed: %v", err)
		if !app.closed && app.firewallRetry == nil {
			app.firewallRetry = time.AfterFunc(time.Second, app.retryFirewall)
		}
		return errors.New("firewall listener snapshot could not be synchronized")
	}
	return nil
}

// retryFirewall recomputes under the mutation lock, never replaying an outdated snapshot.
func (app *App) retryFirewall() {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.firewallRetry = nil
	if !app.closed {
		_ = app.syncFirewallLocked() // Failure is logged and schedules exactly one further attempt.
	}
}

// listenerFailed removes a failed runtime generation unless an edit or shutdown already superseded it.
func (app *App) listenerFailed(runtime *Runtime) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.closed && app.runtimes[runtime.mapping.ID] == runtime {
		_ = app.syncFirewallLocked() // An asynchronous failure has no caller; sync logs and retries.
	}
}

// writeFirewall atomically replaces the snapshot within the store's existing private directory.
// Never open the destination for writing: even a raced-in symlink is replaced, not followed.
func (store *Store) writeFirewall(snapshot firewallSnapshot) error {
	info, err := os.Lstat(store.directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("firewall data directory must be private and not a symlink")
	}
	parent, err := os.Open(store.directory)
	if err != nil {
		return err
	}
	defer parent.Close()
	opened, err := parent.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return errors.New("firewall data directory changed while opening")
	}
	directory := store.directory
	if runtime.GOOS == "linux" {
		// Anchor all operations to this exact directory even if its path is concurrently
		// replaced. Ubuntu containers mount procfs; an unavailable fd path fails closed.
		directory = filepath.Join("/proc/self/fd", strconv.FormatUint(uint64(parent.Fd()), 10))
	}
	path := filepath.Join(directory, firewallFilename)
	if entry, err := os.Lstat(path); err == nil && !entry.Mode().IsRegular() {
		return errors.New("firewall snapshot must be a regular file, not a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(directory, ".firewall-listeners-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		err = json.NewEncoder(file).Encode(snapshot)
	}
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("sync firewall snapshot directory: %w", err)
	}
	return nil
}
