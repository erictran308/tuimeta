// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	waTypes "go.mau.fi/whatsmeow/types"

	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// dbWait bounds one look at the store while w.mu is held.
const dbWait = 10 * time.Second

func dbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbWait)
}

// canon is a person's key: their WhatsApp id (…@lid) once it's known, else
// their number's JID, without a device; w.mu is held.
func (w *WhatsApp) canon(j waTypes.JID) string {
	j = j.ToNonAD()
	if j.Server != waTypes.DefaultUserServer {
		return j.String()
	}
	pn := j.String()
	if key, ok := w.alias[pn]; ok {
		return key
	}
	if lid := w.lidFor(j); !lid.IsEmpty() {
		key := lid.ToNonAD().String()
		w.learn(pn, key)
		return key
	}
	return pn
}

// lidFor is the WhatsApp id of the person with number JID pn, if whatsmeow
// knows it (it keeps the pairs in the store, so they last across runs);
// w.mu is held.
func (w *WhatsApp) lidFor(pn waTypes.JID) waTypes.JID {
	ctx, cancel := dbCtx()
	defer cancel()
	if w.cli != nil {
		if lid, err := w.cli.LIDForPN(ctx, pn); err == nil && !lid.IsEmpty() {
			return lid
		}
	}
	if w.st != nil {
		if lid, err := w.st.container.LIDMap.GetLIDForPN(ctx, pn); err == nil {
			return lid
		}
	}
	return waTypes.EmptyJID
}

// pair learns that a and b are one person when one is a number's JID and
// the other a WhatsApp id; w.mu is held.
func (w *WhatsApp) pair(a, b waTypes.JID) {
	if a.IsEmpty() || b.IsEmpty() {
		return
	}
	a, b = a.ToNonAD(), b.ToNonAD()
	switch {
	case a.Server == waTypes.DefaultUserServer && b.Server == waTypes.HiddenUserServer:
		w.learn(a.String(), b.String())
	case b.Server == waTypes.DefaultUserServer && a.Server == waTypes.HiddenUserServer:
		w.learn(b.String(), a.String())
	}
}

// learn records that the person with number JID pn has WhatsApp id lid. A
// person or chat known so far by the number is known by the id from now on,
// keeping the ids tuimeta has for them; w.mu is held.
func (w *WhatsApp) learn(pn, lid string) {
	if pn == lid || w.alias[pn] == lid {
		return
	}
	w.alias[pn] = lid
	w.numbers[lid] = pn
	if w.st != nil {
		// Kept with whatsmeow's own pairs, so the next run knows it too.
		pj, _ := waTypes.ParseJID(pn)
		lj, _ := waTypes.ParseJID(lid)
		if pj.Server == waTypes.DefaultUserServer && lj.Server == waTypes.HiddenUserServer {
			ctx, cancel := dbCtx()
			if err := w.st.container.LIDMap.PutLIDMapping(ctx, lj, pj); err != nil {
				hlog.Warn("whatsapp: can't keep an id pair", hlog.Kind(err))
			}
			cancel()
		}
	}
	if w.selves[pn] {
		w.selves[lid] = true
		if w.self == pn {
			w.self = lid
		}
	}
	if p := w.people[pn]; p != nil {
		if q := w.people[lid]; q == nil {
			if w.d.IDs.RenameUser(net, pn, lid) {
				delete(w.people, pn)
				p.key = lid
				w.people[lid] = p
			}
		}
	}
	if q := w.people[lid]; q != nil {
		if q.phone == "" {
			q.phone = numberOf(pn)
		}
		// The address book knows people by number: the name may be
		// known now.
		if w.refreshName(q) && q.told {
			w.tellUser(q, true)
		}
	}
	if c := w.chats[pn]; c != nil {
		if other := w.chats[lid]; other != nil {
			w.merge(c, other)
		} else if w.d.IDs.RenameChat(net, pn, lid) {
			delete(w.chats, pn)
			c.key = lid
			c.Other = lid
			w.chats[lid] = c
			w.moveKept(pn, lid)
			w.saveChat(c)
		}
	}
	// Messages and member lists that name the number keep it: resolve
	// reads it as the WhatsApp id wherever people are compared.
}

