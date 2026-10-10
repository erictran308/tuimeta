// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !(linux || openbsd || dragonfly || darwin || freebsd || netbsd)

package server

import "os"

// systemIdentity knows no device, inode or status-change time here
// (tuimeta sends zeros for them on Windows); a file is known by its size and
// modification time only.
func systemIdentity(os.FileInfo) (dev, ino uint64, ctimeNs int64, ok bool) {
	return 0, 0, 0, false
}
