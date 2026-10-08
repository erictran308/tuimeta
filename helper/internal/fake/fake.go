// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fake is a made-up network for tuimeta's tests and for trying the
// app without an account: six chats of sixty messages each, people who type
// and answer, pictures it draws itself. It never touches the network.
package fake

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/download"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// EditWindow is how long (seconds) your text messages can be edited.
const EditWindow = 15 * 60

// Timings, as PROTOCOL.md gives them.
const (
	SentAfter   = 300 * time.Millisecond  // the network accepts a message
	TypingAfter = 500 * time.Millisecond  // a dm's person starts typing
	EchoAfter   = 1500 * time.Millisecond // and answers
	ChunkEvery  = 60 * time.Millisecond   // a download's pieces arrive
	Chunks      = 4
)

// LinkAfter is how long a linking network takes to see its code scanned or
// typed (tests shorten it).
var LinkAfter = 3 * time.Second

// The codes a fake link shows, and how long each says it works.
const (
	FakeQR      = "2@fake,dHVpbWV0YS1mYWtlLXFy,ZmFrZS1ub2lzZS1rZXk=,ZmFrZS1pZGVudGl0eQ==,ZmFrZS1hZHY="
	FakePairing = "FAKE-C0DE"
	QRLasts     = 60 * time.Second
	PairLasts   = 180 * time.Second
)

// Fake is one made-up network.
type Fake struct {
	net    proto.Network
	d      backend.Deps
	anchor time.Time
	selfID int64

	mu       sync.Mutex
	w        *world // nil once logged out, until the next login
	loggedIn bool
	sent     map[int64]bool // chats load_chats has sent since login
	seq      int            // numbers the messages sent this run
	base     context.Context
	live     context.Context // ends at logout or quit: stops pending echoes
	stop     context.CancelFunc
	linking  context.CancelFunc // ends the link waiting for its code, if any
	attempt  uint64             // the waiting link's attempt
}

var (
	_ backend.Backend = (*Fake)(nil)
	_ backend.Linker  = (*Fake)(nil)
)

// New builds the network's world at once, so its ids are given out in the
// same order every run. now picks the week the history covers.
func New(n proto.Network, d backend.Deps, now time.Time) *Fake {
	f := &Fake{net: n, d: d, anchor: now.UTC().Truncate(time.Hour), base: context.Background()}
	f.live, f.stop = context.WithCancel(f.base)
	f.w = f.build()
	return f
}

func (f *Fake) Network() proto.Network { return f.net }

func (f *Fake) self() int64 { return f.selfID }

func (f *Fake) Start(ctx context.Context) {
	f.mu.Lock()
	f.base = ctx
	f.stop()
	f.live, f.stop = context.WithCancel(ctx)
	f.mu.Unlock()
	f.d.Events.Account(f.net, proto.LoggedOut, 0, "", "")
}

func (f *Fake) Close() {
	f.mu.Lock()
	f.stop()
	f.mu.Unlock()
}

func (f *Fake) LoginCookies(ctx context.Context, _ cookies.Set, _ browser.Identity) error {
	if f.net.Links() {
		return proto.Err(proto.BadRequest, f.net.Title()+" logs in by linking tuimeta to your phone, not with cookies.")
	}
	f.mu.Lock()
	self := f.logIn()
	f.mu.Unlock()
	f.ready(self)
	return nil
}

// logIn makes the world the logged-in one; f.mu is held. ready reports it
// once the lock is let go.
func (f *Fake) logIn() *person {
	if f.w == nil {
		f.w = f.build()
	}
	f.loggedIn = true
	f.sent = map[int64]bool{}
	f.stop()
	f.live, f.stop = context.WithCancel(f.base)
	return f.w.self
}

func (f *Fake) ready(self *person) {
	f.d.Events.Account(f.net, proto.Connecting, 0, "", "")
	f.d.Events.Account(f.net, proto.Ready, self.id, self.name, "")
	f.d.Events.User(self.user(f.net))
}

var errLinkCancelled = proto.Err(proto.Cancelled, "Linking was cancelled.")

