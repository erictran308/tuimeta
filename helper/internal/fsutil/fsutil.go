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

// OpenPrivate opens path, one of the helper's own files, creating it 0600
// if flag says so, and refuses a link in its place or a file that isn't a
// plain one of this user's: the helper's folder is private, but if someone
// once planted a link in it, the helper still won't write where it points.
func OpenPrivate(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag|noFollow, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && (!info.Mode().IsRegular() || !ownedByMe(info)) {
		err = errors.New("not a private file")
	}
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
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
