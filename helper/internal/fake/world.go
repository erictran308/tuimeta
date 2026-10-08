// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"cmp"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// HistoryLen is how many messages each fake chat starts with.
const HistoryLen = 60

type person struct {
	netID, name, username string
	id                    int64
	photo                 *proto.Image
	activeAt              int64
	self                  bool
}

type chat struct {
	netID                               string
	id                                  int64
	kind                                proto.ChatKind
	title                               string
	with                                *person   // a dm's other person
	members                             []*person // everyone but you
	photo                               *proto.Image
	muted, archived, encrypted, request bool
	created                             int64 // ms, the order of a chat without messages
	log                                 *history.Log
	msgs                                map[string]*netMsg
	readInbox, readOutbox               int64
}

type reaction struct {
	user  int64
	emoji string
}

// netMsg is a message as the fake network has it: one, whatever its parts.
type netMsg struct {
	netID         string
	ms            int64
	from          *person
	text          string
	entities      []proto.Entity
	media         []proto.Media
	reply         *proto.ReplyTo
	forwarded     bool
	edited        bool
	service       string
	unsupported   string
	preview       *proto.LinkPreview
	reactions     []reaction
	editableUntil int64
	ids           []int64
}

type world struct {
	self   *person
	people []*person // not you
	byUser map[int64]*person
	chats  []*chat
	byChat map[int64]*chat
}

// fakeFile is a fake download: drawn or made up the first time it's asked.
type fakeFile struct {
	once sync.Once
	gen  func() []byte
	data []byte
}

func (f *fakeFile) bytes() []byte {
	f.once.Do(func() { f.data = f.gen() })
	return f.data
}

// --- what each network holds -------------------------------------------

type personDef struct {
	netID, name, username string
	color                 int // -1: no photo
	activeAgo             time.Duration
}

type chatDef struct {
	netID       string
	kind        proto.ChatKind
	title       string   // a group's; a dm is named after its person
	members     []string // people's net ids, not you
	groupPhoto  int      // -1: none
	muted       bool
	archived    bool
	encrypted   bool
	request     bool
	unread      int
	outboxLag   bool // they haven't read your last message
	strangers   bool // only they write (a request)
	specials    map[int]spec
	fillerStart int
}

type networkDef struct {
	self   personDef
	people []personDef
	chats  []chatDef
	offset time.Duration // shifts this network's activity, so chats interleave
}

func definition(n proto.Network) networkDef {
	if n == proto.Instagram {
		return instagram()
	}
	return messenger()
}

func messenger() networkDef {
	return networkDef{
		self: personDef{"100001", "Robin Hale", "robin.hale", 1, 0},
		people: []personDef{
			{"100002", "Alice Example", "alice.example", 0, 4 * time.Minute},
			{"100003", "Ben Carter", "ben.carter", 6, 2 * time.Hour},
			{"100004", "Chloé Martin", "chloe.martin", 2, 0},
			{"100005", "Dmitri Ivanov", "", 3, 3 * 24 * time.Hour},
			{"100006", "Eve Nakamura", "eve.nakamura", 5, 0},
			{"100007", "Farid Haddad", "", -1, 0},
		},
		chats: []chatDef{
			{netID: "300001", kind: proto.DM, members: []string{"100002"}, groupPhoto: -1, encrypted: true, unread: 2, specials: aliceChat()},
			{netID: "300002", kind: proto.Group, title: "Weekend Trip ⛺", members: []string{"100002", "100003", "100004", "100005"}, groupPhoto: 4, unread: 5, specials: tripChat()},
			{netID: "300003", kind: proto.DM, members: []string{"100003"}, groupPhoto: -1, muted: true, unread: 3, specials: benChat(), fillerStart: 5},
			{netID: "300004", kind: proto.Group, title: "Book Club 📚", members: []string{"100002", "100006", "100007"}, groupPhoto: -1, specials: bookChat(), fillerStart: 11},
			{netID: "300005", kind: proto.DM, members: []string{"100004"}, groupPhoto: -1, outboxLag: true, specials: chloeChat(), fillerStart: 17},
			{netID: "300006", kind: proto.DM, members: []string{"100005"}, groupPhoto: -1, archived: true, unread: 1, specials: dmitriChat(), fillerStart: 23},
		},
	}
}

