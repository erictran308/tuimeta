// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"slices"
	"strconv"

	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
)

// reactionRef is what a reaction's own message id stands for: Instagram
// takes some reactions back by that id alone.
type reactionRef struct {
	chat   int64
	msg    string
	sender int64
}

// onDelta applies one update from the socket. Updates for a thread the
// backend doesn't know yet fetch it first, as the connector does; ones that
// only remove something from an unknown thread are dropped.
func (b *Instagram) onDelta(conn *connection, d *slidetypes.Delta) {
	threadIGID := d.ThreadIGID
	create := true
	switch e := d.Data.(type) {
	case slidetypes.UnknownEvent, slidetypes.IgnoredEvent, nil:
		return
	case *slidetypes.DeleteThreadEvent, *slidetypes.DeleteMessageEvent, *slidetypes.DeleteReactionEvent,
		*slidetypes.ParticipantLeaveEvent, *slidetypes.MarkReadEvent, *slidetypes.MarkUnreadEvent,
		*slidetypes.ReadReceiptEvent, *slidetypes.MuteThreadEvent, *slidetypes.EditMessageEvent,
		*slidetypes.PinThreadEvent, *slidetypes.PinMessageEvent, *slidetypes.AdminChangeEvent:
		create = false
	case *slidetypes.NewMessageEvent:
		// Some messages carry their thread only inside.
		if threadIGID == "" && e.Message != nil {
			threadIGID = e.Message.ThreadFBID
		}
	}
	c := b.chatFor(conn, threadIGID, create)
	if c == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != conn {
		return
	}
	switch e := d.Data.(type) {
	case *slidetypes.NewMessageEvent:
		b.onMessage(c, e.Message, true)
	case *slidetypes.AdminMessageEvent:
		b.onMessage(c, e.Message, !e.SkipBumpThread)
	case *slidetypes.EditMessageEvent:
		// The same text is your own edit coming back, already applied.
		if n := c.msgs[e.MessageID]; n != nil && n.text != e.TextBody {
			n.text = e.TextBody
			n.mentions = nil // their ranges were of the old text
			n.editCount++
			b.update(c, n)
		}
	case *slidetypes.CreateReactionEvent:
		if n := c.msgs[e.MessageID]; n != nil && e.Reaction.SenderFBID != 0 {
			n.setReaction(e.Reaction.SenderFBID, e.Reaction.Reaction)
			if e.Reaction.LogMessageID != "" {
				b.reactLogs[e.Reaction.LogMessageID] = reactionRef{c.key, n.netID, e.Reaction.SenderFBID}
			}
			b.update(c, n)
		}
	case *slidetypes.DeleteReactionEvent:
		target, sender := e.MessageID, e.Reaction.SenderFBID
		if sender == 0 {
			ref, ok := b.reactLogs[e.Reaction.LogMessageID]
			if !ok || ref.chat != c.key {
				return
			}
			target, sender = ref.msg, ref.sender
		}
		if n := c.msgs[target]; n != nil {
			n.setReaction(sender, "")
			b.update(c, n)
		}
	case *slidetypes.DeleteMessageEvent:
		b.removeMessage(c, e.MessageID)
	case *slidetypes.DeleteThreadEvent:
		b.removeChat(c)
	case *slidetypes.UpdateThreadFolderEvent:
		c.request = isRequestFolder(e.Folder) || isRequestFolder(e.IGInboxFolder)
		b.changed(c)
	case *slidetypes.UpdateThreadNameEvent:
		c.title = e.ThreadName
		b.eventMessage(c, e.Message)
	case *slidetypes.UpdateThreadImageEvent:
		c.photoURL = e.ThreadImage.URI
		b.eventMessage(c, e.Message)
	case *slidetypes.ParticipantJoinEvent:
		if t := e.Thread.AsIGDirectThread; t != nil {
			for _, u := range t.Users {
				if p := b.person(u); p != nil && p.fbid != b.selfFBID && !slices.Contains(c.members, p.fbid) {
					c.members = append(c.members, p.fbid)
				}
			}
			if t.ThreadTitle != "" {
				c.title = t.ThreadTitle
			}
			c.group = c.group || len(c.members) > 1
		}
		b.eventMessage(c, e.Message)
	case *slidetypes.ParticipantLeaveEvent:
		c.members = slices.DeleteFunc(c.members, func(id int64) bool { return id == e.LeftParticipantFBID })
		if e.LeftParticipantFBID == b.selfFBID {
			c.left = true
		}
		if t := e.Thread.AsIGDirectThread; t != nil && t.ThreadTitle != "" {
			c.title = t.ThreadTitle
		}
		b.eventMessage(c, e.Message)
	case *slidetypes.MarkReadEvent:
		// You read the chat elsewhere.
		inbox, _ := b.applyReceipt(c, b.selfFBID, millis(e.ReadTimestampMS))
		if inbox || c.markedUnread {
			c.markedUnread = false
			b.tellRead(c, true, false)
		}
	case *slidetypes.MarkUnreadEvent:
		c.markedUnread = e.MarkedAsUnread
		b.tellRead(c, false, false)
	case *slidetypes.ReadReceiptEvent:
		inbox, outbox := b.applyReceipt(c, e.ReadReceipt.ParticipantFBID, millis(e.ReadReceipt.WatermarkTimestampMS))
		if inbox || outbox {
			b.tellRead(c, inbox, outbox)
		}
	case *slidetypes.MuteThreadEvent:
		c.muted = e.IsMutedNow
		b.changed(c)
	}
}

