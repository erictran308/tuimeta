// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package fsutil

import (
	"os"
	"syscall"
)

const noFollow = syscall.O_NOFOLLOW

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || st.Uid == uint32(os.Getuid())
}
