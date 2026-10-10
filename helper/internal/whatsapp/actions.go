// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"slices"
	"strings"
	"time"

	"go.mau.fi/util/variationselector"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// MaxSearchResults bounds what a search answers with.
const MaxSearchResults = 25

// jidOf is the JID of a person's key, for a request; w.mu is held.
func (w *WhatsApp) jidOf(key string) waTypes.JID {
	j, _ := waTypes.ParseJID(w.resolve(key))
	return j
}

// replyContext is how a message being sent says what it answers; w.mu is
// held. The quoted message goes with a snippet of its text, as WhatsApp's
// apps quote it.
func (w *WhatsApp) replyContext(c *chat, cli waAPI, ref *backend.MessageRef) *waE2E.ContextInfo {
	var ci *waE2E.ContextInfo
	if ref != nil {
		w.ensureLoaded(c)
		if replied := c.msgs[ref.NetID]; replied != nil {
			participant := w.jidOf(replied.Sender)
			if w.isSelf(replied.Sender) {
				participant = cli.OwnID()
			}
			ci = &waE2E.ContextInfo{
				StanzaID:      gproto.String(replied.ID),
				Participant:   gproto.String(participant.String()),
				QuotedMessage: &waE2E.Message{Conversation: gproto.String(quoteFor(replied))},
			}
		}
	}
	if c.Ephemeral > 0 {
		// In a chat with disappearing messages, what's sent disappears too.
		if ci == nil {
			ci = &waE2E.ContextInfo{}
		}
		ci.Expiration = gproto.Uint32(c.Ephemeral)
	}
	return ci
}

func (w *WhatsApp) Send(ctx context.Context, out *backend.Outgoing) error {
	cli, err := w.connected()
	if err != nil {
		return err
	}
	w.mu.Lock()
	c, err := w.chatOf(out.Chat.ID)
	if err == nil && (c.ReadOnly || !sendable(c.jid())) {
		err = errCantSend
	}
	if err != nil {
		w.mu.Unlock()
		return err
	}
	jid := c.jid()
	ci := w.replyContext(c, cli, out.ReplyTo)
	w.mu.Unlock()

	var msgs []*waE2E.Message
	if len(out.Files) == 0 {
		msgs = append(msgs, textMessage(out.Text, ci))
	}
	for i, f := range out.Files {
		caption := ""
		last := i == len(out.Files)-1
		if last && f.Kind != proto.Audio && f.Kind != proto.Voice {
			caption = out.Text
		}
		ctxInfo := ci
		if i > 0 {
			ctxInfo = withoutReply(ci)
		}
		m, err := w.fileMessage(ctx, cli, f, caption, ctxInfo)
		if err != nil {
			return err
		}
		msgs = append(msgs, m)
	}
	if n := len(out.Files); n > 0 && out.Text != "" && (out.Files[n-1].Kind == proto.Audio || out.Files[n-1].Kind == proto.Voice) {
		// A recording carries no caption: the text follows as its own message.
		msgs = append(msgs, textMessage(out.Text, withoutReply(ci)))
	}
	var parts []proto.Message
	for _, m := range msgs {
		part, err := w.sendOne(ctx, cli, c, jid, m)
		if err != nil {
			// What went out before the failure is sent, the rest failed.
			out.Partly(err, parts...)
			if len(parts) >= len(out.TempIDs) {
				// The failure was the text sent after a recording, which
				// tuimeta had no message for: it's said instead.
				w.d.Events.Error(net, "The text after the recording couldn't be sent; send it again.")
			}
			return err
		}
		parts = append(parts, part)
	}
	out.Sent(parts...)
	w.mu.Lock()
	if cur := w.chats[w.resolve(c.key)]; cur != nil {
		w.touch(cur)
	}
	w.mu.Unlock()
	return nil
}

// errCantSend refuses what would go to a chat tuimeta doesn't send to: one
// that isn't a person's or a group's (a bot's, say), or one you can't post in.
var errCantSend = proto.Err(proto.Unsupported, "You can't send messages in this chat.")

