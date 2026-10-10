// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// tableSource says where a table came from.
type tableSource int

const (
	fromInitial  tableSource = iota // the Messenger page's own data, at connect
	fromSocket                      // pushed on the socket
	fromResponse                    // the answer to a request of ours
)

// threadInfo is what LSDeleteThenInsertThread and LSUpdateOrInsertThread
// share.
type threadInfo interface {
	table.ThreadInfo
}

// applyTable applies a table of changes from Messenger, in the order
// mautrix-meta's connector does, and sends the events they make; m.mu is
// held. It sends nothing over the network itself (lookups of unknown people
// and threads go out in the background).
func (m *Messenger) applyTable(tbl *table.LSTable, src tableSource) {
	if tbl == nil {
		return
	}
	outer := m.dirty == nil
	if outer {
		m.dirty = map[int64]bool{}
		defer m.flushDirty()
	}

	for _, c := range tbl.LSDeleteThenInsertContact {
		m.updatePerson(c.Id, c.Name, firstNonEmpty(c.Username, c.SecondaryName), c.GetAvatarURL())
	}
	for _, c := range tbl.LSVerifyContactRowExists {
		m.updatePerson(c.ContactId, c.Name, c.SecondaryName, c.ProfilePictureUrl)
	}
	for _, p := range tbl.LSDeleteThenInsertContactPresence {
		if p.LastActiveTimestampMs > 0 {
			pp := m.person(p.ContactId)
			if at := p.LastActiveTimestampMs / 1000; at != pp.activeAt {
				pp.activeAt = at
				if pp.told {
					m.d.Events.User(m.userObject(pp))
				}
			}
		}
	}

	// Encrypted threads Messenger also lists under a Facebook thread key.
	for _, h := range tbl.LSUpdateThreadAuthorityAndMappingWithOTIDFromJID {
		m.mapHybrid(h.ThreadKey, h.ThreadJID, 0)
	}
	for _, h := range tbl.LSVerifyHybridThreadExists {
		c := m.mapHybrid(h.ThreadKey, h.ThreadJID, h.ThreadType)
		c.activity = max(c.activity, h.LastActivityTimestampMS)
		if h.LastReadWatermarkTimestampMS > c.readUpTo {
			c.readUpTo = h.LastReadWatermarkTimestampMS
			c.receipted = max(c.receipted, c.readUpTo)
			m.recount(c)
		}
		m.touch(c)
	}

	// Threads with fresh data in this same table weren't really deleted.
	active := map[int64]bool{}
	for _, t := range tbl.LSVerifyThreadExists {
		active[t.ThreadKey] = true
	}
	for _, t := range tbl.LSUpdateOrInsertThread {
		active[t.ThreadKey] = true
	}
	for _, t := range tbl.LSDeleteThenInsertThread {
		active[t.ThreadKey] = true
	}
	for _, r := range tbl.LSInsertNewMessageRange {
		active[r.ThreadKey] = true
	}
	gone := func(key int64) {
		if active[key] {
			return
		}
		if c := m.lookupChat(key); c != nil {
			m.forgetKept(c)
			m.removeChat(c)
		}
	}
	for _, d := range tbl.LSDeleteThread {
		gone(d.ThreadKey)
	}
	for _, d := range tbl.LSDeletePartialThread {
		gone(d.ThreadKey)
	}
	for _, d := range tbl.LSDeleteMessageRequest {
		gone(d.ThreadKey)
	}
	for _, r := range tbl.LSRemoveParticipantFromThread {
		if r.ParticipantId == m.self {
			gone(r.ThreadKey)
		}
	}

	for _, t := range tbl.LSDeleteThenInsertThread {
		c := m.thread(t)
		c.serverSaid, c.serverAt = int(t.UnreadMessageCount), t.LastActivityTimestampMs
		c.muteUntil = t.MuteExpireTimeMs
		m.recount(c)
	}
	for _, t := range tbl.LSUpdateOrInsertThread {
		c := m.thread(t)
		c.muteUntil = t.MuteExpireTimeMs
	}
	for _, t := range tbl.LSVerifyThreadExists {
		c := m.lookupChat(t.ThreadKey)
		if c == nil || !c.known {
			if t.FolderName == "spam" || t.ThreadType == table.FOLDER {
				continue
			}
			c = m.chatByKey(t.ThreadKey)
			m.setType(c, t.ThreadType)
			m.setFolder(c, t.FolderName)
			m.askThread(t.ThreadKey)
		}
	}
	for _, a := range tbl.LSAddParticipantIdToGroupThread {
		c := m.chatByKey(a.ThreadKey)
		if !slices.Contains(c.members, a.ContactId) {
			c.members = append(c.members, a.ContactId)
		}
		if a.Nickname != "" {
			c.nicknames[a.ContactId] = a.Nickname
		}
		if a.ContactId != m.self && a.ReadWatermarkTimestampMs > 0 {
			c.theirRead[a.ContactId] = max(c.theirRead[a.ContactId], a.ReadWatermarkTimestampMs)
		}
		m.touch(c)
	}
	for _, r := range tbl.LSRemoveParticipantFromThread {
		if r.ParticipantId == m.self {
			continue
		}
		if c := m.lookupChat(r.ThreadKey); c != nil {
			c.members = slices.DeleteFunc(c.members, func(id int64) bool { return id == r.ParticipantId })
			delete(c.theirRead, r.ParticipantId)
			m.touch(c)
		}
	}
	for _, u := range tbl.LSUpdateThreadMuteSetting {
		if c := m.lookupChat(u.ThreadKey); c != nil {
			c.muteUntil = u.MuteExpireTimeMS
			m.touch(c)
		}
	}
	for _, u := range tbl.LSMoveThreadToE2EECutoverFolder {
		if c := m.lookupChat(u.ThreadKey); c != nil {
			c.encrypted = true
			m.touch(c)
		}
	}
	for _, u := range tbl.LSMoveThreadToArchivedFolder {
		if c := m.lookupChat(u.ThreadKey); c != nil {
			c.archived = true
			m.touch(c)
		}
	}
	for _, u := range tbl.LSMoveThreadToInboxAndUpdateParent {
		if c := m.lookupChat(u.ThreadKey); c != nil {
			c.archived, c.request = false, false
			m.touch(c)
		}
	}
	for _, u := range tbl.LSDeleteThenInsertMessageRequest {
		if c := m.lookupChat(u.ThreadKey); c != nil {
			c.request = u.MessageRequestStatus == 1
			m.touch(c)
		}
	}
	for _, u := range tbl.LSSyncUpdateThreadName {
		if c := m.lookupChat(u.ThreadKey); c != nil && c.kind == proto.Group {
			c.name = u.ThreadName
			m.touch(c)
		}
	}
	for _, u := range tbl.LSSetThreadImageURL {
		if c := m.lookupChat(u.ThreadKey); c != nil && c.kind == proto.Group {
			c.picture = u.ImageURL
			m.touch(c)
		}
	}

	// Messages: pages of history (ranges) and new arrivals.
	upserts, inserts := tbl.WrapMessages()
	ranges := make([]*table.UpsertMessages, 0, len(upserts))
	for _, u := range upserts {
		ranges = append(ranges, u)
	}
	slices.SortFunc(ranges, func(a, b *table.UpsertMessages) int { return cmp.Compare(a.GetThreadKey(), b.GetThreadKey()) })
	for _, u := range ranges {
		m.applyHistory(u)
	}
	slices.SortStableFunc(inserts, func(a, b *table.WrappedMessage) int { return cmp.Compare(a.TimestampMs, b.TimestampMs) })
	for _, wm := range inserts {
		m.applyRow(func() { m.applyInsert(wm, src) })
	}
	for _, r := range tbl.LSUpdateExistingMessageRange {
		m.wake(r.ThreadKey)
	}

	for _, e := range tbl.LSEditMessage {
		m.applyEdit(e.MessageID, e.Text, e.EditCount)
	}
	for _, d := range tbl.LSDeleteMessage {
		if c := m.lookupChat(d.ThreadKey); c != nil {
			m.deleted(c, d.MessageId)
		}
	}
	for _, d := range tbl.LSDeleteThenInsertMessage {
		if d.IsUnsent {
			if c := m.lookupChat(d.ThreadKey); c != nil {
				m.deleted(c, d.MessageId)
			}
		}
	}
	for _, r := range tbl.LSUpsertReaction {
		m.applyReaction(r.ThreadKey, r.MessageId, r.ActorId, r.Reaction)
	}
	for _, r := range tbl.LSDeleteReaction {
		m.applyReaction(r.ThreadKey, r.MessageId, r.ActorId, "")
	}

	for _, r := range tbl.LSMarkThreadRead {
		m.readElsewhere(r.ThreadKey, r.LastReadWatermarkTimestampMs)
	}
	for _, r := range tbl.LSMarkThreadReadV2 {
		m.readElsewhere(r.ThreadKey, r.LastReadWatermarkTimestampMs)
	}
	for _, r := range tbl.LSUpdateReadReceipt {
		if r.ContactId == m.self {
			m.readElsewhere(r.ThreadKey, r.ReadWatermarkTimestampMs)
		} else {
			m.theyRead(r.ThreadKey, r.ContactId, r.ReadWatermarkTimestampMs)
		}
	}
	for _, t := range tbl.LSUpdateTypingIndicator {
		if c := m.lookupChat(t.ThreadKey); c != nil && t.SenderId != m.self && t.SenderId != 0 {
			p := m.person(t.SenderId)
			m.tellUser(p, false)
			m.d.Events.Typing(c.id, p.id, t.IsTyping)
		}
	}
}

