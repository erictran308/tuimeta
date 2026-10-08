// SPDX-License-Identifier: AGPL-3.0-or-later

package browser

import "syscall"

// systemVersion is macOS's version ("26.0.1"), as Chrome reports it in
// sec-ch-ua-platform-version.
func systemVersion() string {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}
