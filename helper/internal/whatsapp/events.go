// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strconv"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// onEvent handles whatsmeow's events. It returns false only when a message
// couldn't be kept, so whatsmeow doesn't acknowledge it and WhatsApp
// delivers it again.
func (w *WhatsApp) onEvent(gen int, evt any) (ok bool) {
	defer func() {
		if v := recover(); v != nil {
			hlog.Recovered("whatsapp event", v)
			ok = true
		}
	}()
	switch e := evt.(type) {
	case *events.Message:
		return w.receive(gen, e)
	case *events.UndecryptableMessage:
		return w.undecryptable(gen, e)
	case *events.Receipt:
		w.receipt(gen, e)
	case *events.ChatPresence:
		w.chatPresence(gen, e)
	case *events.HistorySync:
		w.historySync(gen, e)
	case *events.GroupInfo:
		w.groupInfo(gen, e)
	case *events.JoinedGroup:
		w.joinedGroup(gen, e)
	case *events.Mute:
		w.mute(gen, e)
	case *events.Archive:
		w.archive(gen, e)
	case *events.Pin:
		w.pin(gen, e)
	case *events.MarkChatAsRead:
		w.markChatAsRead(gen, e)
	case *events.DeleteChat:
		w.deleteChat(gen, e.JID)
	case *events.ClearChat:
		w.clearChat(gen, e.JID)
	case *events.DeleteForMe:
		w.deleteForMe(gen, e)
	case *events.Contact:
		w.renamed(gen, e.JID, waTypes.EmptyJID)
	case *events.PushName:
		w.renamed(gen, e.JID, e.JIDAlt)
	case *events.BusinessName:
		w.renamed(gen, e.JID, waTypes.EmptyJID)
	case *events.PushNameSetting:
		w.ownName(gen, e.Action.GetName())
	case *events.Picture:
		w.picture(gen, e)
	case *events.AppStateSyncComplete:
		w.renameAll(gen)
	case *events.Connected:
		hlog.Info("whatsapp: connected")
		w.becameReady(gen)
	case *events.Disconnected:
		hlog.Info("whatsapp: disconnected; reconnecting")
		w.lost(gen)
	case *events.KeepAliveTimeout:
		hlog.Info("whatsapp: keepalive timed out", hlog.Int("count", int64(e.ErrorCount)))
	case *events.LoggedOut:
		hlog.Warn("whatsapp: device unlinked", hlog.Int("reason", int64(e.Reason)))
		w.unlinkedRemotely(gen)
	case *events.StreamReplaced:
		hlog.Warn("whatsapp: replaced by another connection")
		w.stopped(gen, "WhatsApp was opened by another copy of tuimeta with the same link; close it, then restart this one.")
	case *events.ConnectFailure:
		hlog.Warn("whatsapp: connection refused", hlog.Int("reason", int64(e.Reason)))
		switch e.Reason {
		case events.ConnectFailureLoggedOut, events.ConnectFailureMainDeviceGone, events.ConnectFailureUnknownLogout:
			w.unlinkedRemotely(gen)
		}
	case *events.TemporaryBan:
		hlog.Warn("whatsapp: temporarily banned", hlog.Int("code", int64(e.Code)))
		msg := "WhatsApp has blocked this account for a while"
		if e.Expire > 0 {
			msg += "; it can be used again in " + e.Expire.Round(time.Minute).String() + ", after a restart of tuimeta"
		}
		w.stopped(gen, msg+".")
	case *events.ClientOutdated:
		hlog.Warn("whatsapp: client outdated")
		w.stopped(gen, "WhatsApp says this version of tuimeta is too old; update tuimeta.")
	}
	return true
}

// lockFor takes w.mu for an event of connection gen. It's false, and the
// lock let go, when the event is from an older connection (or the helper is
// quitting): nothing of it may reach tuimeta.
func (w *WhatsApp) lockFor(gen int) bool {
	ok, _ := w.lockForMessage(gen)
	return ok
}

// lockForMessage is lockFor for a message, and says whether one that can't
// be handled may still be acknowledged: not while the helper quits (it
// comes again next run, rather than being lost), but yes for a connection
// logged out or replaced (whose account is gone).
func (w *WhatsApp) lockForMessage(gen int) (ok, ack bool) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false, false
	}
	if w.gen != gen {
		w.mu.Unlock()
		return false, true
	}
	return true, true
}

