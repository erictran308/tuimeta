// SPDX-License-Identifier: AGPL-3.0-or-later

package browser

import "os"

// systemVersion is the kernel's release ("6.8.0-45-generic"), which Chrome on
// Linux reports in sec-ch-ua-platform-version.
func systemVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return string(data)
}
