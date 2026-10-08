// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// MaxThreadPages bounds how many pages of threads one load_chats asks
// Messenger for before answering with what it has.
const MaxThreadPages = 3

// person is the person with Facebook id fbid, made if new; m.mu is held.
func (m *Messenger) person(fbid int64) *person {
	p := m.people[fbid]
	if p == nil {
		p = &person{fbid: fbid}
		p.id = m.d.IDs.User(net, p.netID())
		m.people[fbid] = p
	}
	return p
}

// userObject is p as tuimeta sees it; m.mu is held.
func (m *Messenger) userObject(p *person) proto.User {
	u := proto.User{ID: p.id, Network: net, Name: p.name, Username: p.username, IsSelf: p.fbid == m.self, ActiveAt: p.activeAt}
	if u.Name == "" {
		u.Name = "Messenger user"
	}
	if p.avatar != "" {
		u.Photo = m.picture(p.avatar)
	}
	return u
}

// tellUser sends p's user event if it hasn't gone out yet (or again, with
// again); m.mu is held. Someone tuimeta is about to see whose name isn't
// known is asked for.
func (m *Messenger) tellUser(p *person, again bool) {
	if p == nil || p.fbid == 0 {
		return
	}
	if !p.known && !p.asked && p.fbid != m.self {
		p.asked = true
		m.askContact(p.fbid)
	}
	if p.told && !again {
		return
	}
	p.told = true
	m.d.Events.User(m.userObject(p))
}

// updatePerson records what Messenger says about someone; m.mu is held.
func (m *Messenger) updatePerson(fbid int64, name, username, avatar string) {
	if fbid == 0 {
		return
	}
	p := m.person(fbid)
	changed := !p.known || (name != "" && p.name != name) || (username != "" && p.username != username) || (avatar != "" && avatarKey(avatar) != avatarKey(p.avatar))
	p.known = true
	if name != "" {
		p.name = name
	}
	if username != "" {
		p.username = username
	}
	if avatar != "" {
		p.avatar = avatar
	}
	if !changed {
		return
	}
	if p.told {
		m.d.Events.User(m.userObject(p))
	}
	// One-to-one chats are named after the person.
	if c := m.chats[fbid]; c != nil && c.kind == proto.DM {
		m.touch(c)
	}
	for _, c := range m.chats {
		if c.kind == proto.Group && c.name == "" && slices.Contains(c.members, fbid) {
			m.touch(c)
		}
	}
}

// askContact asks Messenger who fbid is, in the background.
func (m *Messenger) askContact(fbid int64) {
	meta := m.meta
	if meta == nil {
		return
	}
	life, gen := m.life, m.gen
	hlog.Go("messenger contact", func() {
		tbl, err := meta.ExecuteTasks(life, &socket.GetContactsFullTask{ContactID: fbid})
		if err != nil {
			hlog.Info("messenger: contact lookup failed", hlog.Kind(err))
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.gen == gen {
			m.applyTable(tbl, fromResponse)
		}
	})
}

// askThread asks Messenger for a thread's details once, in the background
// (a thread seen only by its key: the connector's verify-thread-exists).
func (m *Messenger) askThread(key int64) {
	if m.asked[key] || m.meta == nil {
		return
	}
	m.asked[key] = true
	meta, life, gen := m.meta, m.life, m.gen
	hlog.Go("messenger thread", func() {
		tbl, err := meta.ExecuteTasks(life, &socket.CreateThreadTask{ThreadFBID: key, SyncGroup: 1})
		if err != nil {
			hlog.Info("messenger: thread lookup failed", hlog.Kind(err))
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.gen == gen {
			m.applyTable(tbl, fromResponse)
		}
	})
}

// avatarKey names a picture by its file, not its signed URL.
func avatarKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return raw
	}
	return path.Base(u.Path)
}

// picture registers a profile or chat picture as a file; m.mu is held.
func (m *Messenger) picture(raw string) *proto.Image {
	if raw == "" || !allowedMediaURL(raw) {
		return nil
	}
	id := m.d.Files.Register(ids.FileRef{
		Network: net, Key: "picture:" + avatarKey(raw), Mime: "image/jpeg", Name: "picture.jpg",
		Source: &fbSource{URL: raw, Mime: "image/jpeg"},
	})
	if id == 0 {
		return nil
	}
	return &proto.Image{FileID: id, Width: 160, Height: 160}
}