// lost reports the connection gone for now; whatsmeow reconnects.
func (w *WhatsApp) lost(gen int) {
	if !w.lockFor(gen) {
		return
	}
	current := w.ready
	w.ready = false
	w.mu.Unlock()
	if current {
		w.d.Events.Account(net, proto.Connecting, 0, "", "")
	}
}

// stopped handles WhatsApp ending the connection for good (another copy of
// the device connected, a ban, an outdated client): whatsmeow won't
// reconnect, so neither does this run.
func (w *WhatsApp) stopped(gen int, why string) {
	if !w.lockFor(gen) {
		return
	}
	w.teardownLocked()
	w.mu.Unlock()
	w.d.Events.Account(net, proto.Errored, 0, "", why)
}

// unlinkedRemotely handles the phone (or WhatsApp) removing this device:
// it can't connect again, so it's dropped; what's kept stays until the user
// logs out or links again.
func (w *WhatsApp) unlinkedRemotely(gen int) {
	if !w.lockFor(gen) {
		return
	}
	w.teardownLocked()
	// Kept under the lock, so a logout's wipe isn't followed by it.
	if err := w.d.Session.SaveJSON(sessionFile, savedSession{Version: 1}); err != nil {
		hlog.Error("whatsapp: can't save session", hlog.Kind(err))
	}
	w.mu.Unlock()
	w.d.Events.Account(net, proto.Errored, 0, "", unlinked)
}

// skipped reports whether a chat isn't one tuimeta shows: status updates
// and channels.
func skipped(chat waTypes.JID) bool {
	return chat == waTypes.StatusBroadcastJID || chat.Server == waTypes.NewsletterServer
}

// chatKey is the key of the chat a message belongs to: a group's JID, or
// the other person's key in a one-to-one chat. A message someone sent to a
// broadcast list you're on belongs in your chat with them, as on the phone;
// one you sent to a list has no chat here (ok is false). w.mu is held.
func (w *WhatsApp) chatKey(src waTypes.MessageSource) (key string, group, ok bool) {
	switch {
	case skipped(src.Chat):
		return "", false, false
	case src.Chat.Server == waTypes.GroupServer:
		return src.Chat.String(), true, true
	case src.Chat.Server == waTypes.BroadcastServer:
		if src.IsFromMe {
			return "", false, false
		}
		w.pair(src.Sender, src.SenderAlt)
		return w.canon(src.Sender), false, true
	}
	if src.IsFromMe {
		w.pair(src.Chat, src.RecipientAlt)
	} else {
		w.pair(src.Sender, src.SenderAlt)
		w.pair(src.Chat, src.SenderAlt)
	}
	return w.canon(src.Chat), false, true
}

// senderKey is the key of a message's sender; w.mu is held.
func (w *WhatsApp) senderKey(src waTypes.MessageSource) string {
	if src.IsFromMe {
		return w.self
	}
	w.pair(src.Sender, src.SenderAlt)
	return w.canon(src.Sender)
}

// clamp keeps a timestamp from the future (WhatsApp sometimes sends ones
// years ahead) from pinning its chat to the top: ms if it's not past now
// by more than a few minutes, else now; w.mu is held.
func (w *WhatsApp) clamp(ms int64) int64 {
	now := w.now()
	if ms > now.Add(5*time.Minute).UnixMilli() {
		return now.UnixMilli()
	}
	return ms
}

// clampMsg keeps a message's time from the future, its disappearing time
// moved along with it; w.mu is held.
func (w *WhatsApp) clampMsg(m *message) {
	if c := w.clamp(m.MS); c != m.MS {
		if m.Expires > 0 {
			m.Expires -= m.MS - c
		}
		m.MS = c
	}
}

// receive handles a message as it arrives. It's false when the message
// couldn't be kept, so it comes again.
func (w *WhatsApp) receive(gen int, e *events.Message) bool {
	if skipped(e.Info.Chat) {
		return true
	}
	if enc := e.Message.GetEncReactionMessage(); enc != nil {
		cli, err := w.connected()
		if err != nil {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), dbWait)
		dec, err := cli.DecryptReaction(ctx, e)
		cancel()
		if err != nil {
			hlog.Info("whatsapp: reaction couldn't be decrypted", hlog.Kind(err))
			return true
		}
		dec.Key = enc.GetTargetMessageKey()
		e.Message = &waE2E.Message{ReactionMessage: dec}
	}
	if ok, ack := w.lockForMessage(gen); !ok {
		return ack
	}
	defer w.mu.Unlock()
	defer w.scrubbed()
	key, group, ok := w.chatKey(e.Info.MessageSource)
	if !ok {
		return true
	}
	w.storeFailed = false
	sender := w.senderKey(e.Info.MessageSource)
	ch := reader{canon: w.canon}.read(e, sender)
	w.apply(key, group, sender, ch)
	return !w.storeFailed
}