// thread records a thread's details; m.mu is held.
func (m *Messenger) thread(t threadInfo) *chat {
	key := t.GetThreadKey()
	c := m.chatByKey(key)
	if c.key != key {
		c.fbKey = key
	}
	c.known = true
	m.setType(c, t.GetThreadType())
	m.setFolder(c, t.GetFolderName())
	if c.kind == proto.Group {
		c.name = t.GetThreadName()
		c.picture = t.GetThreadPictureUrl()
	}
	var activity, read int64
	var cantReply, receiptsOff bool
	switch tt := t.(type) {
	case *table.LSDeleteThenInsertThread:
		activity, read = tt.LastActivityTimestampMs, tt.LastReadWatermarkTimestampMs
		cantReply = tt.DisableComposerInput || (tt.CannotReplyReason != nil && tt.CannotReplyReason != int64(0) && tt.CannotReplyReason != float64(0))
		receiptsOff = flagOn(tt.ReadReceiptsDisabledV2)
	case *table.LSUpdateOrInsertThread:
		activity, read = tt.LastActivityTimestampMs, tt.LastReadWatermarkTimestampMs
		cantReply = tt.DisableComposerInput || tt.CannotReplyReason != 0
		receiptsOff = tt.IsReadReceiptsDisabled || tt.ReadReceiptsDisabledV2 != 0
	}
	c.cantReply = cantReply
	// Once Messenger says your read receipts are off in a chat they stay off
	// for the run: a later row that leaves the flag out can't be told from
	// one turning them back on, and a receipt sent can't be taken back. The
	// next start reads them afresh.
	c.receiptsOff = c.receiptsOff || receiptsOff
	c.activity = max(c.activity, activity)
	if read > c.readUpTo {
		c.readUpTo = read
		c.receipted = max(c.receipted, read)
	}
	m.recount(c)
	m.touch(c)
	return c
}