// Link shows a made-up code at once (a QR code, or with a phone number a
// pairing code) and takes it as scanned LinkAfter later, logging in as a
// cookie login does. A newer Link, CancelLink, Logout or ctx ending stops
// it first; a CancelLink naming another attempt doesn't.
func (f *Fake) Link(ctx context.Context, phone string, attempt uint64) error {
	f.mu.Lock()
	if f.loggedIn {
		f.mu.Unlock()
		return proto.Err(proto.BadRequest, "Already logged in to "+f.net.Title()+"; log out first.")
	}
	if f.linking != nil {
		f.linking()
	}
	wait, cancel := context.WithCancel(ctx)
	defer cancel()
	f.linking, f.attempt = cancel, attempt
	f.mu.Unlock()

	now := time.Now()
	if phone == "" {
		f.d.Events.LoginCode(f.net, attempt, FakeQR, "", now.Add(QRLasts))
	} else {
		f.d.Events.LoginCode(f.net, attempt, "", FakePairing, now.Add(PairLasts))
	}
	scanned := sleep(wait, LinkAfter) == nil

	f.mu.Lock()
	// Stopped as the code was taken: the stop wins, so nothing a newer link
	// or a logout meant to end gets logged in.
	if !scanned || wait.Err() != nil {
		f.mu.Unlock()
		return errLinkCancelled
	}
	f.linking = nil
	self := f.logIn()
	f.mu.Unlock()
	f.ready(self)
	return nil
}

func (f *Fake) CancelLink(attempt uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.linking != nil && (attempt == 0 || attempt == f.attempt) {
		f.linking()
		f.linking = nil
	}
}

func (f *Fake) Logout(ctx context.Context) error {
	f.mu.Lock()
	if f.linking != nil {
		f.linking()
		f.linking = nil
	}
	f.loggedIn = false
	f.w = nil
	f.sent = nil
	f.stop()
	f.mu.Unlock()
	if err := f.d.Session.Wipe(); err != nil {
		hlog.Warn("fake: can't wipe session", hlog.Kind(err))
	}
	f.d.Events.Account(f.net, proto.LoggedOut, 0, "", "")
	return nil
}

// world is the logged-in world; f.mu is held.
func (f *Fake) world() (*world, error) {
	if !f.loggedIn || f.w == nil {
		return nil, proto.ErrLoggedOut(f.net)
	}
	return f.w, nil
}

func (f *Fake) chat(id int64) (*world, *chat, error) {
	w, err := f.world()
	if err != nil {
		return nil, nil, err
	}
	c := w.byChat[id]
	if c == nil {
		return nil, nil, proto.ErrNoChat
	}
	return w, c, nil
}

func (f *Fake) message(ref backend.MessageRef) (*chat, *netMsg, error) {
	_, c, err := f.chat(ref.Chat.ID)
	if err != nil {
		return nil, nil, err
	}
	m := c.msgs[ref.NetID]
	if m == nil {
		return nil, nil, proto.ErrNoMessage
	}
	return c, m, nil
}

func (f *Fake) LoadChats(ctx context.Context, limit int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, err := f.world()
	if err != nil {
		return false, err
	}
	var batch []*chat
	more := false
	for _, c := range f.ordered() {
		if f.sent[c.id] {
			continue
		}
		if len(batch) == limit {
			more = true
			break
		}
		batch = append(batch, c)
	}
	told := map[int64]bool{}
	tell := func(p *person) {
		if p != nil && !told[p.id] {
			told[p.id] = true
			f.d.Events.User(p.user(f.net))
		}
	}
	tell(w.self)
	for _, c := range batch {
		for _, p := range c.members {
			tell(p)
		}
	}
	for _, c := range batch {
		f.sent[c.id] = true
		f.d.Events.Chat(f.chatObject(c))
	}
	return more, nil
}

func (f *Fake) History(ctx context.Context, ref backend.ChatRef, q history.Query) (history.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, c, err := f.chat(ref.ID)
	if err != nil {
		return history.Page{}, err
	}
	return c.log.Page(q), nil
}

func (f *Fake) GetMessage(ctx context.Context, ref backend.MessageRef) (proto.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, _, err := f.message(ref)
	if err != nil {
		return proto.Message{}, err
	}
	m, ok := c.log.Get(ref.ID)
	if !ok {
		return proto.Message{}, proto.ErrNoMessage
	}
	return m, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Fake) Send(ctx context.Context, out *backend.Outgoing) error {
	if err := sleep(ctx, SentAfter); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w, c, err := f.chat(out.Chat.ID)
	if err != nil {
		return err
	}
	f.seq++
	m := &netMsg{
		netID: fmt.Sprintf("sent.%s.%d", c.netID, f.seq),
		ms:    time.Now().UnixMilli(),
		from:  w.self,
		text:  out.Text,
	}
	if out.ReplyTo != nil {
		if ans := c.msgs[out.ReplyTo.NetID]; ans != nil {
			m.reply = &proto.ReplyTo{MessageID: ans.ids[0], SenderID: ans.from.id, Text: quote(ans)}
		}
	}
	for i, u := range out.Files {
		m.media = append(m.media, f.uploaded(fmt.Sprintf("upload:%s:%d", m.netID, i), u))
	}
	if len(m.media) == 0 {
		m.editableUntil = m.ms/1000 + EditWindow
	}
	f.store(c, m)
	c.request = false // answering a request accepts it
	parts, _ := f.partsOf(c, m)
	out.Sent(parts...)
	f.d.Events.Chat(f.chatObject(c))
	if c.kind == proto.DM {
		said := out.Text
		if said == "" {
			said = fmt.Sprintf("[%d file(s)]", len(out.Files))
		}
		live := f.live
		chatID, lastPart := c.id, lastID(m)
		hlog.Go("fake echo", func() { f.echo(live, chatID, lastPart, said) })
	}
	return nil
}