// stillHere reports whether c is still the chat by its key: not logged out
// of, deleted on the phone or merged into another while a request was out;
// w.mu is held.
func (w *WhatsApp) stillHere(c *chat) bool { return w.chats[c.key] == c }

// textMessage is text as a WhatsApp message, with its context if any.
func textMessage(text string, ci *waE2E.ContextInfo) *waE2E.Message {
	if ci == nil {
		return &waE2E.Message{Conversation: gproto.String(text)}
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: gproto.String(text), ContextInfo: ci}}
}

// withoutReply is ci for the second and later messages of a send: they keep
// the timer but answer nothing.
func withoutReply(ci *waE2E.ContextInfo) *waE2E.ContextInfo {
	if ci == nil || ci.GetExpiration() == 0 {
		return nil
	}
	return &waE2E.ContextInfo{Expiration: ci.Expiration}
}

// sendOne sends one message and keeps it as sent from here.
func (w *WhatsApp) sendOne(ctx context.Context, cli waAPI, c *chat, jid waTypes.JID, m *waE2E.Message) (proto.Message, error) {
	id := cli.GenerateMessageID()
	resp, err := cli.SendMessage(ctx, jid, m, whatsmeow.SendRequestExtra{ID: id})
	if err != nil {
		hlog.Info("whatsapp: send failed", hlog.Kind(err))
		return proto.Message{}, requestError(err)
	}
	ts := resp.Timestamp
	if ts.IsZero() {
		ts = w.now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	evt := &events.Message{Message: m}
	evt.Info.Chat, evt.Info.Sender, evt.Info.IsFromMe = jid, cli.OwnID(), true
	evt.Info.IsGroup = jid.Server == waTypes.GroupServer
	evt.Info.ID, evt.Info.Timestamp = id, ts
	ch := reader{canon: w.canon}.read(evt, w.self)
	if ch.kind != added {
		return proto.Message{}, proto.Err(proto.Internal, "The message went out but couldn't be shown; reopen the chat.")
	}
	// The chat may have been deleted on the phone, or found to be one with
	// another, while this went out: it's where the message is now.
	cur := w.chatFor(c.key, c.kind == proto.Group)
	w.ensureLoaded(cur)
	part := w.keep(cur, ch.msg)
	w.store(cur, ch.msg)
	cur.Activity = max(cur.Activity, ch.msg.MS)
	w.saveChat(cur)
	w.tellPeople(ch.msg)
	return part, nil
}

func (w *WhatsApp) EditText(ctx context.Context, ref backend.MessageRef, text string) error {
	cli, err := w.connected()
	if err != nil {
		return err
	}
	w.mu.Lock()
	c, msg, err := w.messageOf(ref)
	if err == nil {
		switch {
		case !sendable(c.jid()):
			err = errCantSend
		case !w.isSelf(msg.Sender):
			err = proto.Err(proto.BadRequest, "You can only edit your own messages.")
		case msg.Media != nil || msg.Service != nil || msg.Unsupported != "":
			err = proto.Err(proto.Unsupported, "Only text messages can be edited.")
		case w.now().Unix() > msg.MS/1000+EditWindow:
			err = proto.Err(proto.Unsupported, "This message is too old to edit; WhatsApp allows 15 minutes.")
		}
	}
	var jid waTypes.JID
	if err == nil {
		jid = c.jid()
	}
	w.mu.Unlock()
	if err != nil {
		return err
	}
	edit := cli.BuildEdit(jid, msg.ID, &waE2E.Message{Conversation: gproto.String(text)})
	if _, err := cli.SendMessage(ctx, jid, edit); err != nil {
		hlog.Info("whatsapp: edit failed", hlog.Kind(err))
		return requestError(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stillHere(c) {
		return nil
	}
	if cur := c.msgs[msg.ID]; cur != nil {
		cur.Text, cur.Mentions, cur.Preview = text, nil, nil
		cur.Edited, cur.EditMS = true, w.now().UnixMilli()
		w.changed(c, cur)
	}
	return nil
}

func (w *WhatsApp) Delete(ctx context.Context, ref backend.MessageRef) error {
	cli, err := w.connected()
	if err != nil {
		return err
	}
	w.mu.Lock()
	c, msg, err := w.messageOf(ref)
	if err == nil {
		switch {
		case !sendable(c.jid()):
			err = errCantSend
		case !w.isSelf(msg.Sender) || msg.Service != nil:
			err = proto.Err(proto.BadRequest, "You can only delete your own messages.")
		case w.now().Unix() > msg.MS/1000+RevokeWindow:
			err = proto.Err(proto.Unsupported, "WhatsApp lets you delete a message for everyone only in the first two days or so.")
		}
	}
	var jid waTypes.JID
	if err == nil {
		jid = c.jid()
	}
	w.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := cli.SendMessage(ctx, jid, cli.BuildRevoke(jid, waTypes.EmptyJID, msg.ID)); err != nil {
		hlog.Info("whatsapp: delete failed", hlog.Kind(err))
		return requestError(err)
	}
	w.mu.Lock()
	if w.stillHere(c) {
		w.deleted(c, msg.ID)
		w.scrubbed()
	}
	w.mu.Unlock()
	return nil
}

func (w *WhatsApp) React(ctx context.Context, ref backend.MessageRef, emoji string) error {
	cli, err := w.connected()
	if err != nil {
		return err
	}
	w.mu.Lock()
	c, msg, err := w.messageOf(ref)
	switch {
	case err != nil:
	case !sendable(c.jid()):
		err = errCantSend
	case msg.Service != nil:
		err = proto.Err(proto.BadRequest, "Events can't be reacted to.")
	}
	var jid, sender waTypes.JID
	if err == nil {
		jid = c.jid()
		sender = w.jidOf(msg.Sender)
		if w.isSelf(msg.Sender) {
			sender = cli.OwnID()
		}
	}
	w.mu.Unlock()
	if err != nil {
		return err
	}
	// WhatsApp's apps send reactions with the variation selector where the
	// emoji has one; tuimeta's picker may not, which they'd show as another
	// emoji.
	emoji = variationselector.FullyQualify(emoji)
	if _, err := cli.SendMessage(ctx, jid, cli.BuildReaction(jid, sender, msg.ID, emoji)); err != nil {
		hlog.Info("whatsapp: reaction failed", hlog.Kind(err))
		return requestError(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stillHere(c) {
		return nil
	}
	if cur := c.msgs[msg.ID]; cur != nil && cur.setReaction(w.resolve(w.self), emoji) {
		w.changed(c, cur)
	}
	return nil
}

// MarkRead is the only place read receipts go out: for others' messages up
// to msg that haven't had one, a receipt per sender (in a group) or one for
// the chat.
func (w *WhatsApp) MarkRead(ctx context.Context, ref backend.MessageRef) error {
	w.mu.Lock()
	c, msg, err := w.messageOf(ref)
	if err != nil {
		w.mu.Unlock()
		return err
	}
	upTo := msg.MS
	jid := c.jid()
	group := c.kind == proto.Group
	bySender := map[waTypes.JID][]waTypes.MessageID{}
	// A chat tuimeta doesn't send to is read here, and nothing goes to it.
	unread := c.msgs
	if !sendable(jid) {
		unread = nil
	}
	for _, m := range unread {
		if m.MS > upTo || m.MS <= c.ReadUpTo || w.isSelf(m.Sender) || m.Service != nil || strings.HasPrefix(m.ID, "event:") {
			continue
		}
		sender := waTypes.EmptyJID
		if group {
			sender = w.jidOf(m.Sender)
		}
		bySender[sender] = append(bySender[sender], m.ID)
	}
	w.mu.Unlock()
	if len(bySender) > 0 {
		cli, err := w.connected()
		if err != nil {
			return err
		}
		for sender, list := range bySender {
			slices.Sort(list)
			if err := cli.MarkRead(ctx, "mark_read", list, w.now(), jid, sender); err != nil {
				hlog.Info("whatsapp: read receipt failed", hlog.Kind(err))
				return requestError(err)
			}
		}
	}
	w.mu.Lock()
	if cur := w.chats[c.key]; cur != nil {
		w.readUpTo(cur, upTo)
	}
	w.mu.Unlock()
	return nil
}

// SetTyping is the only place typing goes out.
func (w *WhatsApp) SetTyping(ctx context.Context, ref backend.ChatRef, typing bool) error {
	cli, err := w.connected()
	if err != nil {
		return err
	}
	w.mu.Lock()
	c, err := w.chatOf(ref.ID)
	var jid waTypes.JID
	if err == nil {
		jid = c.jid()
		if !sendable(jid) {
			err = errCantSend
		}
	}
	w.mu.Unlock()
	if err != nil {
		return err
	}
	state := waTypes.ChatPresencePaused
	if typing {
		state = waTypes.ChatPresenceComposing
	}
	return requestError(cli.SendChatPresence(ctx, "typing", jid, state))
}

// Mute mutes the chat for good, or unmutes it, on every device of the
// account.
func (w *WhatsApp) Mute(ctx context.Context, ref backend.ChatRef, muted bool) error {
	cli, err := w.connected()
	if err != nil {
		return err
	}
	w.mu.Lock()
	c, err := w.chatOf(ref.ID)
	var jid waTypes.JID
	if err == nil {
		jid = c.jid()
		if !sendable(jid) {
			err = errCantSend
		}
	}
	w.mu.Unlock()
	if err != nil {
		return err
	}
	if err := cli.Mute(ctx, jid, muted); err != nil {
		hlog.Info("whatsapp: mute failed", hlog.Kind(err))
		return requestError(err)
	}
	until := int64(0)
	if muted {
		until = -1
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if cur := w.chats[c.key]; cur != nil && cur.MuteUntil != until {
		cur.MuteUntil = until
		w.saveChat(cur)
		w.touch(cur)
	}
	return nil
}

// Search looks through the contacts and groups this device knows, and, for
// a query that's a phone number, asks WhatsApp whether it's on WhatsApp.
func (w *WhatsApp) Search(ctx context.Context, query string) ([]proto.SearchResult, error) {
	cli, err := w.connected()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(oneLine(query))
	digits, isNumber := backend.PhoneDigits(query)
	// Asking WhatsApp about a number is for a number written out with its
	// country code, once a run: not for every pause in typing one.
	lookup := isNumber && strings.HasPrefix(strings.TrimSpace(query), "+")
	cctx, cancel := context.WithTimeout(ctx, dbWait)
	contacts, err := cli.Contacts(cctx)
	cancel()
	if err != nil {
		hlog.Info("whatsapp: contacts unreadable", hlog.Kind(err))
	}

	w.mu.Lock()
	results := []proto.SearchResult{}
	seen := map[string]bool{}
	add := func(key string) {
		key = w.resolve(key)
		if j, err := waTypes.ParseJID(key); err != nil || !personJID(j) {
			return
		}
		if seen[key] || w.isSelf(key) || len(results) >= MaxSearchResults {
			return
		}
		seen[key] = true
		p := w.person(key)
		w.tellUser(p, false)
		res := proto.SearchResult{UserID: p.id, Title: p.name, Kind: proto.DM}
		if c := w.chats[key]; c != nil && w.visible(c) {
			res.ChatID = c.id
		}
		results = append(results, res)
	}
	matches := func(name, number string) bool {
		return (q != "" && strings.Contains(strings.ToLower(name), q)) ||
			(isNumber && strings.Contains(number, digits)) || (len(q) >= 3 && number != "" && strings.Contains(number, strings.TrimPrefix(q, "+")))
	}
	for _, c := range w.chats {
		if c.kind == proto.Group && w.visible(c) && strings.Contains(strings.ToLower(c.Name), q) && len(results) < MaxSearchResults {
			if !w.sent[c.id] {
				w.sent[c.id] = true
				w.sendChat(c)
			}
			results = append(results, proto.SearchResult{ChatID: c.id, Title: w.title(c), Kind: proto.Group})
		}
	}
	for _, p := range w.people {
		if matches(p.name, p.phone) {
			add(p.key)
		}
	}
	for jid, info := range contacts {
		name := firstNonEmpty(info.FullName, info.FirstName, info.PushName, info.BusinessName)
		number := ""
		if jid.Server == waTypes.DefaultUserServer {
			number = jid.User
		}
		if matches(name, number) {
			add(w.canon(jid))
		}
	}
	w.mu.Unlock()

	w.mu.Lock()
	_, looked := w.looked[digits]
	w.searches++
	mine := w.searches
	w.mu.Unlock()
	if lookup && !looked && len(results) < MaxSearchResults && w.mayLookUp(ctx, mine) {
		// Whether a number is on WhatsApp is only known by asking.
		found, err := cli.IsOnWhatsApp(ctx, []string{"+" + digits})
		if err != nil {
			hlog.Info("whatsapp: number lookup failed", hlog.Kind(err))
		}
		w.mu.Lock()
		if err == nil {
			w.looked[digits] = waTypes.EmptyJID
		}
		for _, f := range found {
			if f.IsIn && !f.JID.IsEmpty() {
				w.looked[digits] = f.JID
			}
		}
		w.mu.Unlock()
	}
	w.mu.Lock()
	if j := w.looked[digits]; lookup && personJID(j) {
		add(w.canon(j))
	}
	w.mu.Unlock()
	return results, nil
}

// Asking WhatsApp whether numbers are on it: what it counts as looking
// people up, so it's done sparingly.
const (
	// LookupSettle is how long a number has to stay as typed before it's
	// asked about: a number being typed isn't asked about digit by digit.
	LookupSettle = 1500 * time.Millisecond
	// MaxLookups is how many numbers may be asked about in LookupWindow.
	MaxLookups   = 10
	LookupWindow = 10 * time.Minute
)

// mayLookUp waits for the number to settle and reports whether it may be
// asked about now: no newer search came meanwhile, and fewer than
// MaxLookups were asked in the last LookupWindow.
func (w *WhatsApp) mayLookUp(ctx context.Context, search int) bool {
	select {
	case <-time.After(lookupSettle):
	case <-ctx.Done():
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.searches != search {
		return false
	}
	now := w.now()
	w.lookups = slices.DeleteFunc(w.lookups, func(t time.Time) bool { return now.Sub(t) > LookupWindow })
	if len(w.lookups) >= MaxLookups {
		return false
	}
	w.lookups = append(w.lookups, now)
	return true
}

// lookupSettle is LookupSettle (tests shorten it).
var lookupSettle = LookupSettle

// OpenDM is the chat with a person, made here if there's none: WhatsApp
// makes it with the first message.
func (w *WhatsApp) OpenDM(ctx context.Context, ref backend.UserRef) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if j, err := waTypes.ParseJID(ref.NetID); err != nil || !personJID(j) {
		return 0, proto.ErrNoUser
	}
	c := w.chatFor(ref.NetID, false)
	if !c.known {
		c.known = true
		c.Activity = max(c.Activity, w.now().UnixMilli())
		w.saveChat(c)
	}
	w.sent[c.id] = true
	w.sendChat(c)
	return c.id, nil
}

func (w *WhatsApp) GetMessage(ctx context.Context, ref backend.MessageRef) (proto.Message, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, err := w.chatOf(ref.Chat.ID)
	if err != nil {
		return proto.Message{}, err
	}
	w.ensureLoaded(c)
	msg, ok := c.log.Get(ref.ID)
	if !ok {
		return proto.Message{}, proto.ErrNoMessage
	}
	return msg, nil
}