// flagOn reports whether a loosely typed table flag is set.
func flagOn(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != "" && x != "0" && x != "false"
	}
	return false
}

// setType records what kind of thread c is; m.mu is held.
func (m *Messenger) setType(c *chat, t table.ThreadType) {
	if t == table.UNKNOWN_THREAD_TYPE {
		return
	}
	if c.ttype.IsWhatsApp() && !t.IsWhatsApp() {
		// The Facebook side of an encrypted chat; it stays encrypted.
		return
	}
	c.ttype = t
	if t.IsOneToOne() {
		c.kind = proto.DM
		c.other = c.key
	} else {
		c.kind = proto.Group
	}
	if t.IsWhatsApp() {
		c.encrypted = true
		if t == table.ENCRYPTED_OVER_WA_GROUP {
			c.server = "g.us"
		}
	}
	c.refreshComplete()
}

// setFolder records which folder c is in: archived, a message request, an
// encrypted chat's cutover; m.mu is held.
func (m *Messenger) setFolder(c *chat, folder string) {
	if folder == "" {
		return
	}
	c.folder = folder
	c.archived = folder == "archived" || folder == "e2ee_cutover_archived"
	c.request = folder == "pending" || folder == "other" || folder == "e2ee_cutover_pending" || folder == "e2ee_cutover_other"
	if strings.HasPrefix(folder, "e2ee_cutover") {
		c.encrypted = true
		c.refreshComplete()
	}
}

