// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package main

import "os"

// On Windows files get the user's default ACL, and tuimeta starts the
// helper with stderr discarded.
func restrictUmask() {}

func quietStderr() {}

func ownedByMe(os.FileInfo) bool { return true }
