// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"

	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"

	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// avatarSize is what tuimeta is told of a profile picture's size: Instagram
// serves them at 150 px, and doesn't say.
const avatarSize = 150

// maxInboxPages bounds how many pages of the inbox one load_chats fetches.
const maxInboxPages = 12

// chat is one Instagram thread.
//
// Instagram knows a thread by three ids. The thread key (an fbid; in a dm,
// the other person's) is what tuimeta's chat id is kept under, as the
// connector keys its portals. The thread's IGID ("id", "thread_fbid" in
// socket updates) is what most requests name, and the long thread id
// ("thread_id") is what unsending, read receipts and typing updates name.
type chat struct {
	key    int64
	netID  string
	id     int64
	igid   string
	longID string

	group     bool
	title     string // a group's own title
	photoURL  string // a group's picture
	members   []int64
	muted     bool
	request   bool
	inputMode int
	left      bool

	order        int64 // last activity, ms
	readInbox    int64 // how far you've read, ms
	readOutbox   int64 // how far the others read, ms
	markedUnread bool

	msgs map[string]*netMsg
	last *netMsg

	// The history, under b.mu. histGen counts the times it was dropped
	// (after a reconnect), so a page fetched before doesn't land in the new
	// one. fetchMu keeps one history fetch at a time per chat; it's taken
	// before b.mu, never while holding it.
	log     *history.Log
	histGen int
	fetched bool   // the newest page has been fetched
	cursor  string // where older pages continue
	fetchMu sync.Mutex
}

// person is someone on Instagram, by their messaging id (fbid).
type person struct {
	fbid     int64
	id       int64
	igid     string
	username string
	name     string
	photoURL string
}

func isRequestFolder(folder string) bool { return folder == "PENDING" || folder == "SPAM" }

// pos is the protocol's position for a moment: after every message id of
// that millisecond. 0 stays 0 (nothing read).
func pos(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return ids.MessageID(ms, ids.MaxSlot)
}

// userID is the helper's id of the person with this fbid; 0 for none.
func (b *Instagram) userID(fbid int64) int64 {
	if fbid == 0 {
		return 0
	}
	return b.d.IDs.User(proto.Instagram, strconv.FormatInt(fbid, 10))
}

// person keeps what an Instagram user record says; b.mu is held.
func (b *Instagram) person(u *slidetypes.User) *person {
	if u == nil {
		return nil
	}
	fbid := u.InteropMessagingUserFBID
	if fbid == 0 && u.ID != "" {
		fbid = b.byIGUser[u.ID]
	}
	if fbid == 0 {
		return nil
	}
	p := b.people[fbid]
	if p == nil {
		p = &person{fbid: fbid, id: b.userID(fbid)}
		b.people[fbid] = p
	}
	if u.ID != "" {
		p.igid = u.ID
		b.byIGUser[u.ID] = fbid
	}
	if u.Username != "" {
		p.username = u.Username
	}
	if u.FullName != "" {
		p.name = u.FullName
	}
	if u.ProfilePicURL != "" {
		p.photoURL = u.ProfilePicURL
	}
	return p
}

// displayName is how a person is named: their name, else their username.
func (p *person) displayName() string {
	return cmp.Or(strings.TrimSpace(p.name), p.username, "Instagram user")
}

// userObject is the protocol's person; b.mu is held.
func (b *Instagram) userObject(p *person) proto.User {
	u := proto.User{
		ID: p.id, Network: proto.Instagram, Name: p.displayName(), Username: p.username,
		IsSelf: p.fbid == b.selfFBID,
	}
	if img := b.avatar(p.photoURL); img != nil {
		u.Photo = img
	}
	return u
}

// avatar registers a profile or group picture; nil without one.
func (b *Instagram) avatar(raw string) *proto.Image {
	if webURL(raw) == "" {
		return nil
	}
	id := b.register(fileKey("", raw, "avatar"), raw, mimeOf(raw, proto.Photo), nil, nil)
	return &proto.Image{FileID: id, Width: avatarSize, Height: avatarSize}
}

// tellUser sends p's user event if tuimeta hasn't seen it as it is now;
// b.mu is held.
func (b *Instagram) tellUser(p *person) {
	if p == nil {
		return
	}
	u := b.userObject(p)
	if old, ok := b.told[p.id]; ok && sameUser(old, u) {
		return
	}
	b.told[p.id] = u
	b.d.Events.User(u)
}

// sameUser compares two user objects by value (Photo is a pointer).
func sameUser(a, b proto.User) bool {
	pa, pb := a.Photo, b.Photo
	a.Photo, b.Photo = nil, nil
	if a != b || (pa == nil) != (pb == nil) {
		return false
	}
	return pa == nil || *pa == *pb
}

