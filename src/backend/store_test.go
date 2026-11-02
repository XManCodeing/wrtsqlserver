package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEncryptedStoreRejectsTampering verifies authenticated encryption fails closed on modified bytes.
func TestEncryptedStoreRejectsTampering(t *testing.T) {
	directory := t.TempDir()
	store, err := newStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Proxies: []Proxy{{ID: "example", Password: "hidden"}}, Mappings: []Mapping{}}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.enc")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if err := os.Remove(filepath.Join(directory, "secret.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := newStore(directory); err == nil {
		t.Fatal("missing key silently regenerated")
	}
}

// TestStoreRejectsSymlink prevents following attacker-controlled key paths inside the data directory.
func TestStoreRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "secret.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := newStore(directory); err == nil {
		t.Fatal("key symlink accepted")
	}
}