// mapHybrid records that Facebook thread fbKey is encrypted chat waKey,
// folding a chat known under the Facebook key into the encrypted one;
// m.mu is held.
func (m *Messenger) mapHybrid(fbKey, waKey int64, t table.ThreadType) *chat {
	if fbKey == 0 || waKey == 0 {
		return m.chatByKey(max(fbKey, waKey))
	}
	if fbKey != waKey {
		if old := m.chats[fbKey]; old != nil {
			// Messages and details under the old key move over.
			nc := m.chatByKey(waKey)
			if nc.name == "" {
				nc.name = old.name
			}
			if nc.picture == "" {
				nc.picture = old.picture
			}
			if len(nc.members) == 0 {
				nc.members = old.members
			}
			nc.known = nc.known || old.known
			nc.activity = max(nc.activity, old.activity)
			nc.readUpTo = max(nc.readUpTo, old.readUpTo)
			nc.receipted = max(nc.receipted, old.receipted)
			m.removeChat(old)
		}
		m.hybrid[fbKey] = waKey
	}
	c := m.chatByKey(waKey)
	if c.key != fbKey {
		c.fbKey = fbKey
	}
	if t == table.UNKNOWN_THREAD_TYPE {
		t = table.ENCRYPTED_OVER_WA_ONE_TO_ONE
		if fbKey != waKey {
			t = table.ENCRYPTED_OVER_WA_GROUP
		}
	}
	m.setType(c, t)
	c.encrypted = true
	c.known = true
	return c
}

// refreshComplete records whether c's history is all there: a chat's
// Facebook history can be paged until Messenger says it ends; an encrypted
// chat's messages are only what arrived here, unless it still has Facebook
// history from before it was encrypted.
func (c *chat) refreshComplete() { c.log.SetComplete(!c.olderFB()) }

// olderFB reports whether c's history may go back further on Messenger.
func (c *chat) olderFB() bool {
	if !c.encrypted {
		return c.fbHistory
	}
	oldest, ok := c.log.Oldest()
	if !ok {
		return false
	}
	for _, msg := range c.msgs {
		if len(msg.ids) > 0 && msg.ids[0] == oldest.ID {
			return msg.wa == nil && c.fbHistory
		}
	}
	return false
}

// applyHistory adds a page of a thread's history; m.mu is held. Messages
// newer than any known (a gap filled after a reconnect) are reported as
// new; older ones only join the history.
func (m *Messenger) applyHistory(u *table.UpsertMessages) {
	key := u.GetThreadKey()
	if key == 0 {
		return
	}
	c := m.chatByKey(key)
	newest, hasNewest := c.log.Newest()
	slices.SortStableFunc(u.Messages, func(a, b *table.WrappedMessage) int { return cmp.Compare(a.TimestampMs, b.TimestampMs) })
	for _, wm := range u.Messages {
		m.applyRow(func() {
			if wm.IsUnsent {
				m.deleted(c, wm.MessageId)
				return
			}
			msg := m.convertFB(c, wm)
			if m.sent[c.id] && hasNewest && positionOf(msg.ms) > newest.ID && c.msgs[msg.netID] == nil {
				m.arrived(c, msg, wm.OfflineThreadingId)
				return
			}
			m.keep(c, msg)
			c.activity = max(c.activity, msg.ms)
		})
	}
	if u.Range != nil {
		c.fbHistory = u.Range.HasMoreBefore
	}
	c.refreshComplete()
	m.recount(c)
	m.touch(c)
	m.wake(key)
}

