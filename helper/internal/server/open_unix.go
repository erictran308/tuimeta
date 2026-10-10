// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package server

import (
	"os"
	"syscall"
)

// openUpload opens a file to send without following a link in its place,
// and without blocking, so a pipe there can't hang it.
func openUpload(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