// partsOf is the protocol messages the log holds for m.
func (f *Fake) partsOf(c *chat, m *netMsg) ([]proto.Message, bool) {
	var parts []proto.Message
	for _, id := range m.ids {
		p, ok := c.log.Get(id)
		if !ok {
			return nil, false
		}
		parts = append(parts, p)
	}
	return parts, true
}

func (f *Fake) uploaded(key string, u backend.Upload) proto.Media {
	data := u.Data
	id := f.d.Files.Register(ids.FileRef{
		Network: f.net, Key: key, Size: int64(len(data)), Mime: u.Mime, Name: u.Name,
		Source: &fakeFile{gen: func() []byte { return data }},
	})
	m := proto.Media{Kind: u.Kind, FileID: id, Name: u.Name, Mime: u.Mime, Size: int64(len(data)), Width: u.Width, Height: u.Height}
	if (u.Kind == proto.Photo || u.Kind == proto.GIF) && u.Width > 0 {
		m.Thumbnail = &proto.Image{FileID: id, Width: u.Width, Height: u.Height}
	}
	return m
}

// echo is the dm's other person reading your message, typing, and
// answering "echo: …".
func (f *Fake) echo(live context.Context, chatID, yours int64, said string) {
	if sleep(live, TypingAfter) != nil {
		return
	}
	f.mu.Lock()
	_, c, err := f.chat(chatID)
	if err != nil || live.Err() != nil || c.with == nil {
		f.mu.Unlock()
		return
	}
	c.readOutbox = max(c.readOutbox, yours)
	f.d.Events.Read(c.id, 0, c.readOutbox, nil)
	f.d.Events.Typing(c.id, c.with.id, true)
	f.mu.Unlock()

	if sleep(live, EchoAfter) != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, c, err = f.chat(chatID)
	if err != nil || live.Err() != nil {
		return
	}
	f.seq++
	m := &netMsg{
		netID: fmt.Sprintf("echo.%s.%d", c.netID, f.seq),
		ms:    time.Now().UnixMilli(),
		from:  c.with,
		text:  "echo: " + said,
	}
	f.store(c, m)
	parts, _ := f.partsOf(c, m)
	for _, p := range parts {
		f.d.Events.Message(p)
	}
	f.d.Events.Chat(f.chatObject(c))
}

func (f *Fake) EditText(ctx context.Context, ref backend.MessageRef, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, m, err := f.message(ref)
	if err != nil {
		return err
	}
	if !m.from.self {
		return proto.Err(proto.BadRequest, "You can only edit your own messages.")
	}
	if len(m.media) > 0 || m.service != "" || m.unsupported != "" {
		return proto.Err(proto.Unsupported, "Only text messages can be edited.")
	}
	if time.Now().Unix() > m.editableUntil {
		return proto.Err(proto.Unsupported, "This message is too old to edit; messages can be edited for 15 minutes.")
	}
	m.text = text
	m.entities = nil
	m.preview = nil
	m.edited = true
	f.update(c, m)
	return nil
}

// update puts m's new parts in the log and reports them, and the chat if m
// is its newest message.
func (f *Fake) update(c *chat, m *netMsg) {
	parts := f.parts(c, m)
	c.log.Put(parts...)
	for _, p := range parts {
		f.d.Events.Message(p)
	}
	if last, ok := c.log.Newest(); ok && slices.Contains(m.ids, last.ID) {
		f.d.Events.Chat(f.chatObject(c))
	}
}