// applyRow applies one message row; m.mu is held. A row whose data trips a
// bug is logged (where, never what) and dropped, and the rest of the table
// still applies: one message mustn't cost a page of others, or keep the
// account from connecting when it's in the page's first data.
func (m *Messenger) applyRow(apply func()) {
	defer func() {
		if v := recover(); v != nil {
			hlog.Recovered("messenger message row", v)
		}
	}()
	apply()
}

// applyInsert adds a message that just arrived; m.mu is held.
func (m *Messenger) applyInsert(wm *table.WrappedMessage, src tableSource) {
	c := m.chatByKey(wm.ThreadKey)
	if wm.IsUnsent {
		m.deleted(c, wm.MessageId)
		return
	}
	if !c.known && c.kind == proto.DM && c.ttype == table.UNKNOWN_THREAD_TYPE {
		m.askThread(wm.ThreadKey)
	}
	m.arrived(c, m.convertFB(c, wm), wm.OfflineThreadingId)
}

// applyEdit replaces a message's text; m.mu is held.
func (m *Messenger) applyEdit(netID, text string, count int64) {
	if ch, ok := m.edits[netID]; ok {
		select {
		case ch <- text:
		default:
		}
	}
	key, ok := m.msgChat[netID]
	if !ok {
		return
	}
	c := m.chats[key]
	if c == nil {
		return
	}
	msg := c.msgs[netID]
	if msg == nil || (count != 0 && count < msg.editCount) {
		return
	}
	msg.text = text
	msg.mentions = nil
	msg.edited = true
	msg.editCount = max(msg.editCount, count)
	m.changed(c, msg)
}

// applyReaction sets actor's one reaction to a message; m.mu is held.
func (m *Messenger) applyReaction(threadKey int64, netID string, actor int64, emoji string) {
	c := m.lookupChat(threadKey)
	if c == nil {
		if key, ok := m.msgChat[netID]; ok {
			c = m.chats[key]
		}
	}
	if c == nil {
		return
	}
	msg := c.msgs[netID]
	if msg == nil || !msg.setReaction(actor, emoji) {
		return
	}
	m.changed(c, msg)
}

// readElsewhere moves how far you've read a chat (on another device, or
// here); m.mu is held.
func (m *Messenger) readElsewhere(threadKey, ms int64) {
	c := m.lookupChat(threadKey)
	if c == nil || ms <= c.readUpTo {
		return
	}
	c.readUpTo = ms
	c.receipted = max(c.receipted, ms)
	if c.encrypted && m.store != nil {
		// Messenger won't remember this for an encrypted chat: the next
		// start reads it back from the store.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := m.store.setRead(ctx, c.netID(), ms); err != nil {
			hlog.Error("messenger: can't keep how far a chat was read", hlog.Kind(err))
		}
		cancel()
	}
	m.startTimers(c)
	m.recount(c)
	unread := c.unread
	m.d.Events.Read(c.id, positionOf(ms), 0, &unread)
	m.touch(c)
}

// theyRead moves how far someone else has read the chat; m.mu is held.
func (m *Messenger) theyRead(threadKey, who, ms int64) {
	c := m.lookupChat(threadKey)
	if c == nil || ms <= c.theirRead[who] {
		return
	}
	before := c.theirReadUpTo()
	c.theirRead[who] = ms
	if after := c.theirReadUpTo(); after > before {
		m.d.Events.Read(c.id, 0, positionOf(after), nil)
	}
}

// wake lets history requests waiting on a thread look again; m.mu is held.
func (m *Messenger) wake(key int64) {
	if c := m.lookupChat(key); c != nil {
		key = c.key
	}
	for _, ch := range m.waiters[key] {
		close(ch)
	}
	delete(m.waiters, key)
}