func instagram() networkDef {
	return networkDef{
		self: personDef{"200001", "Robin Hale", "robin.hale", 1, 0},
		people: []personDef{
			{"200002", "Maya Lopez", "maya.lens", 3, 12 * time.Minute},
			{"200003", "Sam Rivera", "sam.climbs", 2, time.Hour},
			{"200004", "Noor Aziz", "noor.draws", 5, 0},
			{"200005", "Oliver King", "oliver.k", 6, 26 * time.Hour},
			{"200006", "Jun Park", "jun.park", 0, 0},
			{"200007", "Promo Deals", "deals.4.you", -1, 0},
			{"200008", "Lena Vogel", "lena.v", 4, 0},
		},
		offset: 19 * time.Minute,
		chats: []chatDef{
			{netID: "400001", kind: proto.DM, members: []string{"200002"}, groupPhoto: -1, unread: 1, specials: mayaChat(), fillerStart: 3},
			{netID: "400002", kind: proto.Group, title: "Climbing crew 🧗", members: []string{"200002", "200003", "200004", "200005"}, groupPhoto: 1, unread: 4, specials: crewChat(), fillerStart: 9},
			{netID: "400003", kind: proto.DM, members: []string{"200005"}, groupPhoto: -1, muted: true, specials: oliverChat(), fillerStart: 15},
			{netID: "400004", kind: proto.DM, members: []string{"200007"}, groupPhoto: -1, request: true, unread: 2, strangers: true, specials: dealsChat()},
			{netID: "400005", kind: proto.DM, members: []string{"200004"}, groupPhoto: -1, specials: noorChat(), fillerStart: 21},
			{netID: "400006", kind: proto.DM, members: []string{"200006"}, groupPhoto: -1, specials: junChat(), fillerStart: 27},
		},
	}
}

// --- building ------------------------------------------------------------

// build makes the network's world, giving everything its ids in a fixed
// order, so they're the same every run.
func (f *Fake) build() *world {
	def := definition(f.net)
	w := &world{byUser: map[int64]*person{}, byChat: map[int64]*chat{}}
	addPerson := func(d personDef, self bool) *person {
		p := &person{netID: d.netID, name: d.name, username: d.username, self: self}
		p.id = f.d.IDs.User(f.net, d.netID)
		if d.color >= 0 {
			p.photo = f.picture("avatar:"+d.netID, art{avatarArt, 160, 160, d.color})
		}
		if d.activeAgo > 0 {
			p.activeAt = f.anchor.Add(-d.activeAgo).Unix()
		}
		w.byUser[p.id] = p
		return p
	}
	w.self = addPerson(def.self, true)
	f.selfID = w.self.id
	byNet := map[string]*person{}
	for _, d := range def.people {
		p := addPerson(d, false)
		w.people = append(w.people, p)
		byNet[d.netID] = p
	}
	for i, cd := range def.chats {
		c := &chat{
			netID: cd.netID, kind: cd.kind, title: cd.title,
			muted: cd.muted, archived: cd.archived, encrypted: cd.encrypted, request: cd.request,
			log: history.New(), msgs: map[string]*netMsg{},
		}
		c.id = f.d.IDs.Chat(f.net, cd.netID)
		for _, m := range cd.members {
			c.members = append(c.members, byNet[m])
		}
		if cd.kind == proto.DM {
			c.with = c.members[0]
			c.title = c.with.name
			c.photo = c.with.photo
		} else if cd.groupPhoto >= 0 {
			c.photo = f.picture("chatphoto:"+cd.netID, art{blocksArt, 160, 160, cd.groupPhoto})
		}
		f.fill(w, c, cd, i, def.offset)
		w.chats = append(w.chats, c)
		w.byChat[c.id] = c
	}
	return w
}

// at is when message i of chat number c was sent: spread over the last
// week, a few hours apart, the first chat's newest a few minutes before the
// anchor (the start of this hour, so a run's ids are the same all hour).
func (f *Fake) at(c, i int, offset time.Duration) int64 {
	t := f.anchor.Add(-offset - time.Duration(c)*41*time.Minute - 3*time.Minute)
	t = t.Add(-time.Duration(HistoryLen-1-i) * 168 * time.Minute)
	t = t.Add(-time.Duration((i*37)%50) * time.Minute)
	t = t.Add(-time.Duration((i*13)%60) * time.Second)
	return t.UnixMilli() - int64((i*7+c)%1000)
}

