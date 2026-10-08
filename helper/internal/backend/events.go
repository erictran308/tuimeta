// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"slices"
	"sync"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// TypingTimeout is how long after someone's last "typing" the helper says
// they stopped, if the network hasn't.
const TypingTimeout = 6 * time.Second

// Events is how backends tell tuimeta what happened. Each method writes one
// line (or a few) in the order called; a backend that must keep two events
// in order (a chat's older and newer state) calls them under its own lock.
type Events struct {
	out func(v any)

	mu       sync.Mutex
	accounts map[proto.Network]proto.AccountEvent
	self     map[proto.Network]int64
	typing   map[typingKey]*time.Timer
	timeout  time.Duration
	closed   bool
}

type typingKey struct{ chat, user int64 }

// NewEvents makes the sink; out writes one protocol line.
func NewEvents(out func(v any)) *Events {
	return &Events{
		out:      out,
		accounts: map[proto.Network]proto.AccountEvent{},
		self:     map[proto.Network]int64{},
		typing:   map[typingKey]*time.Timer{},
		timeout:  TypingTimeout,
	}
}

// SetTypingTimeout changes TypingTimeout (tests).
func (e *Events) SetTypingTimeout(d time.Duration) {
	e.mu.Lock()
	e.timeout = d
	e.mu.Unlock()
}

// Account reports where the network's account stands. UserID and Name go
// with ready; Error, with error, says what to do.
func (e *Events) Account(n proto.Network, state proto.AccountState, userID int64, name, errMsg string) {
	ev := proto.AccountEvent{Event: "account", Network: n, State: state, UserID: userID, Name: name, Error: errMsg}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.accounts[n] = ev
	if userID != 0 {
		e.self[n] = userID
	}
	if state == proto.LoggedOut {
		delete(e.self, n)
	}
	e.out(ev)
}

// State is the network's account state; logged_out until told otherwise.
func (e *Events) State(n proto.Network) proto.AccountState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ev, ok := e.accounts[n]; ok {
		return ev.State
	}
	return proto.LoggedOut
}

// Self is your own user id on the network, once an account event gave it.
func (e *Events) Self(n proto.Network) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.self[n]
}

func (e *Events) Chat(c proto.Chat) { e.out(proto.ChatEvent{Event: "chat", Chat: c}) }

func (e *Events) ChatRemoved(id int64) {
	e.out(proto.ChatRemovedEvent{Event: "chat_removed", ChatID: id})
}

func (e *Events) User(u proto.User) { e.out(proto.UserEvent{Event: "user", User: u}) }

// Message reports a new or changed message (each album part separately). A
// new message from someone who was typing ends their typing first.
func (e *Events) Message(m proto.Message) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !m.Outgoing && m.State != proto.Pending {
		e.stopTyping(typingKey{m.ChatID, m.SenderID})
	}
	e.out(proto.MessageEvent{Event: "message", Message: m})
}

func (e *Events) MessageDeleted(chat int64, ids []int64) {
	if len(ids) == 0 {
		return
	}
	e.out(proto.MessageDeletedEvent{Event: "message_deleted", ChatID: chat, MessageIDs: slices.Clone(ids)})
}

func (e *Events) messageSent(chat, oldID int64, m proto.Message) {
	e.out(proto.MessageSentEvent{Event: "message_sent", ChatID: chat, OldID: oldID, Message: m})
}

func (e *Events) messageFailed(chat, oldID int64, msg string) {
	e.out(proto.MessageFailedEvent{Event: "message_failed", ChatID: chat, OldID: oldID, Error: msg})
}

// Read reports read positions: inbox (how far you've read, here or
// elsewhere, with the unread count) and outbox (how far the other side read
// yours). Zero values are left out.
func (e *Events) Read(chat, inbox, outbox int64, unread *int) {
	ev := proto.ReadEvent{Event: "read", ChatID: chat, Unread: unread}
	if inbox != 0 {
		ev.Inbox = &inbox
	}
	if outbox != 0 {
		ev.Outbox = &outbox
	}
	e.out(ev)
}

// Typing reports someone typing, or stopping. A start is said once, and a
// stop is sent by the helper TypingTimeout after the last start if the
// network doesn't send one.
func (e *Events) Typing(chat, user int64, typing bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	k := typingKey{chat, user}
	if !typing {
		e.stopTyping(k)
		return
	}
	if t := e.typing[k]; t != nil {
		t.Reset(e.timeout)
		return
	}
	var t *time.Timer
	t = time.AfterFunc(e.timeout, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.typing[k] == t && !e.closed {
			e.stopTyping(k)
		}
	})
	e.typing[k] = t
	e.out(proto.TypingEvent{Event: "typing", ChatID: chat, UserID: user, Typing: true})
}

// stopTyping ends k's typing, if it's on; e.mu is held.
func (e *Events) stopTyping(k typingKey) {
	t := e.typing[k]
	if t == nil {
		return
	}
	t.Stop()
	delete(e.typing, k)
	e.out(proto.TypingEvent{Event: "typing", ChatID: k.chat, UserID: k.user, Typing: false})
}

func (e *Events) File(f proto.File) { e.out(proto.FileEvent{Event: "file", File: f}) }

// LoginCode is the code to show while login_link attempt waits: qr to draw
// as a QR code, or pairing to type on the phone; it works until expires.
func (e *Events) LoginCode(n proto.Network, attempt uint64, qr, pairing string, expires time.Time) {
	e.out(proto.LoginCodeEvent{Event: "login_code", Network: n, Attempt: attempt, QR: qr, Pairing: pairing, Expires: expires.Unix()})
}

// Error is something the user should see that no request caused.
func (e *Events) Error(n proto.Network, msg string) {
	e.out(proto.ErrorEvent{Event: "error", Network: n, Message: msg})
}

// Close stops the typing timers (the helper is quitting).
func (e *Events) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for k, t := range e.typing {
		t.Stop()
		delete(e.typing, k)
	}
}