// chatByKey is the chat with key, made if new; m.mu is held. A Facebook
// thread key that maps to an encrypted chat gives that chat.
func (m *Messenger) chatByKey(key int64) *chat {
	if wa, ok := m.hybrid[key]; ok {
		key = wa
	}
	c := m.chats[key]
	if c == nil {
		c = newChat(key)
		c.id = m.d.IDs.Chat(net, c.netID())
		m.chats[key] = c
		m.byID[c.id] = c
	}
	return c
}

// lookupChat is the known chat with key, or nil; m.mu is held.
func (m *Messenger) lookupChat(key int64) *chat {
	if wa, ok := m.hybrid[key]; ok {
		key = wa
	}
	return m.chats[key]
}

// touch marks c changed, for a chat event once the current batch of changes
// is applied (or at once outside one); m.mu is held.
func (m *Messenger) touch(c *chat) {
	if m.dirty != nil {
		m.dirty[c.key] = true
		return
	}
	m.chatChanged(c)
}

// chatChanged sends c's chat event if tuimeta has it, or if it's new
// activity tuimeta should see now; m.mu is held.
func (m *Messenger) chatChanged(c *chat) {
	if !m.visible(c) {
		return
	}
	if !m.sent[c.id] {
		if !m.listed && !m.newerThanSent(c) {
			return // a later load_chats sends it
		}
		m.sent[c.id] = true
	}
	m.sendChat(c)
}

// newerThanSent reports whether c's activity is at least the oldest chat's
// load_chats sent; m.mu is held.
func (m *Messenger) newerThanSent(c *chat) bool {
	if len(m.sent) == 0 {
		return false
	}
	for id := range m.sent {
		if o := m.byID[id]; o != nil && o.activity <= c.activity {
			return true
		}
	}
	return false
}

// visible reports whether c belongs in the chat list; m.mu is held.
func (m *Messenger) visible(c *chat) bool {
	if c.folder == "spam" || c.folder == "hidden" {
		return false
	}
	switch c.ttype {
	case table.FOLDER, table.MONTAGE, table.COMMUNITY_FOLDER, table.COMMUNITY_CHANNEL_CATEGORY:
		return false
	}
	return c.known || len(c.msgs) > 0
}

// sendChat sends c's chat event, after the people in it; m.mu is held.
func (m *Messenger) sendChat(c *chat) {
	m.tellUser(m.person(m.self), false)
	if c.kind == proto.DM {
		m.tellUser(m.person(c.other), false)
	}
	for _, fbid := range c.members {
		m.tellUser(m.person(fbid), false)
	}
	m.d.Events.Chat(m.chatObject(c))
}

// flushDirty sends the chat events of the chats a batch changed; m.mu is held.
func (m *Messenger) flushDirty() {
	dirty := m.dirty
	m.dirty = nil
	keys := make([]int64, 0, len(dirty))
	for k := range dirty {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if c := m.chats[k]; c != nil {
			m.chatChanged(c)
		}
	}
}

// chatObject is c as tuimeta sees it; m.mu is held.
func (m *Messenger) chatObject(c *chat) proto.Chat {
	out := proto.Chat{
		ID: c.id, Network: net, Kind: c.kind, Title: m.title(c),
		Order: c.activity, Unread: c.unread, Muted: m.muted(c), Archived: c.archived,
		Encrypted: c.encrypted, Request: c.request, CanSend: !c.cantReply,
	}
	if c.readUpTo > 0 {
		out.ReadInbox = positionOf(c.readUpTo)
	}
	if theirs := c.theirReadUpTo(); theirs > 0 {
		out.ReadOutbox = positionOf(theirs)
	}
	if c.kind == proto.DM {
		p := m.person(c.other)
		out.UserID = p.id
		if p.avatar != "" {
			out.Photo = m.picture(p.avatar)
		}
	} else if c.picture != "" {
		out.Photo = m.picture(c.picture)
	}
	if last, ok := c.log.Newest(); ok {
		out.LastMessage = &last
		out.Order = max(out.Order, ids.Millis(last.ID))
	}
	return out
}

// theirReadUpTo is how far the other side has read your messages: in a
// group, the furthest anyone has.
func (c *chat) theirReadUpTo() int64 {
	var most int64
	for _, ms := range c.theirRead {
		most = max(most, ms)
	}
	return most
}

// muted reports whether c is muted now; m.mu is held.
func (m *Messenger) muted(c *chat) bool {
	return c.muteUntil < 0 || c.muteUntil > m.now().UnixMilli()
}

