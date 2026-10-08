// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package main

import (
	"os"
	"syscall"
)

// restrictUmask makes every file the helper creates private to the user.
func restrictUmask() { syscall.Umask(0o077) }

// quietStderr points stderr at /dev/null, so nothing (not even the Go
// runtime's report of a crash in a library's goroutine, which can quote a
// message) ever reaches the terminal tuimeta draws on.
func quietStderr() {
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer null.Close()
	_ = syscall.Dup3(int(null.Fd()), 2, 0)
}

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || st.Uid == uint32(os.Getuid())
}