// apply applies what a message did to its chat; w.mu is held.
func (w *WhatsApp) apply(key string, group bool, sender string, ch change) {
	switch ch.kind {
	case added:
		c := w.chatFor(key, group)
		w.clampMsg(ch.msg)
		if ch.msg.Expires > 0 && ch.msg.Expires <= w.now().UnixMilli() {
			return // it has already disappeared
		}
		if group && !c.known {
			w.askGroup(c, false)
		}
		if ch.timer != nil {
			c.Ephemeral = *ch.timer
		}
		w.arrived(c, ch.msg)
	case edited:
		c := w.chats[w.resolve(key)]
		if c == nil {
			return
		}
		w.ensureLoaded(c)
		msg := c.msgs[ch.target]
		if msg == nil || !w.same(msg.Sender, sender) || ch.msg.MS <= msg.EditMS {
			return // only its sender edits a message, and an older edit never undoes a newer one
		}
		msg.Text, msg.Mentions, msg.Preview = ch.msg.Text, ch.msg.Mentions, ch.msg.Preview
		msg.Edited, msg.EditMS = true, ch.msg.MS
		w.changed(c, msg)
	case revoked:
		c := w.chats[w.resolve(key)]
		if c == nil {
			return
		}
		w.ensureLoaded(c)
		msg := c.msgs[ch.target]
		if msg == nil {
			return
		}
		// Its sender deletes a message, or in a group one of its admins.
		// The deletion comes end to end encrypted, so WhatsApp's servers
		// can't check that: it's checked here, as the official apps do.
		if !w.same(msg.Sender, sender) && !(group && w.isAdmin(c, sender)) {
			return
		}
		w.deleted(c, msg.ID)
	case reacted:
		c := w.chats[w.resolve(key)]
		if c == nil {
			return
		}
		w.ensureLoaded(c)
		if msg := c.msgs[ch.target]; msg != nil && msg.Service == nil && msg.setReaction(w.resolve(sender), ch.emoji) {
			w.changed(c, msg)
		}
	}
}

// isAdmin reports whether key is one of c's admins, as WhatsApp last said;
// w.mu is held.
func (w *WhatsApp) isAdmin(c *chat, key string) bool {
	return slices.ContainsFunc(c.Admins, func(a string) bool { return w.same(a, key) })
}

// undecryptable shows a message that couldn't be decrypted yet; whatsmeow
// asks the phone and the sender for it again, and when it comes it takes
// this one's place.
func (w *WhatsApp) undecryptable(gen int, e *events.UndecryptableMessage) bool {
	if e.DecryptFailMode == events.DecryptFailHide {
		return true
	}
	if ok, ack := w.lockForMessage(gen); !ok {
		return ack
	}
	defer w.mu.Unlock()
	key, group, ok := w.chatKey(e.Info.MessageSource)
	if !ok {
		return true
	}
	c := w.chatFor(key, group)
	w.ensureLoaded(c)
	if c.msgs[e.Info.ID] != nil {
		return true
	}
	text := "[Waiting for this message; it couldn't be decrypted yet]"
	if e.UnavailableType == events.UnavailableTypeViewOnce {
		text = "[View once message: open it on your phone]"
	}
	w.storeFailed = false
	w.arrived(c, &message{ID: e.Info.ID, Sender: w.senderKey(e.Info.MessageSource), MS: w.clamp(e.Info.Timestamp.UnixMilli()), Unsupported: text, Placeholder: true})
	return !w.storeFailed
}

// dmOrGroup is the key of the chat a receipt or a typing notice is about;
// w.mu is held.
func (w *WhatsApp) dmOrGroup(chat, sender, senderAlt waTypes.JID) string {
	if chat.Server == waTypes.GroupServer {
		return chat.String()
	}
	w.pair(sender, senderAlt)
	return w.canon(chat)
}

