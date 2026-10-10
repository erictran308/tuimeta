// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix

package server

import (
	"os"
	"syscall"
)

// openUpload opens a file to send, refusing a link in its place. Windows
// can't open a path without following a link at its end (short of opening
// the link itself), so this looks first; what was opened is then compared
// with what tuimeta listed.
func openUpload(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EINVAL}
	}
	return os.OpenFile(path, os.O_RDONLY, 0)
}
