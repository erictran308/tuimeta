// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build race

package metatext

// slowdown is how much slower the race detector makes the parser.
const slowdown = 10