// receipt handles read receipts: someone read your messages, or you read a
// chat on another of your devices.
func (w *WhatsApp) receipt(gen int, e *events.Receipt) {
	if e.Type != waTypes.ReceiptTypeRead && e.Type != waTypes.ReceiptTypeReadSelf && e.Type != waTypes.ReceiptTypePlayed {
		return
	}
	if skipped(e.Chat) || e.Chat.Server == waTypes.BroadcastServer {
		return
	}
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	c := w.chats[w.resolve(w.dmOrGroup(e.Chat, e.Sender, e.SenderAlt))]
	if c == nil {
		return
	}
	w.ensureLoaded(c)
	var newest int64
	for _, id := range e.MessageIDs {
		if m := c.msgs[id]; m != nil {
			newest = max(newest, m.MS)
		}
	}
	if newest == 0 {
		newest = w.clamp(e.Timestamp.UnixMilli())
	}
	if e.IsFromMe {
		// Read on another of your devices. A "read-self" receipt from
		// anyone else says nothing about what you read.
		if e.Type == waTypes.ReceiptTypeRead || e.Type == waTypes.ReceiptTypeReadSelf {
			w.readUpTo(c, newest)
		}
		return
	}
	if e.Type == waTypes.ReceiptTypeReadSelf {
		return
	}
	if newest > c.TheirRead {
		c.TheirRead = newest
		w.saveChat(c)
		w.d.Events.Read(c.id, 0, positionOf(newest), nil)
	}
}

// positionOf is the protocol position of millisecond ms: after every
// message sent in it.
func positionOf(ms int64) int64 { return ms<<8 | 255 }

// readUpTo moves how far you've read c (here or on another device), and
// ends its being marked unread; w.mu is held.
func (w *WhatsApp) readUpTo(c *chat, ms int64) {
	if ms <= c.ReadUpTo && !c.MarkedUnread {
		return
	}
	c.ReadUpTo = max(c.ReadUpTo, ms)
	c.MarkedUnread = false
	w.recount(c)
	w.saveChat(c)
	unread := c.Unread
	w.d.Events.Read(c.id, positionOf(c.ReadUpTo), 0, &unread)
	w.touch(c)
}

// chatPresence reports someone typing. WhatsApp sends it only to devices
// that say they're online, which this one never does, so it seldom comes.
func (w *WhatsApp) chatPresence(gen int, e *events.ChatPresence) {
	if skipped(e.Chat) || e.Chat.Server == waTypes.BroadcastServer {
		return
	}
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	c := w.chats[w.resolve(w.dmOrGroup(e.Chat, e.Sender, e.SenderAlt))]
	who := w.canon(e.Sender)
	if c == nil || w.isSelf(who) {
		return
	}
	p := w.person(who)
	w.tellUser(p, false)
	w.d.Events.Typing(c.id, p.id, e.State == waTypes.ChatPresenceComposing)
}

// historySync takes in what the phone sent: its recent chats when this
// device was linked, and older messages asked for since.
func (w *WhatsApp) historySync(gen int, e *events.HistorySync) {
	data := e.Data
	switch data.GetSyncType() {
	case waHistorySync.HistorySync_INITIAL_BOOTSTRAP, waHistorySync.HistorySync_RECENT,
		waHistorySync.HistorySync_FULL, waHistorySync.HistorySync_ON_DEMAND:
	case waHistorySync.HistorySync_PUSH_NAME:
		w.renameAll(gen)
		return
	default:
		return
	}
	onDemand := data.GetSyncType() == waHistorySync.HistorySync_ON_DEMAND
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	cli := w.cli
	if cli == nil {
		return
	}
	for _, mp := range data.GetPhoneNumberToLidMappings() {
		pn, err1 := waTypes.ParseJID(mp.GetPnJID())
		lid, err2 := waTypes.ParseJID(mp.GetLidJID())
		if err1 == nil && err2 == nil {
			w.pair(pn, lid)
		}
	}
	count := 0
	for _, conv := range data.GetConversations() {
		jid, err := waTypes.ParseJID(conv.GetID())
		if err != nil || skipped(jid) || jid.Server == waTypes.BroadcastServer {
			continue
		}
		count += w.conversation(cli, jid, conv, onDemand)
	}
	hlog.Info("whatsapp: history taken in", hlog.Int("type", int64(data.GetSyncType())), hlog.Int("messages", int64(count)))
}

