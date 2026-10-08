// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !darwin && !linux

package browser

// systemVersion is unknown here. Chrome on Windows reports the version of
// Windows' UniversalApiContract, which the helper doesn't read, so the
// header goes empty, as the libraries send it.
func systemVersion() string { return "" }
