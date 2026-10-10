// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"slices"
	"unicode/utf8"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/metatext"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// assign gives msg its ids in c: the ones its network id already has, or
// new ones. A message whose number of parts changed since its ids were given
// gets new ones that fit; the old ones are returned as gone. m.mu is held.
func (m *Messenger) assign(c *chat, msg *message) (gone []int64) {
	n := msg.parts()
	got := m.d.Messages.Assign(c.id, msg.netID, msg.ms, n)
	if len(got) != n {
		got = m.d.Messages.Assign(c.id, shapeKey(msg.netID, n, false), msg.ms, n)
	}
	if old := c.msgs[msg.netID]; old != nil && len(old.ids) > 0 && !slices.Equal(old.ids, got) {
		gone = old.ids
		c.log.Remove(old.ids...)
	}
	msg.ids = got
	return gone
}

// keep puts msg in c, replacing the message with its network id, and
// returns its parts and the ids of parts that went away; m.mu is held.
func (m *Messenger) keep(c *chat, msg *message) (parts []proto.Message, gone []int64) {
	gone = m.assign(c, msg)
	c.msgs[msg.netID] = msg
	if msg.wa == nil {
		m.msgChat[msg.netID] = c.key
	} else {
		c.indexWA(msg)
	}
	parts = m.render(c, msg)
	c.log.Put(parts...)
	return parts, gone
}

// render is msg as protocol messages, one per part; m.mu is held.
func (m *Messenger) render(c *chat, msg *message) []proto.Message {
	userID := func(fbid int64) int64 { return m.person(fbid).id }
	text, ents := textOf(msg, userID)
	own := msg.sender == m.self
	base := proto.Message{
		ChatID:      c.id,
		SenderID:    userID(msg.sender),
		Outgoing:    own,
		Date:        msg.ms / 1000,
		Text:        text,
		Entities:    ents,
		Forwarded:   msg.forwarded,
		Reactions:   tally(msg.reactions, m.self),
		Edited:      msg.edited,
		Deletable:   own && msg.canUnsend && msg.service == "",
		State:       proto.Sent,
		Service:     msg.service,
		LinkPreview: msg.preview,
		Unsupported: msg.unsupported,
	}
	if msg.service != "" {
		base.Text, base.Entities = "", nil
	}
	if own && len(msg.media) == 0 && msg.service == "" && msg.unsupported == "" && msg.text != "" {
		base.EditableUntil = msg.ms/1000 + EditWindow
	}
	if r := msg.reply; r != nil {
		rt := &proto.ReplyTo{Text: r.text}
		if r.sender != 0 {
			rt.SenderID = userID(r.sender)
		}
		if known, ok := m.d.Messages.Known(c.id, r.netID); ok && len(known) > 0 {
			rt.MessageID = known[0]
		} else if replied := c.msgs[r.netID]; replied != nil && len(replied.ids) > 0 {
			rt.MessageID = replied.ids[0]
		}
		base.ReplyTo = rt
	}
	media := msg.media
	if len(msg.ids) != max(len(media), 1) {
		// Can't happen once assign has run; kept so a bug shows as a
		// message without its media rather than a panic.
		media = nil
		msg.ids = msg.ids[:1]
	}
	return proto.SplitAlbum(base, media, msg.ids)
}

// show reports msg to tuimeta: its parts, after the people it names, and the
// ids of parts that went away; m.mu is held.
func (m *Messenger) show(c *chat, msg *message, parts []proto.Message, gone []int64) {
	if m.replaying {
		return
	}
	m.tellPeople(msg)
	m.d.Events.MessageDeleted(c.id, gone)
	for _, p := range parts {
		m.d.Events.Message(p)
	}
}

// confirm reports msg as the network's confirmation of a message being sent;
// m.mu is held.
func (m *Messenger) confirm(out *backend.Outgoing, msg *message, parts []proto.Message) bool {
	m.tellPeople(msg)
	return out.Sent(parts...)
}