// moveKept moves what's kept of chat from to chat to; w.mu is held.
func (w *WhatsApp) moveKept(from, to string) {
	if w.st == nil {
		return
	}
	ctx, cancel := dbCtx()
	defer cancel()
	if err := w.st.renameChat(ctx, from, to); err != nil {
		hlog.Error("whatsapp: can't move a chat to its WhatsApp id", hlog.Kind(err))
	}
}

// merge folds the chat by number from into the chat by WhatsApp id to, when
// both were made before WhatsApp said they're one person: to keeps its id,
// gets from's messages and the later activity, and from leaves the list;
// w.mu is held.
func (w *WhatsApp) merge(from, to *chat) {
	delete(w.chats, from.key)
	delete(w.byID, from.id)
	w.moveKept(from.key, to.key)
	to.Activity = max(to.Activity, from.Activity)
	to.Unread += from.Unread
	to.ReadUpTo = max(to.ReadUpTo, from.ReadUpTo)
	// Read back from the store, where both chats' messages now are.
	to.loaded, to.last = false, nil
	to.msgs, to.log = map[string]*message{}, history.New()
	w.ensureLoaded(to)
	w.saveChat(to)
	if w.sent[from.id] {
		delete(w.sent, from.id)
		w.d.Events.ChatRemoved(from.id)
	}
	w.touch(to)
}

// resolve is key, or the WhatsApp id it's known to stand for (a number's
// JID the store pairs with one counts, so keys kept by an earlier run
// still find their person); w.mu is held. It only looks: chats and people
// are moved to a WhatsApp id (learn) only where events come in, never in
// the middle of showing or keeping something.
func (w *WhatsApp) resolve(key string) string {
	if k, ok := w.alias[key]; ok {
		return k
	}
	if j, err := waTypes.ParseJID(key); err == nil && j.Server == waTypes.DefaultUserServer {
		if lid := w.lidFor(j); !lid.IsEmpty() {
			return lid.ToNonAD().String()
		}
	}
	return key
}

// keyOf is the key of the person with jid, looked up without learning
// anything (see resolve); w.mu is held.
func (w *WhatsApp) keyOf(j waTypes.JID) string { return w.resolve(j.ToNonAD().String()) }

// personJID reports whether j can be a person: a phone number's JID or a
// WhatsApp id. Anything else a sender names (a group, a channel, the status
// broadcast, a bot) is never made a person, so it can never become a chat
// you send to.
func personJID(j waTypes.JID) bool {
	return j.User != "" && (j.Server == waTypes.DefaultUserServer || j.Server == waTypes.HiddenUserServer)
}

// sendable reports whether a chat's JID is one tuimeta sends to: a
// person's, or a group's.
func sendable(j waTypes.JID) bool {
	return personJID(j) || (j.Server == waTypes.GroupServer && j.User != "")
}

// same reports whether two keys are one person; w.mu is held.
func (w *WhatsApp) same(a, b string) bool { return w.resolve(a) == w.resolve(b) }

// numberOf is the phone number in a number's JID, or "".
func numberOf(key string) string {
	user, server, ok := strings.Cut(key, "@")
	if !ok || server != waTypes.DefaultUserServer {
		return ""
	}
	return user
}

// phoneLabel is a number's JID written as a number ("+15550100100"), or a
// stand-in for someone known only by their WhatsApp id.
func phoneLabel(key string) string {
	if n := numberOf(key); n != "" {
		return "+" + n
	}
	return "WhatsApp user"
}

// isSelf reports whether key is you; w.mu is held.
func (w *WhatsApp) isSelf(key string) bool {
	return key != "" && (w.selves[key] || w.selves[w.resolve(key)])
}

// person is the person with key, made if new; w.mu is held.
func (w *WhatsApp) person(key string) *person {
	key = w.resolve(key)
	p := w.people[key]
	if p == nil {
		p = &person{key: key, id: w.d.IDs.User(net, key), phone: numberOf(key)}
		w.people[key] = p
		w.refreshName(p)
	}
	return p
}