// title is c's name: a group's own, else its members' first few names, or
// the other person's name in a one-to-one chat; m.mu is held.
func (m *Messenger) title(c *chat) string {
	if c.kind == proto.DM {
		if c.other == m.self {
			return "Notes to self"
		}
		if p := m.people[c.other]; p != nil && p.name != "" {
			return p.name
		}
		return "Messenger user"
	}
	if c.name != "" {
		return c.name
	}
	var names []string
	others := 0
	for _, fbid := range c.members {
		if fbid == m.self {
			continue
		}
		others++
		if len(names) == 3 {
			continue
		}
		if n := m.firstName(fbid); n != "" {
			names = append(names, n)
		}
	}
	switch {
	case len(names) == 0:
		return "Group chat"
	case others > len(names):
		return strings.Join(names, ", ") + " and " + strconv.Itoa(others-len(names)) + " more"
	}
	return strings.Join(names, ", ")
}

func (m *Messenger) firstName(fbid int64) string {
	p := m.people[fbid]
	if p == nil {
		return ""
	}
	first, _, _ := strings.Cut(p.name, " ")
	return first
}

// recount works out c's unread messages: others' messages after readUpTo,
// an album counted once. When the loaded messages don't reach back to
// readUpTo, the server's last count stands in for those it covered, plus
// what came after it; m.mu is held.
func (m *Messenger) recount(c *chat) {
	if c.activity > 0 && c.activity <= c.readUpTo {
		c.unread = 0
		return
	}
	all := c.log.All()
	count := func(after int64) int {
		n := 0
		var lastAlbum int64
		for _, msg := range all {
			if msg.Outgoing || msg.Service != "" || ids.Millis(msg.ID) <= after {
				continue
			}
			if msg.Album != 0 {
				if msg.Album == lastAlbum {
					continue
				}
				lastAlbum = msg.Album
			}
			n++
		}
		return n
	}
	n := count(c.readUpTo)
	covered := c.log.Complete() || (len(all) > 0 && ids.Millis(all[0].ID) <= c.readUpTo)
	if !covered && c.serverSaid >= 0 && c.serverAt >= c.readUpTo {
		n = max(n, c.serverSaid+count(c.serverAt))
	}
	c.unread = n
}

// LoadChats sends the next chats, newest activity first, asking Messenger
// for older threads when the ones it has run out.
func (m *Messenger) LoadChats(ctx context.Context, limit int) (bool, error) {
	limit = max(limit, 1)
	for page := 0; ; page++ {
		m.mu.Lock()
		if !m.ready && len(m.chats) == 0 {
			m.mu.Unlock()
			return false, errNotConnected
		}
		batch, rest := m.unsent(limit)
		if len(batch) == limit || !m.more || page >= MaxThreadPages || m.meta == nil {
			for _, c := range batch {
				m.sent[c.id] = true
				m.sendChat(c)
			}
			more := rest > 0 || (m.more && m.meta != nil)
			m.listed = !more
			m.mu.Unlock()
			return more, nil
		}
		meta, gen := m.meta, m.gen
		m.mu.Unlock()

		keys, tbl, err := meta.FetchMoreThreads(ctx, 1)
		if err != nil {
			hlog.Info("messenger: fetching threads failed", hlog.Kind(err))
			return false, requestError(err)
		}
		m.mu.Lock()
		if m.gen == gen {
			if tbl != nil {
				m.applyTable(tbl, fromResponse)
			}
			switch {
			case tbl == nil || keys == nil || !keys.HasMoreBefore:
				m.more = false
			case keys.MinThreadKey == m.minKey:
				m.more = false // paging stopped moving
			default:
				m.minKey = keys.MinThreadKey
			}
		}
		m.mu.Unlock()
	}
}

// unsent is up to limit chats load_chats hasn't sent, newest first, and how
// many more remain; m.mu is held.
func (m *Messenger) unsent(limit int) ([]*chat, int) {
	var all []*chat
	for _, c := range m.chats {
		if !m.sent[c.id] && m.visible(c) {
			all = append(all, c)
		}
	}
	slices.SortFunc(all, byOrder)
	if len(all) <= limit {
		return all, 0
	}
	return all[:limit], len(all) - limit
}

// removeChat drops a chat that left the list; m.mu is held.
func (m *Messenger) removeChat(c *chat) {
	delete(m.chats, c.key)
	delete(m.byID, c.id)
	for fb, wa := range m.hybrid {
		if wa == c.key {
			delete(m.hybrid, fb)
		}
	}
	if m.dirty != nil {
		delete(m.dirty, c.key)
	}
	if m.sent[c.id] {
		delete(m.sent, c.id)
		m.d.Events.ChatRemoved(c.id)
	}
}

// chatOf is the chat a request names; m.mu is held.
func (m *Messenger) chatOf(id int64) (*chat, error) {
	c := m.byID[id]
	if c == nil {
		return nil, proto.ErrNoChat
	}
	return c, nil
}