// tellPeople sends the user events of the people msg names; m.mu is held.
func (m *Messenger) tellPeople(msg *message) {
	m.tellUser(m.person(msg.sender), false)
	for _, mn := range msg.mentions {
		if mn.fbid > 0 {
			m.tellUser(m.person(mn.fbid), false)
		}
	}
	if msg.reply != nil && msg.reply.sender != 0 {
		m.tellUser(m.person(msg.reply.sender), false)
	}
	for _, r := range msg.reactions {
		m.tellUser(m.person(r.actor), false)
	}
}

// arrived records a message that just arrived (or one sent from elsewhere):
// the chat's activity and unread count move, and, unless it confirms one
// being sent here, it's reported; m.mu is held.
func (m *Messenger) arrived(c *chat, msg *message, otid string) {
	parts, gone := m.keep(c, msg)
	c.activity = max(c.activity, msg.ms)
	// Your own message doesn't move how far you've read: Messenger's web
	// client marks the chat read along with a send, but this backend sends no
	// receipt tuimeta didn't ask for, so the chat stays unread on Messenger
	// until it does.
	m.recount(c)
	if otid != "" && msg.sender == m.self {
		if out := m.d.Outbox.Find(net, otid); out != nil && out.Chat.ID == c.id {
			m.confirm(out, msg, parts)
			m.touch(c)
			return
		}
	}
	m.show(c, msg, parts, gone)
	m.touch(c)
}

// changed reports a known message after an edit or a reaction; m.mu is held.
func (m *Messenger) changed(c *chat, msg *message) {
	parts, gone := m.keep(c, msg)
	m.show(c, msg, parts, gone)
	if last, ok := c.log.Newest(); ok && slices.Contains(msg.ids, last.ID) {
		m.touch(c)
	}
}

// deleted reports a message gone (unsent); m.mu is held.
func (m *Messenger) deleted(c *chat, netID string) {
	msg := c.msgs[netID]
	if msg == nil {
		return
	}
	delete(c.msgs, netID)
	delete(m.msgChat, netID)
	if msg.wa != nil {
		c.unindexWA(msg)
	}
	c.log.Remove(msg.ids...)
	if !m.replaying {
		m.d.Events.MessageDeleted(c.id, msg.ids)
	}
	m.recount(c)
	m.touch(c)
}

// messageOf is the message a request names, by any of its parts; m.mu is held.
func (m *Messenger) messageOf(ref backend.MessageRef) (*chat, *message, error) {
	c, err := m.chatOf(ref.Chat.ID)
	if err != nil {
		return nil, nil, err
	}
	msg := c.msgs[baseNetID(ref.NetID)]
	if msg == nil {
		return nil, nil, proto.ErrNoMessage
	}
	return c, msg, nil
}

// maxQuoted is as much of a message's text as its quote reads: a quote
// shows a hundred characters, whatever the text a sender quotes.
const maxQuoted = 1024

// snippet is a one-line quote of text, its markers read. Only its start is
// read.
func snippet(text string) string {
	plain, _ := metatext.Parse(cutText(text, maxQuoted))
	return proto.Snippet(plain, 100)
}

// cutText is s cut to at most n bytes, on a character's boundary.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// label names a kind of media, for a reply's quote.
func label(k proto.MediaKind) string {
	switch k {
	case proto.Photo:
		return "Photo"
	case proto.Video:
		return "Video"
	case proto.GIF:
		return "GIF"
	case proto.Sticker:
		return "Sticker"
	case proto.Voice:
		return "Voice message"
	case proto.Audio:
		return "Audio"
	}
	return "File"
}

// quoteOf is how a reply shows msg.
func quoteOf(msg *message) string {
	switch {
	case msg.text != "":
		return snippet(msg.text)
	case len(msg.media) > 0:
		return label(msg.media[0].Kind)
	case msg.unsupported != "":
		return msg.unsupported
	}
	return ""
}