// refreshName works out what p is called: the name in your phone's address
// book, else the name they gave WhatsApp, else their business's, else their
// number; w.mu is held.
func (w *WhatsApp) refreshName(p *person) bool {
	old := p.name
	p.name = ""
	if w.isSelf(p.key) {
		p.name = w.selfLabel()
		return p.name != old
	}
	if w.cli != nil {
		jids := []waTypes.JID{}
		if j, err := waTypes.ParseJID(p.key); err == nil {
			jids = append(jids, j)
			if j.Server == waTypes.HiddenUserServer {
				pn, _ := waTypes.ParseJID(w.numbers[p.key])
				if pn.IsEmpty() {
					ctx, cancel := dbCtx()
					pn, _ = w.cli.PNForLID(ctx, j)
					cancel()
				}
				if !pn.IsEmpty() {
					jids = append(jids, pn)
					p.phone = pn.User
				}
			}
		}
		var found waTypes.ContactInfo
		for _, j := range jids {
			ctx, cancel := dbCtx()
			info, err := w.cli.Contact(ctx, j)
			cancel()
			if err != nil || !info.Found {
				continue
			}
			found.FullName = firstNonEmpty(found.FullName, info.FullName)
			found.FirstName = firstNonEmpty(found.FirstName, info.FirstName)
			found.PushName = firstNonEmpty(found.PushName, info.PushName)
			found.BusinessName = firstNonEmpty(found.BusinessName, info.BusinessName)
		}
		// The name in your address book is yours; the one a person gave
		// WhatsApp is theirs, and could be anyone's ("Mum", a contact's
		// name, "You"), so it's shown as WhatsApp shows it: after a "~".
		if own := oneLine(firstNonEmpty(found.FullName, found.FirstName)); own != "" {
			p.name, p.contact = own, true
		} else if theirs := oneLine(firstNonEmpty(found.PushName, found.BusinessName)); theirs != "" {
			p.name, p.contact = "~"+theirs, false
		}
	}
	if p.name == "" && p.phone != "" {
		p.name = "+" + p.phone
	}
	if p.name == "" {
		p.name = "WhatsApp user"
	}
	p.name = cut(p.name, 300)
	return p.name != old
}

// cut keeps at most n runes of s.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// userObject is p as tuimeta sees it; w.mu is held.
func (w *WhatsApp) userObject(p *person) proto.User {
	u := proto.User{ID: p.id, Network: net, Name: p.name, IsSelf: w.isSelf(p.key)}
	u.Photo = w.avatar(p.key, p.pictureID)
	return u
}

// tellUser sends p's user event the first time it's needed, or again when
// what's known changed; w.mu is held.
func (w *WhatsApp) tellUser(p *person, again bool) {
	if p.told && !again {
		return
	}
	p.told = true
	w.d.Events.User(w.userObject(p))
}

// avatar is the picture of the person or group with key, downloaded only
// when tuimeta asks for it. pictureID names the picture, when an event said
// which, so a changed one is fetched again.
func (w *WhatsApp) avatar(key, pictureID string) *proto.Image {
	id := w.d.Files.Register(ids.FileRef{
		Network: net, Key: "wa:avatar:" + key + ":" + pictureID, Mime: "image/jpeg", Name: "avatar.jpg",
		Source: avatarSource{jid: key},
	})
	if id == 0 {
		return nil
	}
	return &proto.Image{FileID: id, Width: 96, Height: 96}
}

// chatFor is the chat with key, made if new; w.mu is held.
func (w *WhatsApp) chatFor(key string, group bool) *chat {
	key = w.resolve(key)
	c := w.chats[key]
	if c == nil {
		kind := proto.DM
		if group {
			kind = proto.Group
		}
		c = newChat(key, kind)
		if !group {
			c.Other = key
		}
		c.id = w.d.IDs.Chat(net, key)
		c.loaded = true // nothing kept yet
		w.chats[key] = c
		w.byID[c.id] = c
	}
	return c
}