// fill writes the chat's history from its specials and filler lines.
func (f *Fake) fill(w *world, c *chat, cd chatDef, ci int, offset time.Duration) {
	who := func(k int) *person {
		if k <= 0 || k > len(c.members) {
			return w.self
		}
		return c.members[k-1]
	}
	firstUnread := HistoryLen - cd.unread
	msgs := make([]*netMsg, HistoryLen)
	var lastMs int64
	for i := range HistoryLen {
		s, special := cd.specials[i]
		if !special {
			s = filler(c, cd, ci, i)
		}
		if cd.strangers || (i >= firstUnread && s.from == 0) {
			s.from = 1 // a request has only their messages; unread ones are theirs
		}
		m := &netMsg{
			netID:       fmt.Sprintf("m.%s.%02d", c.netID, i),
			ms:          f.at(ci, i, offset),
			from:        who(s.from),
			text:        s.text,
			forwarded:   s.fwd,
			edited:      s.edited,
			service:     s.service,
			unsupported: s.unsup,
		}
		if s.sameMs && i > 0 {
			m.ms = lastMs
		}
		lastMs = m.ms
		for _, e := range s.ents {
			ent, ok := proto.EntityFor(m.text, e.sub, e.typ)
			if !ok {
				panic("fake: entity text not in its message")
			}
			ent.URL = e.url
			if e.user > 0 {
				ent.UserID = who(e.user).id
			}
			m.entities = append(m.entities, ent)
		}
		for k, ms := range s.media {
			m.media = append(m.media, f.mediaFor(fmt.Sprintf("media:%s:%02d:%d", c.netID, i, k), ms))
		}
		if s.re > 0 {
			ans := msgs[s.re]
			m.reply = &proto.ReplyTo{MessageID: ans.ids[0], SenderID: ans.from.id, Text: quote(ans)}
		}
		if s.preview != nil {
			p := *s.preview
			if s.previewArt {
				p.Image = f.picture("card:"+c.netID, art{cardArt, 600, 315, ci + 2})
			}
			m.preview = &p
		}
		for _, r := range s.reacts {
			m.reactions = append(m.reactions, reaction{who(r.who).id, r.emoji})
		}
		if m.from.self && len(m.media) == 0 && m.service == "" && m.unsupported == "" {
			m.editableUntil = m.ms/1000 + EditWindow
		}
		msgs[i] = m
		f.store(c, m)
	}
	c.readInbox = lastID(msgs[firstUnread-1])
	for i := HistoryLen - 1; i >= 0; i-- {
		if msgs[i].from.self && msgs[i].service == "" {
			c.readOutbox = lastID(msgs[i])
			if !cd.outboxLag {
				break
			}
			cd.outboxLag = false // keep looking: the one before is the last read
		}
	}
	c.log.SetComplete(true)
}

func lastID(m *netMsg) int64 { return m.ids[len(m.ids)-1] }

// quote is how a reply shows the message it answers.
func quote(m *netMsg) string {
	switch {
	case m.text != "":
		return proto.Snippet(m.text, 100)
	case len(m.media) > 0:
		return mediaLabel(m.media[0].Kind)
	case m.unsupported != "":
		return m.unsupported
	}
	return ""
}

func mediaLabel(k proto.MediaKind) string {
	switch k {
	case proto.Photo:
		return "Photo"
	case proto.Video:
		return "Video"
	case proto.Sticker:
		return "Sticker"
	case proto.Voice:
		return "Voice message"
	case proto.GIF:
		return "GIF"
	case proto.Audio:
		return "Audio"
	}
	return "File"
}

// store gives a network message its ids and puts its parts in the log.
func (f *Fake) store(c *chat, m *netMsg) {
	c.msgs[m.netID] = m
	c.log.Put(f.parts(c, m)...)
}

// parts is the network message as protocol messages (several for an album).
func (f *Fake) parts(c *chat, m *netMsg) []proto.Message {
	m.ids = f.d.Messages.Assign(c.id, m.netID, m.ms, len(m.media))
	base := proto.Message{
		ChatID:      c.id,
		SenderID:    m.from.id,
		Outgoing:    m.from.self,
		Date:        m.ms / 1000,
		Text:        m.text,
		Entities:    m.entities,
		ReplyTo:     m.reply,
		Forwarded:   m.forwarded,
		Reactions:   tally(m.reactions, f.self()),
		Edited:      m.edited,
		Deletable:   m.from.self && m.service == "",
		State:       proto.Sent,
		Service:     m.service,
		LinkPreview: m.preview,
		Unsupported: m.unsupported,
	}
	if m.editableUntil > 0 {
		base.EditableUntil = m.editableUntil
	}
	return proto.SplitAlbum(base, m.media, m.ids)
}

// tally counts reactions by emoji, in the order each was first chosen.
func tally(rs []reaction, self int64) []proto.Reaction {
	var out []proto.Reaction
	for _, r := range rs {
		i := slices.IndexFunc(out, func(x proto.Reaction) bool { return x.Emoji == r.emoji })
		if i < 0 {
			out = append(out, proto.Reaction{Emoji: r.emoji})
			i = len(out) - 1
		}
		out[i].Count++
		out[i].Mine = out[i].Mine || r.user == self
	}
	return out
}

