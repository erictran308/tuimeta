// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package server

import "syscall"

const openNonBlock = syscall.O_NONBLOCK
