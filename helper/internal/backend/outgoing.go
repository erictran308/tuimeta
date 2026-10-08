// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"errors"
	"sync"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Outgoing is a message being sent: what to send, and the temporary ids
// tuimeta knows it by until Sent or Failed says what became of it.
type Outgoing struct {
	Chat    ChatRef
	TempIDs []int64 // one per file, or one for text
	Text    string  // the text, or the files' caption
	ReplyTo *MessageRef
	Files   []Upload

	box  *Outbox
	mu   sync.Mutex
	key  string
	done chan struct{}
	over bool
}

// Outbox tracks the messages being sent, so a confirmation that arrives on
// the event stream before the send call returns finds its message.
type Outbox struct {
	events   *Events
	messages *ids.Messages

	mu    sync.Mutex
	byKey map[outKey]*Outgoing
}

type outKey struct {
	network proto.Network
	key     string
}

func NewOutbox(events *Events, messages *ids.Messages) *Outbox {
	return &Outbox{events: events, messages: messages, byKey: map[outKey]*Outgoing{}}
}

// New starts tracking a message being sent (the server calls it).
func (b *Outbox) New(chat ChatRef, temps []int64, text string, reply *MessageRef, files []Upload) *Outgoing {
	return &Outgoing{
		Chat: chat, TempIDs: temps, Text: text, ReplyTo: reply, Files: files,
		box: b, done: make(chan struct{}),
	}
}

// Find is the message being sent on network n under key, or nil.
func (b *Outbox) Find(n proto.Network, key string) *Outgoing {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.byKey[outKey{n, key}]
}

// SetKey files the message under the network's own handle for it (say, the
// offline threading id), for Outbox.Find.
func (o *Outgoing) SetKey(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.over {
		return
	}
	o.box.mu.Lock()
	if o.key != "" {
		delete(o.box.byKey, outKey{o.Chat.Network, o.key})
	}
	o.key = key
	o.box.byKey[outKey{o.Chat.Network, key}] = o
	o.box.mu.Unlock()
}

// Done is closed once Sent or Failed has been called.
func (o *Outgoing) Done() <-chan struct{} { return o.done }

// finish ends the message's tracking; o.mu is held. False if it was over.
func (o *Outgoing) finish() bool {
	if o.over {
		return false
	}
	o.over = true
	close(o.done)
	if o.key != "" {
		o.box.mu.Lock()
		delete(o.box.byKey, outKey{o.Chat.Network, o.key})
		o.box.mu.Unlock()
	}
	for _, id := range o.TempIDs {
		o.box.messages.Retire(o.Chat.ID, id)
	}
	return true
}

// Sent reports the network accepted the message, as msgs (one per part;
// their ids already assigned). Each temporary id is paired with a part in
// order (message_sent); temporary ids left over were merged into the others
// (message_deleted), and parts left over are new messages. Only the first
// of Sent and Failed counts; later calls return false.
func (o *Outgoing) Sent(msgs ...proto.Message) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.finish() {
		return false
	}
	ev := o.box.events
	var merged []int64
	for i, temp := range o.TempIDs {
		if i < len(msgs) {
			ev.messageSent(o.Chat.ID, temp, msgs[i])
		} else {
			merged = append(merged, temp)
		}
	}
	ev.MessageDeleted(o.Chat.ID, merged)
	for _, m := range msgs[min(len(o.TempIDs), len(msgs)):] {
		ev.Message(m)
	}
	return true
}

// Failed reports the message couldn't be sent. A *proto.Error's message is
// shown; any other error's text never is.
func (o *Outgoing) Failed(err error) bool { return o.Partly(err) }

// Partly reports a send that stopped with err after msgs went out (one per
// part, in order): the temporary ids paired with them get message_sent, the
// ones left message_failed. With no msgs it's Failed.
func (o *Outgoing) Partly(err error, msgs ...proto.Message) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.finish() {
		return false
	}
	msg := "The message couldn't be sent; try again."
	var pe *proto.Error
	if errors.As(err, &pe) {
		msg = pe.Message
	}
	hlog.Warn("send failed", hlog.Str("network", string(o.Chat.Network)), hlog.Int("sent", int64(len(msgs))), hlog.Kind(err))
	for i, temp := range o.TempIDs {
		if i < len(msgs) {
			o.box.events.messageSent(o.Chat.ID, temp, msgs[i])
		} else {
			o.box.events.messageFailed(o.Chat.ID, temp, msg)
		}
	}
	for _, m := range msgs[min(len(o.TempIDs), len(msgs)):] {
		o.box.events.Message(m)
	}
	return true
}