// loadKept reads the kept chats back, with each one's newest message for
// the list; their other messages are read when the chat is opened. w.mu is
// held.
func (w *WhatsApp) loadKept(ctx context.Context) {
	rows, err := w.st.chats(ctx)
	if err != nil {
		hlog.Error("whatsapp: can't read kept chats", hlog.Kind(err))
		return
	}
	newest, err := w.st.newest(ctx)
	if err != nil {
		hlog.Error("whatsapp: can't read kept messages", hlog.Kind(err))
	}
	for key, row := range rows {
		c := newChat(key, row.Kind)
		c.chatRow = row
		c.kind = row.Kind
		if c.kind != proto.Group {
			c.kind = proto.DM
		}
		c.id = w.d.IDs.Chat(net, key)
		c.known = true
		c.last = newest[key]
		w.chats[key] = c
		w.byID[c.id] = c
	}
	hlog.Info("whatsapp: chats read back", hlog.Int("count", int64(len(rows))))
}

// saveChat keeps c's state; w.mu is held.
func (w *WhatsApp) saveChat(c *chat) {
	if w.st == nil {
		return
	}
	ctx, cancel := dbCtx()
	defer cancel()
	if err := w.st.putChat(ctx, c.key, c.row()); err != nil {
		w.storeFailed = true
		hlog.Error("whatsapp: can't keep a chat", hlog.Kind(err))
	}
}

// ensureLoaded reads c's kept messages in, once; w.mu is held.
func (w *WhatsApp) ensureLoaded(c *chat) {
	if c.loaded {
		return
	}
	c.loaded = true
	c.last = nil
	if w.st == nil {
		return
	}
	ctx, cancel := dbCtx()
	msgs, err := w.st.messages(ctx, c.key)
	cancel()
	if err != nil {
		hlog.Error("whatsapp: can't read a chat's messages", hlog.Kind(err))
		return
	}
	now := w.now().UnixMilli()
	for _, m := range msgs {
		if m.Expires > 0 && m.Expires <= now {
			continue // the sweep deletes it
		}
		w.keep(c, m)
	}
	c.log.SetComplete(c.Complete && len(msgs) < MaxStoredPerChat)
}

// touch sends c's chat event if tuimeta has the chat, or if it's new
// activity tuimeta should see now; w.mu is held.
func (w *WhatsApp) touch(c *chat) {
	if !w.visible(c) {
		return
	}
	if !w.sent[c.id] {
		if !w.listed && !w.newerThanSent(c) {
			return // a later load_chats sends it
		}
		w.sent[c.id] = true
	}
	w.sendChat(c)
}

// newerThanSent reports whether c's activity is at least the oldest chat's
// load_chats sent; w.mu is held.
func (w *WhatsApp) newerThanSent(c *chat) bool {
	for id := range w.sent {
		if o := w.byID[id]; o != nil && o.Activity <= c.Activity {
			return true
		}
	}
	return false
}

// visible reports whether c belongs in the chat list; w.mu is held.
func (w *WhatsApp) visible(c *chat) bool {
	return c.known || len(c.msgs) > 0 || c.last != nil
}

// sendChat sends c's chat event, after the people in it; w.mu is held.
func (w *WhatsApp) sendChat(c *chat) {
	if c.kind == proto.DM {
		w.tellUser(w.person(c.Other), false)
	}
	for _, key := range c.Members {
		w.tellUser(w.person(key), false)
	}
	w.d.Events.Chat(w.chatObject(c))
}

// chatObject is c as tuimeta sees it; w.mu is held.
func (w *WhatsApp) chatObject(c *chat) proto.Chat {
	out := proto.Chat{
		ID: c.id, Network: net, Kind: c.kind, Title: w.title(c),
		Order: c.Activity, Unread: c.Unread, Muted: w.muted(c), Archived: c.Archived,
		// Every WhatsApp chat is end-to-end encrypted.
		Encrypted: true, CanSend: !c.ReadOnly,
	}
	if c.MarkedUnread && out.Unread == 0 {
		out.Unread = 1
	}
	if c.ReadUpTo > 0 {
		out.ReadInbox = ids.MessageID(c.ReadUpTo, ids.MaxSlot)
	}
	if c.TheirRead > 0 {
		out.ReadOutbox = ids.MessageID(c.TheirRead, ids.MaxSlot)
	}
	if c.kind == proto.DM {
		p := w.person(c.Other)
		out.UserID = p.id
		out.Photo = w.avatar(p.key, p.pictureID)
	} else {
		out.Photo = w.avatar(c.key, c.PictureID)
	}
	var last *proto.Message
	if c.loaded {
		if m, ok := c.log.Newest(); ok {
			last = &m
		}
	} else if c.last != nil {
		m := w.render(c, c.last)
		last = &m
	}
	if last != nil {
		out.LastMessage = last
		out.Order = max(out.Order, ids.Millis(last.ID))
	}
	return out
}

