// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session keeps what a network's login needs between runs: files in
// <data-dir>/<network>/, private to the user, replaced atomically, and all
// removed on logout.
package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/erictran308/tuimeta/helper/internal/fsutil"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// ErrBadName is returned for a name that isn't a plain file name.
var ErrBadName = errors.New("session: bad file name")

// Store is one network's folder.
type Store struct{ dir string }

// New is the network's store under dataDir; nothing is created until used.
func New(dataDir string, n proto.Network) *Store {
	return &Store{dir: filepath.Join(dataDir, string(n))}
}

// Dir makes the folder (0700) and returns it, for a library that keeps its
// own files there (say, the encrypted chats' database).
func (s *Store) Dir() (string, error) {
	if err := fsutil.PrivateDir(s.dir); err != nil {
		return "", err
	}
	return s.dir, nil
}

// Path is where name is kept, making the folder first.
func (s *Store) Path(name string) (string, error) {
	if !fsutil.ValidName(name) {
		return "", ErrBadName
	}
	dir, err := s.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Save replaces name with data (0600, synced to disk).
func (s *Store) Save(name string, data []byte) error {
	path, err := s.Path(name)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, data, true)
}

// SaveJSON saves v as JSON.
func (s *Store) SaveJSON(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Save(name, data)
}

// Load reads name; the error wraps os.ErrNotExist if it was never saved.
func (s *Store) Load(name string) ([]byte, error) {
	if !fsutil.ValidName(name) {
		return nil, ErrBadName
	}
	return os.ReadFile(filepath.Join(s.dir, name))
}

// LoadJSON reads name into v; false if it was never saved.
func (s *Store) LoadJSON(name string, v any) (bool, error) {
	data, err := s.Load(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

// Remove deletes name, if it's there.
func (s *Store) Remove(name string) error {
	if !fsutil.ValidName(name) {
		return ErrBadName
	}
	err := os.Remove(filepath.Join(s.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Wipe deletes the folder and everything in it (logout).
func (s *Store) Wipe() error { return os.RemoveAll(s.dir) }