// chatFor is the chat a thread IGID names, fetched from Instagram if it's
// new and create allows.
func (b *Instagram) chatFor(conn *connection, threadIGID string, create bool) *chat {
	if threadIGID == "" {
		return nil
	}
	b.mu.Lock()
	c := b.byIGID[threadIGID]
	b.mu.Unlock()
	if c != nil || !create {
		return c
	}
	resp, err := conn.cli.GetThread(withQuietLog(conn.ctx), slidetypes.MakeGetThreadInfoRequest(threadIGID))
	if err != nil || resp == nil || resp.ThreadInfo.AsIGDirectThread == nil {
		hlog.Warn("instagram: can't fetch a new thread", hlog.Kind(err))
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != conn {
		return nil
	}
	return b.threadPage(resp.ThreadInfo.AsIGDirectThread)
}

// threadPage keeps a thread fetched whole, with its newest page of history;
// b.mu is held.
func (b *Instagram) threadPage(t *slidetypes.ThreadInfo) *chat {
	known := b.chats[t.ThreadKey] != nil
	c := b.upsertThread(t, true)
	if c == nil {
		return nil
	}
	if !c.fetched && t.SlideMessages != nil {
		c.fetched = true
		c.cursor = t.SlideMessages.PageInfo.EndCursor
		c.log.SetComplete(!t.SlideMessages.PageInfo.HasNextPage)
	}
	if !known {
		b.tellChat(c)
	}
	return c
}

// onMessage takes a message from the socket; b.mu is held. bump says it
// moves the chat up the list.
func (b *Instagram) onMessage(c *chat, m *slidetypes.Message, bump bool) {
	n := b.convert(c, m)
	if n == nil || n.skip {
		// Instagram's "reacted to your message" notes repeat a reaction,
		// which comes as its own update.
		return
	}
	if st := b.sending[n.otid]; n.otid != "" && st != nil && st.chat == c {
		// The socket confirmed a message being sent before its request
		// answered: Send reports it, once, as sent.
		n = b.keep(c, n)
		b.putLog(c, n)
		st.got[n.otid] = n
		return
	}
	known := b.sent[c.id]
	n = b.keep(c, n)
	if bump {
		c.order = max(c.order, n.ms)
	}
	b.putLog(c, n)
	b.tellUser(b.people[n.sender])
	if !known {
		// tuimeta learns of the chat first, then of the message in it.
		b.tellChat(c)
	}
	for _, p := range b.parts(c, n) {
		b.d.Events.Message(p)
	}
	if known {
		b.tellChat(c)
	}
}

// eventMessage is the message that tells of a change to the chat (renamed,
// someone joined), if Instagram sent one, and the chat's new state; b.mu is
// held.
func (b *Instagram) eventMessage(c *chat, m *slidetypes.Message) {
	if m != nil {
		b.onMessage(c, m, true)
		return
	}
	b.changed(c)
}

// putLog puts a message in its chat's history; b.mu is held.
func (b *Instagram) putLog(c *chat, n *netMsg) {
	c.log.Put(b.parts(c, n)...)
	n.inLog = true
}

// update reports a changed message, if tuimeta can have it, and the chat if
// it's the newest; b.mu is held.
func (b *Instagram) update(c *chat, n *netMsg) {
	if n.inLog {
		parts := b.parts(c, n)
		c.log.Put(parts...)
		for _, p := range parts {
			b.d.Events.Message(p)
		}
	}
	if c.last == n {
		b.changed(c)
	}
}

// changed sends a chat's new state if tuimeta has the chat; b.mu is held.
func (b *Instagram) changed(c *chat) {
	if b.sent[c.id] {
		b.tellChat(c)
	}
}

// tellRead reports read positions and the unread count; b.mu is held.
func (b *Instagram) tellRead(c *chat, inbox, outbox bool) {
	if !b.sent[c.id] {
		return
	}
	var in, out int64
	if inbox {
		in = pos(c.readInbox)
	}
	if outbox {
		out = pos(c.readOutbox)
	}
	unread := c.unread(b.selfFBID)
	b.d.Events.Read(c.id, in, out, &unread)
}

// removeMessage forgets an unsent or deleted message; b.mu is held.
func (b *Instagram) removeMessage(c *chat, netID string) {
	var ids []int64
	if n := c.msgs[netID]; n != nil {
		ids = n.ids
		delete(c.msgs, netID)
		if c.last == n {
			c.last = nil
			for _, m := range c.msgs {
				if m.counted && (c.last == nil || m.ms > c.last.ms) {
					c.last = m
				}
			}
		}
	} else if known, ok := b.d.Messages.Known(c.id, netID); ok {
		ids = known
	}
	if len(ids) == 0 {
		return
	}
	c.log.Remove(ids...)
	if b.sent[c.id] {
		b.d.Events.MessageDeleted(c.id, ids)
	}
	b.changed(c)
}

// removeChat forgets a chat Instagram deleted (for you); b.mu is held.
func (b *Instagram) removeChat(c *chat) {
	delete(b.chats, c.key)
	if b.byIGID[c.igid] == c {
		delete(b.byIGID, c.igid)
	}
	if b.byLongID[c.longID] == c {
		delete(b.byLongID, c.longID)
	}
	if b.sent[c.id] {
		delete(b.sent, c.id)
		b.d.Events.ChatRemoved(c.id)
	}
}

// onTyping reports someone typing. Typing updates name the thread by its
// long id and the person by their Instagram user id.
func (b *Instagram) onTyping(e *slidetypes.TypingNotification) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.byLongID[e.ThreadID]
	if c == nil || !b.sent[c.id] {
		return
	}
	fbid := b.byIGUser[strconv.FormatInt(e.SenderID, 10)]
	if fbid == 0 || fbid == b.selfFBID {
		return
	}
	b.d.Events.Typing(c.id, b.userID(fbid), e.ActivityStatus != 0)
}