func (f *Fake) Delete(ctx context.Context, ref backend.MessageRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, m, err := f.message(ref)
	if err != nil {
		return err
	}
	if !m.from.self || m.service != "" {
		return proto.Err(proto.BadRequest, "You can only unsend your own messages.")
	}
	c.log.Remove(m.ids...)
	delete(c.msgs, m.netID)
	f.d.Events.MessageDeleted(c.id, m.ids)
	f.d.Events.Chat(f.chatObject(c))
	return nil
}

func (f *Fake) React(ctx context.Context, ref backend.MessageRef, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, m, err := f.message(ref)
	if err != nil {
		return err
	}
	if m.service != "" {
		return proto.Err(proto.BadRequest, "Events can't be reacted to.")
	}
	self := f.self()
	m.reactions = slices.DeleteFunc(m.reactions, func(r reaction) bool { return r.user == self })
	if emoji != "" {
		m.reactions = append(m.reactions, reaction{self, emoji})
	}
	f.update(c, m)
	return nil
}

func (f *Fake) MarkRead(ctx context.Context, ref backend.MessageRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, m, err := f.message(ref)
	if err != nil {
		return err
	}
	c.readInbox = max(c.readInbox, lastID(m))
	unread := c.unread()
	f.d.Events.Read(c.id, c.readInbox, 0, &unread)
	return nil
}

func (f *Fake) SetTyping(ctx context.Context, ref backend.ChatRef, typing bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _, err := f.chat(ref.ID)
	return err // nobody on a fake network sees you type
}

func (f *Fake) Mute(ctx context.Context, ref backend.ChatRef, muted bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, c, err := f.chat(ref.ID)
	if err != nil {
		return err
	}
	c.muted = muted
	f.d.Events.Chat(f.chatObject(c))
	return nil
}

func (f *Fake) Search(ctx context.Context, query string) ([]proto.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, err := f.world()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	// WhatsApp knows people by their numbers too: a query of three digits
	// or more (spaces, dashes and a "+" aside) finds the numbers holding them.
	digits := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return r
		case r == ' ' || r == '-' || r == '+':
			return -1
		}
		return 'x'
	}, query)
	byNumber := f.net == proto.WhatsApp && len(digits) >= 3 && !strings.Contains(digits, "x")
	results := []proto.SearchResult{}
	for _, p := range w.people {
		if !strings.Contains(strings.ToLower(p.name), q) && !strings.Contains(strings.ToLower(p.username), q) &&
			!(byNumber && strings.Contains(p.netID, digits)) {
			continue
		}
		res := proto.SearchResult{UserID: p.id, Title: p.name, Username: p.username, Kind: proto.DM}
		if c := w.dmWith(p); c != nil {
			res.ChatID = c.id
		}
		results = append(results, res)
	}
	for _, c := range w.chats {
		if c.kind == proto.Group && strings.Contains(strings.ToLower(c.title), q) {
			results = append(results, proto.SearchResult{ChatID: c.id, Title: c.title, Kind: proto.Group})
		}
	}
	return results, nil
}

func (w *world) dmWith(p *person) *chat {
	for _, c := range w.chats {
		if c.with == p {
			return c
		}
	}
	return nil
}

func (f *Fake) OpenDM(ctx context.Context, ref backend.UserRef) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, err := f.world()
	if err != nil {
		return 0, err
	}
	p := w.byUser[ref.ID]
	if p == nil || p.self {
		return 0, proto.ErrNoUser
	}
	if c := w.dmWith(p); c != nil {
		return c.id, nil
	}
	c := &chat{
		netID: "dm." + p.netID, kind: proto.DM, title: p.name, with: p, members: []*person{p},
		photo: p.photo, created: time.Now().UnixMilli(), log: history.New(), msgs: map[string]*netMsg{},
	}
	c.log.SetComplete(true)
	c.id = f.d.IDs.Chat(f.net, c.netID)
	w.chats = append(w.chats, c)
	w.byChat[c.id] = c
	f.sent[c.id] = true
	f.d.Events.User(p.user(f.net))
	f.d.Events.Chat(f.chatObject(c))
	return c.id, nil
}

func (f *Fake) Fetch(ctx context.Context, ref ids.FileRef, w io.Writer) error {
	src, ok := ref.Source.(*fakeFile)
	if !ok {
		return proto.ErrNoFile
	}
	data := src.bytes()
	download.SetSize(w, int64(len(data)))
	chunk := (len(data) + Chunks - 1) / Chunks
	for i := 0; i < len(data); i += chunk {
		if i > 0 {
			if err := sleep(ctx, ChunkEvery); err != nil {
				return err
			}
		}
		if _, err := w.Write(data[i:min(i+chunk, len(data))]); err != nil {
			return err
		}
	}
	return nil
}