// conversation takes in one chat of a history sync; w.mu is held. It
// returns how many messages it kept.
func (w *WhatsApp) conversation(cli waAPI, jid waTypes.JID, conv *waHistorySync.Conversation, onDemand bool) int {
	group := jid.Server == waTypes.GroupServer
	if !group {
		if pn, err := waTypes.ParseJID(conv.GetPnJID()); err == nil {
			w.pair(jid, pn)
		}
		if lid, err := waTypes.ParseJID(conv.GetLidJID()); err == nil {
			w.pair(jid, lid)
		}
	}
	key := jid.String()
	if !group {
		key = w.canon(jid)
	}
	c := w.chatFor(key, group)
	w.ensureLoaded(c)
	c.known = true
	now := w.now().UnixMilli()
	// The phone lists a chat's messages newest first: they're taken oldest
	// first, so messages of the same second keep their order.
	msgs := slices.Clone(conv.GetMessages())
	slices.Reverse(msgs)
	slices.SortStableFunc(msgs, func(a, b *waHistorySync.HistorySyncMsg) int {
		return cmp.Compare(a.GetMessage().GetMessageTimestamp(), b.GetMessage().GetMessageTimestamp())
	})
	var batch []*message
	for _, hm := range msgs {
		web := hm.GetMessage()
		if web == nil || web.GetKey().GetID() == "" {
			continue
		}
		id := web.GetKey().GetID()
		ms := w.clamp(int64(web.GetMessageTimestamp()) * 1000)
		if web.GetMessage() == nil {
			actor := ""
			if p, err := waTypes.ParseJID(firstNonEmpty(web.GetParticipant(), web.GetKey().GetParticipant())); err == nil && !p.IsEmpty() {
				actor = w.canon(p)
			} else if web.GetKey().GetFromMe() {
				actor = w.self
			} else if !group {
				actor = c.Other // a missed call in a dm is from the other person
			}
			m := (reader{canon: w.canon}).stub(web, id, actor, ms)
			if m != nil && c.msgs[id] == nil {
				if m.Sender == "" {
					m.Sender = firstNonEmpty(firstOf(m.Service.Targets), w.self)
				}
				batch = append(batch, m)
			}
			continue
		}
		evt, err := cli.ParseWebMessage(jid, web)
		if err != nil {
			continue
		}
		sender := w.senderKey(evt.Info.MessageSource)
		ch := reader{canon: w.canon}.read(evt, sender)
		if ch.kind != added {
			continue
		}
		msg := ch.msg
		w.clampMsg(msg)
		if msg.Expires > 0 && msg.Expires <= now {
			continue
		}
		if old := c.msgs[msg.ID]; old != nil {
			if !w.same(old.Sender, msg.Sender) || !old.Placeholder || msg.Placeholder {
				continue // already here; what came live is as new
			}
			msg.Reactions = old.Reactions
		}
		for _, r := range web.GetReactions() {
			actor := w.self
			if !r.GetKey().GetFromMe() {
				p, err := waTypes.ParseJID(firstNonEmpty(r.GetKey().GetParticipant(), r.GetKey().GetRemoteJID()))
				if err != nil || p.IsEmpty() {
					continue
				}
				if !group {
					p = jid
				}
				actor = w.canon(p)
			}
			msg.setReaction(w.resolve(actor), r.GetText())
		}
		batch = append(batch, msg)
	}
	for _, m := range batch {
		w.keep(c, m)
		c.Activity = max(c.Activity, m.MS)
	}
	w.store(c, batch...)
	if !onDemand {
		w.syncedState(c, conv)
	} else if len(batch) == 0 || conv.GetEndOfHistoryTransferType() == waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY {
		c.Complete = true
		c.log.SetComplete(true)
	}
	if ts := int64(conv.GetConversationTimestamp()) * 1000; ts > 0 {
		c.Activity = max(c.Activity, w.clamp(ts))
	}
	w.saveChat(c)
	w.touch(c)
	if onDemand {
		w.wake(c.key)
	}
	return len(batch)
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// syncedState takes a chat's state from the phone's sync: its name,
// archive, mute, timer and how much of it is unread. The phone sends that
// as it was when this device was linked, in blobs that may come minutes
// apart, so only the first one a chat appears in counts: by the later ones
// the chat may have been read, muted or archived since. w.mu is held.
func (w *WhatsApp) syncedState(c *chat, conv *waHistorySync.Conversation) {
	if conv.GetEndOfHistoryTransferType() == waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY {
		c.Complete = true
		c.log.SetComplete(true)
	}
	if c.Synced {
		return
	}
	c.Synced = true
	if c.kind == proto.Group {
		if name := oneLine(conv.GetName()); name != "" {
			c.Name = name
		}
		if ps := conv.GetParticipant(); len(ps) > 0 {
			c.Members, c.Admins = c.Members[:0], c.Admins[:0]
			for _, p := range ps {
				j, err := waTypes.ParseJID(p.GetUserJID())
				if err != nil || j.IsEmpty() {
					continue
				}
				key := w.canon(j)
				c.Members = append(c.Members, key)
				if p.GetRank() != waHistorySync.GroupParticipant_REGULAR {
					c.Admins = append(c.Admins, key)
				}
			}
		}
	}
	c.Archived = conv.GetArchived()
	c.Pinned = conv.GetPinned() > 0
	c.ReadOnly = conv.GetReadOnly()
	c.Ephemeral = conv.GetEphemeralExpiration()
	switch end := int64(conv.GetMuteEndTime()); {
	case end == 0:
		c.MuteUntil = 0
	case end < 0 || end > w.now().AddDate(50, 0, 0).Unix():
		c.MuteUntil = -1
	default:
		c.MuteUntil = end * 1000
	}
	// WhatsApp says how many messages are unread, not up to where: the
	// newest that many of others' messages are unread, the rest read. When
	// it's more than this device has, all of them are, and older ones that
	// come later aren't.
	unread := int(conv.GetUnreadCount())
	c.MarkedUnread = conv.GetMarkedAsUnread()
	var theirs []*message
	for _, m := range c.msgs {
		if !w.isSelf(m.Sender) && m.Service == nil {
			theirs = append(theirs, m)
		}
	}
	slices.SortFunc(theirs, func(a, b *message) int { return cmp.Compare(b.MS, a.MS) })
	switch {
	case unread <= 0 && len(theirs) > 0:
		c.ReadUpTo = max(c.ReadUpTo, theirs[0].MS)
	case unread > 0 && unread < len(theirs):
		c.ReadUpTo = max(c.ReadUpTo, theirs[unread].MS)
	case unread > 0 && len(theirs) > 0:
		c.ReadUpTo = max(c.ReadUpTo, theirs[len(theirs)-1].MS-1)
	}
	w.recount(c)
	if unread > c.Unread {
		c.Unread = unread // more than this device has of them
	}
}

// wake ends the waits for an older page of the chat with key; w.mu is held.
func (w *WhatsApp) wake(key string) {
	for _, ch := range w.waiters[key] {
		close(ch)
	}
	delete(w.waiters, key)
}

// eventID is the id of an event made here (they have none on WhatsApp):
// its time and a random part, so two in one second don't collide.
func eventID(ts time.Time) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "event:" + strconv.FormatInt(ts.UnixMilli(), 10) + ":" + hex.EncodeToString(b)
}

