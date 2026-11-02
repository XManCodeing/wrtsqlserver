package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// Store encrypts the entire configuration with a local, permission-restricted AES key.
type Store struct {
	directory string
	aead      cipher.AEAD
}

// newStore opens or creates directory and its 0600 local key, returning an encrypted store.
func newStore(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return nil, errors.New("data directory must be a real directory")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(directory, "secret.key")
	key, err := readPrivateFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(filepath.Join(directory, "config.enc")); statErr == nil {
			return nil, errors.New("encryption key missing for existing configuration")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		file, createErr := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if createErr != nil {
			return nil, createErr
		}
		_, writeErr := file.Write(key)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid encryption key length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Store{directory: directory, aead: aead}, nil
}

// readPrivateFile reads a bounded regular file and enforces owner-only permissions.
func readPrivateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, errors.New("invalid or oversized private file")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Load authenticates and decrypts stored configuration; missing data yields empty collections.
func (store *Store) Load() (Config, error) {
	cfg := Config{Proxies: []Proxy{}, Mappings: []Mapping{}}
	data, err := readPrivateFile(filepath.Join(store.directory, "config.enc"))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	nonceSize := store.aead.NonceSize()
	if len(data) < nonceSize {
		return cfg, errors.New("invalid encrypted configuration")
	}
	plaintext, err := store.aead.Open(nil, data[:nonceSize], data[nonceSize:], []byte("opernwrtdocker-v1"))
	if err != nil {
		return cfg, errors.New("configuration authentication failed")
	}
	if err := json.Unmarshal(plaintext, &cfg); err != nil {
		return cfg, errors.New("invalid configuration JSON")
	}
	if cfg.Proxies == nil {
		cfg.Proxies = []Proxy{}
	}
	if cfg.Mappings == nil {
		cfg.Mappings = []Mapping{}
	}
	return cfg, nil
}

// Save encrypts cfg and atomically replaces the data file. A failure before rename preserves old data.
func (store *Store) Save(cfg Config) error {
	plaintext, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	nonce := make([]byte, store.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	encrypted := store.aead.Seal(nonce, nonce, plaintext, []byte("opernwrtdocker-v1"))
	file, err := os.CreateTemp(store.directory, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(encrypted); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(store.directory, "config.enc")); err != nil {
		return err
	}
	// Rename is the commit point. Directory sync errors must not falsely signal rollback.
	directory, err := os.Open(store.directory)
	if err != nil {
		log.Print("configuration committed but directory sync unavailable")
		return nil
	}
	if err := directory.Sync(); err != nil {
		log.Print("configuration committed but directory sync failed")
	}
	if err := directory.Close(); err != nil {
		log.Print("configuration directory close failed")
	}
	return nil
}
