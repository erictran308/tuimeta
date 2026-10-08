// SPDX-License-Identifier: AGPL-3.0-or-later

// Package backend is what a network plugs into: the Backend interface the
// server routes requests to, the Events it reports with, and the shared
// pieces (ids, files, sessions, the outbox) it's given in Deps.
package backend

import (
	"context"
	"io"

	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/session"
)

// Backend is one network's account. The server calls it from many
// goroutines at once, one per request, so every method must be safe for
// concurrent use. Errors meant for the user are *proto.Error (their Message
// is shown as is); any other error becomes "internal" and only its kind is
// logged, so a library's error text never reaches tuimeta or the log.
//
// The server has already checked what it can: ids name a known chat, person
// or message of this network, the account isn't logged_out (except for
// LoginCookies and Logout), and params are well formed.
type Backend interface {
	// Network is the network this backend speaks.
	Network() proto.Network

	// Start runs once, after hello. It sends an account event at once
	// (logged_out, or connecting when a saved session is being resumed) and
	// connects in the background. ctx ends when the helper quits.
	Start(ctx context.Context)

	// Close disconnects without telling the network anything: no read
	// receipts, no presence, no logout. tuimeta has quit; it should return
	// within a second.
	Close()

	// LoginCookies logs in with cookies whose required names the server has
	// checked, as the browser named (browser.Default() for the libraries'
	// own), returning once connected (account ready). The session is saved
	// only after that, browser and all: a resumed session says it's the same
	// browser. Errors: bad_cookies, checkpoint, network.
	LoginCookies(ctx context.Context, c cookies.Set, as browser.Identity) error

	// Logout logs out on the network where it can (and removes the
	// encrypted-chat device), wipes Deps.Session, and reports logged_out.
	// The server then forgets the network's ids and downloaded files.
	Logout(ctx context.Context) error

	// LoadChats sends chat (and user) events for up to limit more chats,
	// newest activity first, beyond those it sent before (since the last
	// login), and says whether more remain. It sends the events itself, under
	// the same lock as its other chat events, so an older copy of a chat
	// never follows a newer one.
	LoadChats(ctx context.Context, limit int) (hasMore bool, err error)

	// History answers a history request; history.Log.Page does the paging
	// once the messages are in a Log.
	History(ctx context.Context, chat ChatRef, q history.Query) (history.Page, error)

	// GetMessage is one message (one part, msg.ID, of an album).
	GetMessage(ctx context.Context, msg MessageRef) (proto.Message, error)

	// Send sends out.Text and out.Files to out.Chat. The server has sent
	// the pending messages and answered with their temporary ids. Send calls
	// out.Sent with the network's messages once it's accepted, or returns an
	// error (or calls out.Failed). If the confirmation can come from the
	// event stream before the send call returns, set out.SetKey first and
	// look it up with Deps.Outbox.Find when it arrives.
	Send(ctx context.Context, out *Outgoing) error

	// EditText replaces the text of your message.
	EditText(ctx context.Context, msg MessageRef, text string) error

	// Delete unsends your message for everyone.
	Delete(ctx context.Context, msg MessageRef) error

	// React sets your one reaction to msg; emoji "" removes it.
	React(ctx context.Context, msg MessageRef, emoji string) error

	// MarkRead marks the chat read up to msg. It's the only way a read
	// receipt may go out.
	MarkRead(ctx context.Context, msg MessageRef) error

	// SetTyping says you started or stopped typing. It's the only way a
	// typing notification may go out.
	SetTyping(ctx context.Context, chat ChatRef, typing bool) error

	// Mute mutes the chat for good, or unmutes it.
	Mute(ctx context.Context, chat ChatRef, muted bool) error

	// Search finds people and groups to open a chat with.
	Search(ctx context.Context, query string) ([]proto.SearchResult, error)

	// OpenDM is the chat with the person, made if needed (sending its chat
	// event when it's new).
	OpenDM(ctx context.Context, user UserRef) (int64, error)

	// Fetch writes the file's bytes to w, for a download. The download
	// manager counts them, enforces the size limit, reports progress and
	// saves the file.
	Fetch(ctx context.Context, file ids.FileRef, w io.Writer) error
}

// ChatRef is a chat, by the helper's id and the network's.
type ChatRef struct {
	ID      int64
	Network proto.Network
	NetID   string
}

// UserRef is a person, by the helper's id and the network's.
type UserRef struct {
	ID      int64
	Network proto.Network
	NetID   string
}

// MessageRef is a message a request names. A request naming any part of an
// album acts on the whole network message, NetID; ID is the part named.
type MessageRef struct {
	Chat  ChatRef
	ID    int64
	NetID string
	// Index is the part named, of Count; IDs are all the parts' ids.
	Index, Count int
	IDs          []int64
}

// Upload is a file to send, read when the request came (it's never read
// again, so swapping the file afterwards changes nothing).
type Upload struct {
	Name string // the file's base name
	Mime string
	Kind proto.MediaKind
	Data []byte
	// Width and Height are an image's, when its header says.
	Width, Height int
}

// Deps is what the server gives a backend.
type Deps struct {
	Events   *Events
	IDs      *ids.Store    // chat and person ids, kept across runs
	Messages *ids.Messages // message ids for the run
	Files    *ids.Files    // file ids for the run
	Outbox   *Outbox       // messages being sent
	Session  *session.Store
}