// upsertThread keeps what a thread record says; b.mu is held. Its messages
// count as the chat's own (unread or not); full says they're a whole page,
// fit for the history (the inbox only has stubs of the newest).
func (b *Instagram) upsertThread(t *slidetypes.ThreadInfo, full bool) *chat {
	if t == nil || t.ThreadKey == 0 {
		return nil
	}
	c := b.chats[t.ThreadKey]
	if c == nil {
		c = &chat{key: t.ThreadKey, netID: strconv.FormatInt(t.ThreadKey, 10), log: history.New(), msgs: map[string]*netMsg{}}
		c.id = b.d.IDs.Chat(proto.Instagram, c.netID)
		b.chats[c.key] = c
	}
	if t.ID != "" {
		c.igid = t.ID
		b.byIGID[t.ID] = c
	}
	if t.ThreadID != "" {
		c.longID = t.ThreadID
		b.byLongID[t.ThreadID] = c
	}
	if t.Viewer != nil {
		if p := b.person(t.Viewer); p != nil && b.selfFBID == 0 {
			b.selfFBID = p.fbid
		}
	}
	var members []int64
	for _, u := range t.Users {
		if p := b.person(u); p != nil && p.fbid != b.selfFBID {
			members = append(members, p.fbid)
		}
	}
	if len(t.Users) > 0 || members != nil {
		c.members = members
	}
	c.group = t.IsGroup || len(t.Users) > 1
	c.title = t.ThreadTitle
	if c.group {
		c.photoURL = t.ThreadImageURL
	}
	if t.IsMuted != nil {
		c.muted = *t.IsMuted
	}
	if t.SystemFolder != "" || t.Folder != "" {
		c.request = isRequestFolder(t.SystemFolder) || isRequestFolder(t.Folder)
	}
	c.inputMode = t.InputMode
	c.order = max(c.order, millis(t.LastActivityTimestampMS))
	c.markedUnread = t.MarkedAsUnread
	for _, rr := range t.SlideReadReceipts {
		b.applyReceipt(c, rr.ParticipantFBID, millis(rr.WatermarkTimestampMS))
	}
	if t.SlideMessages != nil {
		for _, edge := range t.SlideMessages.Edges {
			n := b.convert(c, edge.Node)
			if n == nil || n.skip {
				continue
			}
			n = b.keep(c, n)
			if full {
				c.log.Put(b.parts(c, n)...)
				n.inLog = true
			}
		}
	}
	return c
}

// keep stores a message of the chat proper (not a copy quoted in a reply),
// replacing an older copy and keeping what only that one knew; b.mu is held.
func (b *Instagram) keep(c *chat, n *netMsg) *netMsg {
	if old := c.msgs[n.netID]; old != nil {
		n.ids = old.ids
		n.inLog = old.inLog
		if n.replyNet != "" && n.replySnippet == "" && old.replyNet == n.replyNet {
			n.replySender, n.replySnippet = old.replySender, old.replySnippet
		}
	}
	n.counted = true
	c.msgs[n.netID] = n
	b.assign(c, n)
	if n.counted && (c.last == nil || n.ms > c.last.ms || (n.ms == c.last.ms && n.netID == c.last.netID)) {
		c.last = n
	}
	if n.mine {
		// Writing in a chat reads it, on Instagram as anywhere.
		c.readInbox = max(c.readInbox, n.ms)
	}
	return n
}

// applyReceipt moves a read position: yours (read here or elsewhere) or
// someone else's; b.mu is held. It says which moved.
func (b *Instagram) applyReceipt(c *chat, who, ms int64) (inbox, outbox bool) {
	if ms <= 0 {
		return false, false
	}
	if who == b.selfFBID {
		if ms > c.readInbox {
			c.readInbox = ms
			return true, false
		}
		return false, false
	}
	if ms > c.readOutbox {
		c.readOutbox = ms
		return false, true
	}
	return false, false
}

// unread counts others' messages past your read position; b.mu is held.
func (c *chat) unread(self int64) int {
	n := 0
	for _, m := range c.msgs {
		if m.counted && !m.skip && !m.mine && m.service == "" && m.ms > c.readInbox {
			n++
		}
	}
	if n == 0 && c.markedUnread {
		n = 1
	}
	return n
}

// title is the chat's name: a group's title (or its people), the other
// person in a dm; b.mu is held.
func (b *Instagram) title(c *chat) string {
	if c.group {
		if t := strings.TrimSpace(c.title); t != "" {
			return t
		}
		var names []string
		for _, fbid := range c.members {
			if p := b.people[fbid]; p != nil {
				names = append(names, p.displayName())
			}
			if len(names) == 3 {
				break
			}
		}
		if len(names) > 0 {
			return strings.Join(names, ", ")
		}
		return "Group chat"
	}
	if p := b.other(c); p != nil {
		return p.displayName()
	}
	if len(c.members) == 0 && b.selfFBID != 0 {
		if p := b.people[b.selfFBID]; p != nil {
			return p.displayName() // your own notes
		}
	}
	return "Instagram chat"
}

// other is the other person in a dm; b.mu is held.
func (b *Instagram) other(c *chat) *person {
	if c.group {
		return nil
	}
	if len(c.members) > 0 {
		return b.people[c.members[0]]
	}
	return b.people[c.key] // a dm's key is the other person's id
}