// groupInfo applies a change to a group: its name, who joined or left, who
// became an admin. These come as notifications, not messages, so they're
// shown as events of their own.
func (w *WhatsApp) groupInfo(gen int, e *events.GroupInfo) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	c := w.chats[e.JID.String()]
	if c == nil {
		return
	}
	actor := ""
	if e.Sender != nil {
		if e.SenderPN != nil {
			w.pair(*e.Sender, *e.SenderPN)
		}
		actor = w.canon(*e.Sender)
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = w.now()
	}
	var events []*service
	if e.Name != nil && e.Name.Name != c.Name {
		c.Name = oneLine(e.Name.Name)
		events = append(events, &service{Kind: "rename", Actor: actor, Text: c.Name})
	}
	if e.Topic != nil {
		events = append(events, &service{Kind: "description", Actor: actor})
	}
	if e.Announce != nil {
		w.askGroup(c, true)
	}
	if e.Ephemeral != nil {
		secs := uint32(0)
		if e.Ephemeral.IsEphemeral {
			secs = e.Ephemeral.DisappearingTimer
		}
		c.Ephemeral = secs
		events = append(events, timerService(actor, secs))
	}
	for _, j := range e.Promote {
		if key := w.canon(j); !w.isAdmin(c, key) {
			c.Admins = append(c.Admins, key)
		}
	}
	for _, j := range e.Demote {
		key := w.canon(j)
		c.Admins = slices.DeleteFunc(c.Admins, func(a string) bool { return w.same(a, key) })
	}
	var joined, left []string
	for _, j := range e.Join {
		key := w.canon(j)
		if !slices.Contains(c.Members, key) {
			c.Members = append(c.Members, key)
		}
		joined = append(joined, key)
	}
	for _, j := range e.Leave {
		key := w.canon(j)
		c.Members = slices.DeleteFunc(c.Members, func(k string) bool { return w.same(k, key) })
		c.Admins = slices.DeleteFunc(c.Admins, func(k string) bool { return w.same(k, key) })
		if w.isSelf(key) {
			c.ReadOnly = true
		}
		left = append(left, key)
	}
	if len(joined) > 0 {
		events = append(events, &service{Kind: "add", Actor: actor, Targets: joined})
	}
	if len(left) > 0 {
		kind := "remove"
		if actor == "" || (len(left) == 1 && w.same(left[0], actor)) {
			kind = "leave"
		}
		events = append(events, &service{Kind: kind, Actor: actor, Targets: left})
	}
	for _, s := range events {
		w.arrived(c, &message{ID: eventID(ts), Sender: firstNonEmpty(actor, firstOf(s.Targets), w.self), MS: w.clamp(ts.UnixMilli()), Service: s})
	}
	w.saveChat(c)
	w.touch(c)
}

