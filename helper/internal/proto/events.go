// SPDX-License-Identifier: AGPL-3.0-or-later

package proto

// Events as written on the wire: each carries its name in "event".

type Hello struct {
	Event    string    `json:"event"`
	Version  int       `json:"version"`
	Helper   string    `json:"helper"`
	Networks []Network `json:"networks"`
}

func NewHello(helper string) Hello {
	return Hello{Event: "hello", Version: Version, Helper: helper, Networks: Networks}
}

// AccountEvent is where an account stands; Error says what to do about it.
type AccountEvent struct {
	Event   string       `json:"event"`
	Network Network      `json:"network"`
	State   AccountState `json:"state"`
	UserID  int64        `json:"user_id,omitempty"`
	Name    string       `json:"name,omitempty"`
	Error   string       `json:"error,omitempty"`
}

type ChatEvent struct {
	Event string `json:"event"`
	Chat  Chat   `json:"chat"`
}

type ChatRemovedEvent struct {
	Event  string `json:"event"`
	ChatID int64  `json:"chat_id"`
}

type UserEvent struct {
	Event string `json:"event"`
	User  User   `json:"user"`
}

type MessageEvent struct {
	Event   string  `json:"event"`
	Message Message `json:"message"`
}

type MessageSentEvent struct {
	Event   string  `json:"event"`
	ChatID  int64   `json:"chat_id"`
	OldID   int64   `json:"old_id"`
	Message Message `json:"message"`
}

// MessageFailedEvent's Error is one sentence, like an account's error.
type MessageFailedEvent struct {
	Event  string `json:"event"`
	ChatID int64  `json:"chat_id"`
	OldID  int64  `json:"old_id"`
	Error  string `json:"error"`
}

type MessageDeletedEvent struct {
	Event      string  `json:"event"`
	ChatID     int64   `json:"chat_id"`
	MessageIDs []int64 `json:"message_ids"`
}

// ReadEvent: Inbox is how far you've read (here or elsewhere), Outbox how far
// the other side read yours; nil fields didn't change.
type ReadEvent struct {
	Event  string `json:"event"`
	ChatID int64  `json:"chat_id"`
	Inbox  *int64 `json:"inbox,omitempty"`
	Outbox *int64 `json:"outbox,omitempty"`
	Unread *int   `json:"unread,omitempty"`
}

type TypingEvent struct {
	Event  string `json:"event"`
	ChatID int64  `json:"chat_id"`
	UserID int64  `json:"user_id"`
	Typing bool   `json:"typing"`
}

type FileEvent struct {
	Event string `json:"event"`
	File  File   `json:"file"`
}

// LoginCodeEvent is the code to show while a device is being linked: QR is
// drawn as a QR code to scan, Pairing typed on the phone. Expires is in unix
// seconds.
type LoginCodeEvent struct {
	Event   string  `json:"event"`
	Network Network `json:"network"`
	Attempt uint64  `json:"attempt,omitempty"` // the login_link's attempt
	QR      string  `json:"qr,omitempty"`
	Pairing string  `json:"pairing,omitempty"`
	Expires int64   `json:"expires"`
}

type ErrorEvent struct {
	Event   string  `json:"event"`
	Network Network `json:"network,omitempty"`
	Message string  `json:"message"`
}
