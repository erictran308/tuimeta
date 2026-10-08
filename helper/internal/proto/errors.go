// SPDX-License-Identifier: AGPL-3.0-or-later

package proto

// Code is an error's kind, which tuimeta acts on.
type Code string

const (
	BadRequest    Code = "bad_request"
	UnknownMethod Code = "unknown_method"
	NotFound      Code = "not_found"
	NotLoggedIn   Code = "not_logged_in"
	BadCookies    Code = "bad_cookies"
	Checkpoint    Code = "checkpoint"
	NetworkError  Code = "network"
	Unsupported   Code = "unsupported"
	Timeout       Code = "timeout"
	Cancelled     Code = "cancelled"
	Internal      Code = "internal"
)

// Error is an answer tuimeta can show: Message is one sentence a person can
// act on. It must never hold message text, names, cookies, tokens or keys,
// so it's always written by the helper, never copied from a library's error.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// Err makes an Error.
func Err(code Code, message string) *Error { return &Error{Code: code, Message: message} }

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// ErrorCode lets the log name the kind of an error without its text.
func (e *Error) ErrorCode() string { return string(e.Code) }

// Errors the dispatcher and several backends share.
var (
	ErrNoChat     = Err(NotFound, "That chat isn't known; reload the chat list.")
	ErrNoMessage  = Err(NotFound, "That message isn't loaded; load the chat's history first.")
	ErrNoUser     = Err(NotFound, "That person isn't known; search for them again.")
	ErrNoFile     = Err(NotFound, "That file isn't known; reload the message it's in.")
	ErrStillGoing = Err(BadRequest, "That message is still being sent.")
	ErrInternal   = Err(Internal, "Something went wrong in tuimeta-helper; its log has the details.")
)

// ErrLoggedOut is the error for a request to a network nobody is logged in to.
func ErrLoggedOut(n Network) *Error {
	return Err(NotLoggedIn, "Log in to "+n.Title()+" first.")
}

// Title is the network's name as people write it.
func (n Network) Title() string {
	switch n {
	case Messenger:
		return "Messenger"
	case Instagram:
		return "Instagram"
	case WhatsApp:
		return "WhatsApp"
	}
	return "that network"
}
