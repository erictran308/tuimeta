// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"context"
	"strings"
)

// Linker is a network that logs in by linking tuimeta as a new device of
// the account (WhatsApp), instead of with cookies. The server sends
// login_link and cancel_login to it.
type Linker interface {
	// Link links a new device and returns once it's connected (account
	// ready). With phone empty it reports QR codes to scan as they come
	// (Events.LoginCode, with attempt); with a phone number (digits only,
	// PhoneDigits), the pairing code to type on the phone. A newer Link,
	// CancelLink or Logout ends a waiting one with cancelled, until the
	// phone has confirmed it (then only Logout does). Errors: timeout,
	// cancelled, network, unsupported.
	Link(ctx context.Context, phone string, attempt uint64) error

	// CancelLink ends the waiting Link of attempt (any, with 0), so no code
	// shown for it works any more. Nothing waiting, nothing happens.
	CancelLink(attempt uint64)
}

// PhoneDigits reads a phone number in international form: digits, with an
// optional leading "+", and spaces, dashes, dots or brackets between them.
// It returns the digits, 7 to 15 of them (E.164's most), not starting with
// 0, which would be a number in national form.
func PhoneDigits(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", false
		}
	}
	d := b.String()
	if len(d) < 7 || len(d) > 15 || d[0] == '0' {
		return "", false
	}
	return d, true
}
