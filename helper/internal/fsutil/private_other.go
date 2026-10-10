// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix

package fsutil

import "os"

// Windows opens through a link at a path's end; the folder's ACL is what
// keeps others from planting one.
const noFollow = 0

func ownedByMe(os.FileInfo) bool { return true }
