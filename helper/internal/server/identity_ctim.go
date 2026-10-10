// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux || openbsd || dragonfly

package server

import (
	"os"
	"syscall"
)

// systemIdentity is the device, inode and status-change time (in ns) stat
// gives for the file info describes.
func systemIdentity(info os.FileInfo) (dev, ino uint64, ctimeNs int64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return uint64(st.Dev), st.Ino, st.Ctim.Nano(), true
}