// picture registers a drawn picture as a file.
func (f *Fake) picture(key string, a art) *proto.Image {
	id := f.d.Files.Register(ids.FileRef{
		Network: f.net, Key: key, Mime: "image/png", Name: key2name(key) + ".png",
		Source: &fakeFile{gen: a.png},
	})
	return &proto.Image{FileID: id, Width: a.w, Height: a.h}
}

func (f *Fake) blob(key, name, mime string, data []byte) int32 {
	return f.d.Files.Register(ids.FileRef{
		Network: f.net, Key: key, Mime: mime, Name: name, Size: int64(len(data)),
		Source: &fakeFile{gen: func() []byte { return data }},
	})
}

func key2name(key string) string {
	out := []byte(key)
	for i, b := range out {
		if b == ':' {
			out[i] = '-'
		}
	}
	return string(out)
}

func (f *Fake) mediaFor(key string, ms mediaSpec) proto.Media {
	switch ms.kind {
	case proto.Photo:
		if ms.viewOnce {
			return proto.Media{Kind: proto.Photo, ViewOnce: true}
		}
		img := f.picture(key, ms.art)
		return proto.Media{Kind: proto.Photo, FileID: img.FileID, Thumbnail: img, Width: img.Width, Height: img.Height, Mime: "image/png", Name: key2name(key) + ".png"}
	case proto.Sticker:
		img := f.picture(key, ms.art)
		return proto.Media{Kind: proto.Sticker, FileID: img.FileID, Thumbnail: img, Width: img.Width, Height: img.Height, Mime: "image/png"}
	case proto.Video:
		data := mp4Box("mp42", 2048)
		id := f.blob(key, ms.name, "video/mp4", data)
		thumb := f.picture(key+":thumb", ms.art)
		return proto.Media{Kind: proto.Video, FileID: id, Thumbnail: thumb, Width: thumb.Width, Height: thumb.Height,
			Duration: ms.duration, Name: ms.name, Mime: "video/mp4", Size: int64(len(data))}
	case proto.Voice:
		data := mp4Box("M4A ", 1024)
		id := f.blob(key, ms.name, "audio/mp4", data)
		return proto.Media{Kind: proto.Voice, FileID: id, Duration: ms.duration, Name: ms.name, Mime: "audio/mp4", Size: int64(len(data))}
	default:
		data := pdf(ms.title)
		id := f.blob(key, ms.name, ms.mime, data)
		return proto.Media{Kind: proto.FileMedia, FileID: id, Name: ms.name, Mime: ms.mime, Size: int64(len(data))}
	}
}

// chatObject is the chat as tuimeta sees it; f.mu is held.
func (f *Fake) chatObject(c *chat) proto.Chat {
	out := proto.Chat{
		ID: c.id, Network: f.net, Kind: c.kind, Title: c.title, Photo: c.photo,
		Order: c.created, Unread: c.unread(), Muted: c.muted, Archived: c.archived,
		Encrypted: c.encrypted, Request: c.request, CanSend: true,
		ReadInbox: c.readInbox, ReadOutbox: c.readOutbox,
	}
	if c.with != nil {
		out.UserID = c.with.id
	}
	if last, ok := c.log.Newest(); ok {
		out.LastMessage = &last
		out.Order = ids.Millis(last.ID)
	}
	return out
}

// unread counts others' messages after how far you've read.
func (c *chat) unread() int {
	n := 0
	for _, m := range c.log.All() {
		if m.ID > c.readInbox && !m.Outgoing && m.Service == "" {
			n++
		}
	}
	return n
}

func (p *person) user(n proto.Network) proto.User {
	return proto.User{ID: p.id, Network: n, Name: p.name, Username: p.username, Photo: p.photo, IsSelf: p.self, ActiveAt: p.activeAt}
}

// ordered is the chats, newest activity first (ties by id).
func (f *Fake) ordered() []*chat {
	cs := slices.Clone(f.w.chats)
	order := func(c *chat) int64 {
		if last, ok := c.log.Newest(); ok {
			return ids.Millis(last.ID)
		}
		return c.created
	}
	slices.SortStableFunc(cs, func(a, b *chat) int {
		if o := cmp.Compare(order(b), order(a)); o != 0 {
			return o
		}
		return cmp.Compare(a.id, b.id)
	})
	return cs
}