// chatObject is the protocol's chat; b.mu is held.
func (b *Instagram) chatObject(c *chat) proto.Chat {
	ch := proto.Chat{
		ID: c.id, Network: proto.Instagram, Kind: proto.DM, Title: b.title(c),
		Order: c.order, Unread: c.unread(b.selfFBID), Muted: c.muted, Request: c.request,
		CanSend:   c.inputMode == 0 && !c.left,
		ReadInbox: pos(c.readInbox), ReadOutbox: pos(c.readOutbox),
	}
	if c.group {
		ch.Kind = proto.Group
		ch.Photo = b.avatar(c.photoURL)
	} else if p := b.other(c); p != nil {
		ch.UserID = p.id
		ch.Photo = b.avatar(p.photoURL)
	}
	if c.last != nil {
		parts := b.parts(c, c.last)
		last := parts[len(parts)-1]
		ch.LastMessage = &last
		ch.Order = max(ch.Order, c.last.ms)
	}
	return ch
}

// tellChat sends a chat's event (with its people first, so tuimeta knows
// them), and counts it as sent; b.mu is held.
func (b *Instagram) tellChat(c *chat) {
	if p := b.people[b.selfFBID]; p != nil {
		b.tellUser(p)
	}
	for _, fbid := range c.members {
		b.tellUser(b.people[fbid])
	}
	b.sent[c.id] = true
	b.d.Events.Chat(b.chatObject(c))
}

// addInbox keeps a page of the inbox; b.mu is held.
func (b *Instagram) addInbox(threads slidetypes.Edged[slidetypes.Node[slidetypes.WrappedThreadInfo]]) {
	for _, edge := range threads.Edges {
		b.upsertThread(edge.Node.AsIGDirectThread, false)
	}
	b.inboxCursor = threads.PageInfo.EndCursor
	b.inboxMore = threads.PageInfo.HasNextPage && threads.PageInfo.EndCursor != ""
}

// LoadChats sends the next chats, newest activity first, fetching more of
// the inbox as needed.
func (b *Instagram) LoadChats(ctx context.Context, limit int) (bool, error) {
	conn, err := b.waitReady(ctx)
	if err != nil {
		return false, err
	}
	limit = max(limit, 1)
	// Each step that takes b.mu unlocks it deferred: they apply Instagram's
	// data and render chats' last messages, and a panic on something
	// unforeseen there mustn't leave b.mu held, and the backend stuck, for
	// the rest of the run.
	for page := 0; ; page++ {
		req, more, err := b.nextChats(conn, limit, page)
		if req == nil {
			return more, err
		}
		resp, err := conn.cli.PaginateMailbox(withQuietLog(ctx), req)
		if err != nil || resp == nil || resp.Mailbox == nil {
			hlog.Warn("instagram: inbox page failed", hlog.Kind(err))
			if more, sent := b.sendKnownChats(conn, limit); sent {
				return more, nil
			}
			if err == nil {
				err = errUnreachable
			}
			return false, requestError(err, "Couldn't load more Instagram chats; try again.")
		}
		b.inboxPage(conn, resp.Mailbox.ThreadsByFolder)
	}
}

// nextChats sends the next chats if enough are known, or no more can be
// fetched (answering whether more remain), else names the inbox page to
// fetch.
func (b *Instagram) nextChats(conn *connection, limit, page int) (*slidetypes.PaginateMailboxRequest, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != conn {
		return nil, false, errNotConnected
	}
	unsent := b.unsentChats()
	if len(unsent) >= limit || !b.inboxMore || page >= maxInboxPages {
		return nil, b.sendChats(unsent, limit), nil
	}
	return slidetypes.MakePaginateMailboxRequest(b.selfFBID, b.inboxCursor, "INBOX", nil), false, nil
}

// sendKnownChats sends what chats are known when no more of the inbox could
// be fetched, and says whether it had any to send.
func (b *Instagram) sendKnownChats(conn *connection, limit int) (more, sent bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	unsent := b.unsentChats()
	if len(unsent) == 0 || b.conn != conn {
		return false, false
	}
	return b.sendChats(unsent, limit), true
}

// inboxPage keeps a page of the inbox, if conn is still the connection.
func (b *Instagram) inboxPage(conn *connection, threads slidetypes.Edged[slidetypes.Node[slidetypes.WrappedThreadInfo]]) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == conn {
		b.addInbox(threads)
	}
}

// unsentChats are the chats load_chats hasn't sent, newest first; b.mu is
// held.
func (b *Instagram) unsentChats() []*chat {
	var out []*chat
	for _, c := range b.chats {
		if !b.sent[c.id] {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(x, y *chat) int {
		if c := cmp.Compare(b.activity(y), b.activity(x)); c != 0 {
			return c
		}
		return cmp.Compare(x.id, y.id)
	})
	return out
}

func (b *Instagram) activity(c *chat) int64 {
	if c.last != nil {
		return max(c.order, c.last.ms)
	}
	return c.order
}

// sendChats sends up to limit of unsent and says whether more remain; b.mu
// is held.
func (b *Instagram) sendChats(unsent []*chat, limit int) bool {
	for _, c := range unsent[:min(limit, len(unsent))] {
		b.tellChat(c)
	}
	return len(unsent) > limit || b.inboxMore
}