// joinedGroup adds a group you were added to or created.
func (w *WhatsApp) joinedGroup(gen int, e *events.JoinedGroup) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	c := w.chatFor(e.JID.String(), true)
	w.groupDetails(c, &e.GroupInfo)
	c.known = true
	c.Activity = max(c.Activity, w.now().UnixMilli())
	w.saveChat(c)
	w.touch(c)
}

// groupDetails takes a group's name, members and admins from WhatsApp; w.mu
// is held.
func (w *WhatsApp) groupDetails(c *chat, info *waTypes.GroupInfo) {
	if info.Name != "" {
		c.Name = oneLine(info.Name)
	}
	c.Members, c.Admins = c.Members[:0], c.Admins[:0]
	admin := false
	for _, p := range info.Participants {
		if !p.PhoneNumber.IsEmpty() {
			w.pair(p.JID, p.PhoneNumber)
		}
		key := w.canon(p.JID)
		c.Members = append(c.Members, key)
		if p.IsAdmin || p.IsSuperAdmin {
			c.Admins = append(c.Admins, key)
		}
		if w.isSelf(key) {
			admin = p.IsAdmin || p.IsSuperAdmin
		}
	}
	c.ReadOnly = info.IsAnnounce && !admin
	if info.IsEphemeral {
		c.Ephemeral = info.DisappearingTimer
	}
}

// askGroup asks WhatsApp for a group's details, once (or again when force);
// w.mu is held.
func (w *WhatsApp) askGroup(c *chat, force bool) {
	if w.cli == nil || (w.asked[c.key] && !force) {
		return
	}
	w.asked[c.key] = true
	cli, gen, jid := w.cli, w.gen, c.jid()
	hlog.Go("whatsapp group", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		info, err := cli.GetGroupInfo(ctx, jid)
		cancel()
		if err != nil {
			hlog.Info("whatsapp: group lookup failed", hlog.Kind(err))
			return
		}
		if !w.lockFor(gen) {
			return
		}
		defer w.mu.Unlock()
		if c := w.chats[jid.String()]; c != nil {
			w.groupDetails(c, info)
			c.known = true
			w.saveChat(c)
			w.touch(c)
		}
	})
}

// chatByJID is the known chat with a JID from an app-state event; w.mu is
// held.
func (w *WhatsApp) chatByJID(j waTypes.JID) *chat {
	if j.Server == waTypes.GroupServer {
		return w.chats[j.String()]
	}
	return w.chats[w.canon(j)]
}

func (w *WhatsApp) mute(gen int, e *events.Mute) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	c := w.chatByJID(e.JID)
	if c == nil {
		return
	}
	until := int64(0)
	if e.Action.GetMuted() {
		until = e.Action.GetMuteEndTimestamp()
		if until <= 0 {
			until = -1
		}
	}
	if c.MuteUntil != until {
		c.MuteUntil = until
		w.saveChat(c)
		w.touch(c)
	}
}

func (w *WhatsApp) archive(gen int, e *events.Archive) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	if c := w.chatByJID(e.JID); c != nil && c.Archived != e.Action.GetArchived() {
		c.Archived = e.Action.GetArchived()
		w.saveChat(c)
		w.touch(c)
	}
}

