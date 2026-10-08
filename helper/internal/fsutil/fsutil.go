// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fsutil makes the helper's folders private and replaces its files
// atomically, so a crash never leaves half a file behind.
package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// PrivateDir makes path (and any missing parents) a folder only this user
// can open, fixing its mode if it already exists.
func PrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a folder")
	}
	return os.Chmod(path, 0o700)
}

// WriteAtomic replaces path with data, readable by this user only. With
// durable, the data reaches the disk before the rename, for files whose loss
// would cost the user something (a login).
func WriteAtomic(path string, data []byte, durable bool) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if durable {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// ValidName reports whether name is a plain file name the helper may use in
// its own folders: letters, digits, '.', '_' and '-', not starting with '.'.
func ValidName(name string) bool {
	if name == "" || len(name) > 100 || strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