// muted reports whether c is muted now; w.mu is held.
func (w *WhatsApp) muted(c *chat) bool {
	return c.MuteUntil < 0 || c.MuteUntil > w.now().UnixMilli()
}

// title is c's name: a group's subject, else its members' first names, or
// the other person's name; w.mu is held.
func (w *WhatsApp) title(c *chat) string {
	if c.kind == proto.DM {
		if w.isSelf(c.Other) {
			return "Notes to self"
		}
		// Someone not in your address book is known by their number too,
		// as on the phone: a name they chose alone doesn't title the chat.
		p := w.person(c.Other)
		if !p.contact && p.phone != "" && p.name != "+"+p.phone {
			return cut(p.name+" · +"+p.phone, 300)
		}
		return p.name
	}
	if c.Name != "" {
		return cut(oneLine(c.Name), 300)
	}
	var names []string
	others := 0
	for _, key := range c.Members {
		if w.isSelf(key) {
			continue
		}
		others++
		if len(names) == 3 {
			continue
		}
		first, _, _ := strings.Cut(w.person(key).name, " ")
		names = append(names, first)
	}
	switch {
	case len(names) == 0:
		return "Group"
	case others > len(names):
		return strings.Join(names, ", ") + " and " + strconv.Itoa(others-len(names)) + " more"
	}
	return strings.Join(names, ", ")
}

// recount works out c's unread messages: others' messages after ReadUpTo;
// w.mu is held.
func (w *WhatsApp) recount(c *chat) {
	w.ensureLoaded(c)
	n := 0
	for _, m := range c.msgs {
		if m.MS > c.ReadUpTo && !w.isSelf(m.Sender) && m.Service == nil {
			n++
		}
	}
	c.Unread = n
}

// unsent is up to limit chats load_chats hasn't sent, newest first, and how
// many more remain; w.mu is held.
func (w *WhatsApp) unsent(limit int) ([]*chat, int) {
	var all []*chat
	for _, c := range w.chats {
		if !w.sent[c.id] && w.visible(c) {
			all = append(all, c)
		}
	}
	slices.SortFunc(all, byOrder)
	if len(all) <= limit {
		return all, 0
	}
	return all[:limit], len(all) - limit
}

// LoadChats sends the next chats, newest activity first. Every chat this
// device has is kept here, so nothing is asked of WhatsApp.
func (w *WhatsApp) LoadChats(ctx context.Context, limit int) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	batch, rest := w.unsent(max(limit, 1))
	for _, c := range batch {
		w.sent[c.id] = true
		w.sendChat(c)
	}
	w.listed = rest == 0
	return rest > 0, nil
}

// removeChat drops a chat that left the list (deleted on the phone); w.mu
// is held.
func (w *WhatsApp) removeChat(c *chat) {
	w.ensureLoaded(c)
	for _, m := range c.msgs {
		w.forgetFiles(m)
	}
	delete(w.chats, c.key)
	delete(w.byID, c.id)
	if w.st != nil {
		ctx, cancel := dbCtx()
		if err := w.st.deleteChat(ctx, c.key); err != nil {
			w.storeFailed = true
			hlog.Error("whatsapp: can't delete a chat", hlog.Kind(err))
		}
		cancel()
		w.scrub = true
	}
	if w.sent[c.id] {
		delete(w.sent, c.id)
		w.d.Events.ChatRemoved(c.id)
	}
}

// chatOf is the chat a request names; w.mu is held.
func (w *WhatsApp) chatOf(id int64) (*chat, error) {
	c := w.byID[id]
	if c == nil {
		return nil, proto.ErrNoChat
	}
	return c, nil
}