func (w *WhatsApp) pin(gen int, e *events.Pin) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	if c := w.chatByJID(e.JID); c != nil {
		c.Pinned = e.Action.GetPinned()
		w.saveChat(c)
	}
}

// markChatAsRead is a chat marked read or unread on another device.
func (w *WhatsApp) markChatAsRead(gen int, e *events.MarkChatAsRead) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	c := w.chatByJID(e.JID)
	if c == nil {
		return
	}
	if !e.Action.GetRead() {
		if !c.MarkedUnread {
			c.MarkedUnread = true
			w.saveChat(c)
			w.touch(c)
		}
		return
	}
	upTo := e.Action.GetMessageRange().GetLastMessageTimestamp() * 1000
	if upTo <= 0 {
		upTo = e.Timestamp.UnixMilli()
	}
	w.readUpTo(c, w.clamp(upTo))
}

// deleteChat is a chat deleted on the phone: it goes here too.
func (w *WhatsApp) deleteChat(gen int, j waTypes.JID) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	defer w.scrubbed()
	if c := w.chatByJID(j); c != nil {
		w.removeChat(c)
	}
}

// clearChat is a chat emptied on the phone: its messages go here too.
func (w *WhatsApp) clearChat(gen int, j waTypes.JID) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	defer w.scrubbed()
	c := w.chatByJID(j)
	if c == nil {
		return
	}
	w.ensureLoaded(c)
	var gone []int64
	for _, m := range c.msgs {
		gone = append(gone, m.ids...)
		w.forgetFiles(m)
	}
	c.log.Remove(gone...)
	c.msgs = map[string]*message{}
	if w.st != nil {
		ctx, cancel := dbCtx()
		if err := w.st.clearChat(ctx, c.key); err != nil {
			hlog.Error("whatsapp: can't clear a chat", hlog.Kind(err))
		}
		cancel()
		w.scrub = true
	}
	w.d.Events.MessageDeleted(c.id, gone)
	c.Unread = 0
	w.saveChat(c)
	w.touch(c)
}

// deleteForMe is a message deleted on the phone for you only: it goes here
// too.
func (w *WhatsApp) deleteForMe(gen int, e *events.DeleteForMe) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	defer w.scrubbed()
	if c := w.chatByJID(e.ChatJID); c != nil {
		w.ensureLoaded(c)
		w.deleted(c, e.MessageID)
	}
}

// renamed refreshes what a person is called after their contact, push or
// business name changed.
func (w *WhatsApp) renamed(gen int, j, alt waTypes.JID) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	w.pair(j, alt)
	key := w.canon(j)
	if p := w.people[w.resolve(key)]; p != nil && w.refreshName(p) && p.told {
		w.tellUser(p, true)
		if c := w.chats[p.key]; c != nil && c.kind == proto.DM {
			w.touch(c)
		}
	}
}

// renameAll refreshes everyone's names once the phone sent its address
// book.
func (w *WhatsApp) renameAll(gen int) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	for _, p := range w.people {
		if w.refreshName(p) && p.told {
			w.tellUser(p, true)
			if c := w.chats[p.key]; c != nil && c.kind == proto.DM {
				w.touch(c)
			}
		}
	}
}

// ownName is your name as you set it on WhatsApp.
func (w *WhatsApp) ownName(gen int, name string) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	if name == "" || name == w.selfName {
		return
	}
	w.selfName = name
	if p := w.people[w.resolve(w.self)]; p != nil && w.refreshName(p) && p.told {
		w.tellUser(p, true)
	}
}

// picture is a person's or group's new picture: its file changes, so it's
// fetched again when shown.
func (w *WhatsApp) picture(gen int, e *events.Picture) {
	if !w.lockFor(gen) {
		return
	}
	defer w.mu.Unlock()
	id := e.PictureID
	if e.Remove {
		id = "removed"
	}
	if e.JID.Server == waTypes.GroupServer {
		if c := w.chats[e.JID.String()]; c != nil && c.PictureID != id {
			c.PictureID = id
			w.saveChat(c)
			w.touch(c)
		}
		return
	}
	if p := w.people[w.resolve(w.canon(e.JID))]; p != nil && p.pictureID != id {
		p.pictureID = id
		if p.told {
			w.tellUser(p, true)
		}
		if c := w.chats[p.key]; c != nil {
			w.touch(c)
		}
	}
}
