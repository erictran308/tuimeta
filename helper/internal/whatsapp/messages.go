// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	waTypes "go.mau.fi/whatsmeow/types"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/metatext"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// keep puts msg in c (replacing the message with its id) and returns it as
// tuimeta sees it; w.mu is held. It doesn't store it.
func (w *WhatsApp) keep(c *chat, msg *message) proto.Message {
	msg.ids = w.d.Messages.Assign(c.id, msg.ID, msg.MS, 1)
	c.msgs[msg.ID] = msg
	part := w.render(c, msg)
	c.log.Put(part)
	return part
}

// store writes msgs of c to the database; w.mu is held.
func (w *WhatsApp) store(c *chat, msgs ...*message) {
	if w.st == nil || len(msgs) == 0 {
		return
	}
	ctx, cancel := dbCtx()
	defer cancel()
	trimmed, err := w.st.putMessages(ctx, c.key, msgs...)
	if err != nil {
		w.storeFailed = true
		hlog.Error("whatsapp: can't keep messages", hlog.Kind(err))
		return
	}
	if len(trimmed) == 0 {
		return
	}
	// The oldest, past what's kept of a chat: what was downloaded of them
	// goes, but not while they're still shown this run; then it goes when
	// the helper stops.
	w.forgetFiles(trimmed...)
	for _, t := range trimmed {
		if m := c.msgs[t.ID]; m != nil && len(fileRefs(m)) > 0 && len(w.trimmed) < MaxTrimmedShown {
			w.trimmed = append(w.trimmed, trim{chat: c.key, msg: m})
		}
	}
}

// trim is a message the store's bound took while it was shown.
type trim struct {
	chat string
	msg  *message
}

// MaxTrimmedShown bounds the messages a run remembers deleting the files of
// when it stops.
const MaxTrimmedShown = 10000

// forgetTrimmed deletes what was downloaded of the messages the store's
// bound took while they were shown, unless they were kept again since; w.mu
// is held.
func (w *WhatsApp) forgetTrimmed() {
	if w.st == nil || len(w.trimmed) == 0 {
		return
	}
	var gone []*message
	for _, t := range w.trimmed {
		ctx, cancel := dbCtx()
		kept, err := w.st.message(ctx, t.chat, t.msg.ID)
		cancel()
		if err == nil && kept == nil {
			gone = append(gone, t.msg)
		}
	}
	w.trimmed = nil
	w.forgetFiles(gone...)
}

// render is msg as tuimeta sees it; w.mu is held.
func (w *WhatsApp) render(c *chat, msg *message) proto.Message {
	if len(msg.ids) == 0 {
		msg.ids = w.d.Messages.Assign(c.id, msg.ID, msg.MS, 1)
	}
	own := w.isSelf(msg.Sender)
	text, ents := w.textOf(msg)
	out := proto.Message{
		ID:          msg.ids[0],
		ChatID:      c.id,
		SenderID:    w.person(msg.Sender).id,
		Outgoing:    own,
		Date:        msg.MS / 1000,
		Text:        text,
		Entities:    ents,
		Forwarded:   msg.Forwarded,
		Reactions:   tally(msg.Reactions, w.isSelf),
		Edited:      msg.Edited,
		Deletable:   own && msg.Service == nil,
		State:       proto.Sent,
		Unsupported: msg.Unsupported,
	}
	if msg.Service != nil {
		out.Service = w.sentence(msg.Service)
		out.Text, out.Entities, out.Deletable = "", nil, false
	}
	if own && msg.Media == nil && msg.Service == nil && msg.Unsupported == "" && msg.Text != "" {
		out.EditableUntil = msg.MS/1000 + EditWindow
	}
	if r := msg.Reply; r != nil {
		// Who said what's quoted, and what it said, are only the reply's
		// sender's word: they're taken as said only from a kept message.
		rt := &proto.ReplyTo{Text: r.Text}
		replied := c.msgs[r.ID]
		switch {
		case replied != nil && len(replied.ids) > 0 && r.Sender != "" && w.same(r.Sender, replied.Sender):
			// The message it answers is kept here: the one with its id and
			// its sender (a WhatsApp message is both, and another sender
			// may use the same id). It's quoted as it is here, never with
			// words only the reply gave it: an event by its sentence.
			rt.MessageID = replied.ids[0]
			rt.SenderID = w.person(replied.Sender).id
			rt.Text = quoteFor(replied)
			if rt.Text == "" && replied.Service != nil {
				rt.Text = proto.Snippet(w.sentence(replied.Service), 100)
			}
		case replied == nil:
			// Not a message kept here: the quote is shown, but isn't put
			// in anyone's mouth.
			if known, ok := w.d.Messages.Known(c.id, r.ID); ok && len(known) > 0 {
				rt.MessageID = known[0]
			}
		}
		// A kept message by someone else than the reply names isn't what it
		// answers: the quote is shown as the reply gave it, linked to
		// nothing and nobody's.
		out.ReplyTo = rt
	}
	if msg.Media != nil {
		out.Media = w.mediaOf(msg.Media)
	}
	if msg.Preview != nil {
		out.LinkPreview = w.previewOf(msg.Preview)
	}
	return out
}

// quoteFor is how a reply shows msg.
func quoteFor(msg *message) string {
	switch {
	case msg.Text != "":
		return snippet(msg.Text)
	case msg.Media != nil:
		return label(msg.Media.Kind)
	case msg.Unsupported != "":
		return msg.Unsupported
	}
	return ""
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

// textOf is msg's text and entities: WhatsApp's markers read as formatting,
// and each "@<number>" it mentions written as "@" and the person's name, as
// WhatsApp's apps show them; w.mu is held.
func (w *WhatsApp) textOf(msg *message) (string, []proto.Entity) {
	if msg.Text == "" {
		return "", nil
	}
	text := msg.Text
	type spot struct {
		at, end int
		key     string
	}
	var spots []spot
	for _, j := range w.mentioned(msg) {
		needle := "@" + j.User
		if i := strings.Index(text, needle); i >= 0 {
			spots = append(spots, spot{i, i + len(needle), w.keyOf(j)})
		}
	}
	slices.SortFunc(spots, func(a, b spot) int { return a.at - b.at })
	var b strings.Builder
	var mentions []metatext.Mention
	prev := 0
	for _, s := range spots {
		if s.at < prev {
			continue
		}
		b.WriteString(text[prev:s.at])
		start := b.Len()
		p := w.person(s.key)
		b.WriteString("@" + p.name)
		built := b.String()
		off, n := proto.UTF16Range(built, start, len(built))
		mentions = append(mentions, metatext.Mention{Offset: off, Length: n, UserID: p.id})
		prev = s.end
	}
	b.WriteString(text[prev:])
	in := b.String()
	sum := sha256.Sum256([]byte(in))
	if p := msg.parsed; p == nil || p.sum != sum || !slices.Equal(p.mentions, mentions) {
		plain, ents := metatext.ParseWithMentions(in, mentions)
		msg.parsed = &parse{sum: sum, mentions: mentions, text: plain, ents: ents}
	}
	// Each render has its own entities: what's sent of one isn't another's.
	return msg.parsed.text, slices.Clone(msg.parsed.ents)
}

// sentence is a service event in words, with the names known now; w.mu is
// held.
func (w *WhatsApp) sentence(s *service) string {
	who := func(key string) string {
		if key == "" {
			return "Someone"
		}
		if w.isSelf(key) {
			return "You"
		}
		return w.person(key).name
	}
	actor := who(s.Actor)
	var names []string
	for _, t := range s.Targets {
		if w.isSelf(t) {
			names = append(names, "you")
		} else {
			names = append(names, w.person(t).name)
		}
	}
	targets := joinNames(names)
	selfTarget := len(s.Targets) == 1 && s.Actor != "" && w.same(s.Targets[0], s.Actor)
	switch s.Kind {
	case "create":
		if s.Text != "" {
			return actor + " created the group " + s.Text
		}
		return actor + " created the group"
	case "rename":
		return actor + " named the group " + s.Text
	case "photo":
		return actor + " changed the group's photo"
	case "description":
		return actor + " changed the group's description"
	case "add":
		if s.Actor == "" || selfTarget {
			return capitalize(targets) + " joined"
		}
		return actor + " added " + targets
	case "join":
		return capitalize(targets) + " joined"
	case "remove":
		if selfTarget {
			return actor + " left"
		}
		return actor + " removed " + targets
	case "leave":
		return capitalize(targets) + " left"
	case "missed_voice":
		return "Missed voice call"
	case "missed_video":
		return "Missed video call"
	case "timer":
		secs, _ := strconv.ParseUint(s.Text, 10, 32)
		if secs == 0 {
			return actor + " turned off disappearing messages"
		}
		return actor + " turned on disappearing messages: new messages disappear after " + duration(secs)
	}
	return "Something changed in the chat"
}

func capitalize(s string) string {
	if strings.HasPrefix(s, "you") {
		return "Y" + s[1:]
	}
	return s
}

// joinNames writes names as a list: "A", "A and B", "A, B and C".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return "someone"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// duration writes a disappearing-messages timer.
func duration(secs uint64) string {
	switch {
	case secs%86400 == 0:
		n := secs / 86400
		if n == 1 {
			return "24 hours"
		}
		return strconv.FormatUint(n, 10) + " days"
	case secs%3600 == 0:
		n := secs / 3600
		if n == 1 {
			return "1 hour"
		}
		return strconv.FormatUint(n, 10) + " hours"
	}
	return strconv.FormatUint(secs/60, 10) + " minutes"
}

// mediaRefs are an attachment's files: the file itself (nil when there's
// nothing to download) and a thumbnail the message carried (nil if none).
//
// The file is named by its hash and its media key together: the hash alone
// is the sender's to state, so another sender stating someone else's
// file's hash could otherwise take over its entry (its name, where it's
// fetched from), while the media key is the secret only the file's own
// messages hold. A thumbnail is named by its own bytes.
func mediaRefs(md *media) (full, thumbnail *ids.FileRef) {
	return fullRef(md), thumbRef(md)
}

// fullRef is an attachment's file, nil when there's nothing to download.
func fullRef(md *media) *ids.FileRef {
	// Both hashes, or whatsmeow can't check what it downloads (and refuses).
	// The hash and the key are each 32 bytes as WhatsApp makes them, and
	// only then is the name made of them one way only.
	if md.ViewOnce || len(md.FileSHA256) != 32 || len(md.MediaKey) != 32 || len(md.FileEncSHA256) == 0 || md.DirectPath == "" {
		return nil
	}
	h := sha256.New()
	h.Write(md.FileSHA256)
	h.Write(md.MediaKey)
	return &ids.FileRef{
		Network: net, Key: "wa:" + hex.EncodeToString(h.Sum(nil)), Size: md.Size, Mime: md.Mime, Name: md.Name, Source: md,
	}
}

// thumbRef is the thumbnail an attachment carried, nil if none.
func thumbRef(md *media) *ids.FileRef {
	if md.ViewOnce || len(md.Thumb) == 0 {
		return nil
	}
	return &ids.FileRef{
		Network: net, Key: "wa:thumb:" + contentKey(md.Thumb), Size: int64(len(md.Thumb)), Mime: "image/jpeg", Name: "thumbnail.jpg",
		Source: inlineSource(md.Thumb),
	}
}

// previewRef is a link card's picture, carried in the message.
func previewRef(p *preview) *ids.FileRef {
	if len(p.Thumb) == 0 {
		return nil
	}
	return &ids.FileRef{
		Network: net, Key: "wa:preview:" + contentKey(p.Thumb), Size: int64(len(p.Thumb)),
		Mime: "image/jpeg", Name: "preview.jpg", Source: inlineSource(p.Thumb),
	}
}

// mediaOf registers an attachment's files, downloaded and decrypted only
// on request; w.mu is held. A photo or a sticker is drawn from the file
// itself (downloaded when it's on screen, within previewLimit).
func (w *WhatsApp) mediaOf(md *media) *proto.Media {
	out := &proto.Media{
		Kind: md.Kind, Mime: md.Mime, Size: md.Size, Width: md.Width, Height: md.Height,
		Duration: md.Seconds, Name: md.Name, ViewOnce: md.ViewOnce,
	}
	full, thumbnail := mediaRefs(md)
	if full != nil {
		out.FileID = w.d.Files.Register(*full)
	}
	image := md.Kind == proto.Photo || md.Kind == proto.Sticker || (md.Kind == proto.GIF && strings.HasPrefix(md.Mime, "image/"))
	switch {
	case image && out.FileID != 0:
		out.Thumbnail = &proto.Image{FileID: out.FileID, Width: md.Width, Height: md.Height}
	case thumbnail != nil:
		if id := w.d.Files.Register(*thumbnail); id != 0 {
			tw, th := md.ThumbWidth, md.ThumbHeight
			if tw == 0 || th == 0 {
				tw, th = md.Width, md.Height
			}
			out.Thumbnail = &proto.Image{FileID: id, Width: tw, Height: th}
		}
	}
	return out
}

// previewOf is a link's card, its picture served from the message itself;
// w.mu is held.
func (w *WhatsApp) previewOf(p *preview) *proto.LinkPreview {
	out := &proto.LinkPreview{URL: p.URL, Title: p.Title, Description: p.Description}
	if ref := previewRef(p); ref != nil {
		if id := w.d.Files.Register(*ref); id != 0 {
			out.Image = &proto.Image{FileID: id}
		}
	}
	return out
}

// fileRefs are msg's files: its attachment, its thumbnail and its link's
// picture.
func fileRefs(msg *message) []ids.FileRef {
	var out []ids.FileRef
	var refs []*ids.FileRef
	if msg.Media != nil {
		refs = append(refs, fullRef(msg.Media), thumbRef(msg.Media))
	}
	if msg.Preview != nil {
		refs = append(refs, previewRef(msg.Preview))
	}
	for _, r := range refs {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

// forgetFiles deletes what was downloaded of msgs (their files, thumbnails
// and links' pictures) once they're gone, and ends what's being downloaded
// of them; w.mu is held. A file another message here still shows (a
// forward of the same photo, the same thumbnail) stays, and so does its
// file id: only the messages gone are out of their chats.
func (w *WhatsApp) forgetFiles(msgs ...*message) {
	if w.d.Downloads == nil {
		return
	}
	want := map[string]ids.FileRef{}
	for _, m := range msgs {
		for _, r := range fileRefs(m) {
			want[r.Key] = r
		}
	}
	if len(want) == 0 {
		return
	}
	for _, key := range w.stillShown(want, msgs) {
		delete(want, key)
	}
	for _, r := range want {
		w.d.Downloads.Remove(r)
	}
}

// dropFiles deletes what was downloaded of msgs, for when nothing is shown
// yet (the store opening): w.mu needn't be held.
func (w *WhatsApp) dropFiles(msgs ...*message) {
	if w.d.Downloads == nil {
		return
	}
	for _, m := range msgs {
		for _, r := range fileRefs(m) {
			w.d.Downloads.Remove(r)
		}
	}
}

// stillShown is the keys, of those in want, of files a message here (other
// than gone) has; w.mu is held. Only pictures of a size wanted are hashed
// to compare them.
func (w *WhatsApp) stillShown(want map[string]ids.FileRef, gone []*message) []string {
	skip := make(map[*message]bool, len(gone))
	for _, m := range gone {
		skip[m] = true
	}
	sizes := map[int]bool{}
	for _, r := range want {
		if src, ok := r.Source.(inlineSource); ok {
			sizes[len(src)] = true
		}
	}
	var out []string
	has := func(r *ids.FileRef) {
		if r != nil {
			if _, ok := want[r.Key]; ok {
				out = append(out, r.Key)
			}
		}
	}
	look := func(m *message) {
		if m == nil || skip[m] {
			return
		}
		if md := m.Media; md != nil {
			has(fullRef(md))
			if sizes[len(md.Thumb)] {
				has(thumbRef(md))
			}
		}
		if p := m.Preview; p != nil && sizes[len(p.Thumb)] {
			has(previewRef(p))
		}
	}
	for _, c := range w.chats {
		look(c.last)
		for _, m := range c.msgs {
			look(m)
		}
	}
	return out
}

// contentKey names bytes by their hash, for a file the message carries.
func contentKey(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// mentioned is the people msg's text mentions: the mentions whose "@<id>"
// is in the text (a sender can list anyone; only those shown count); w.mu
// is held.
func (w *WhatsApp) mentioned(msg *message) []waTypes.JID {
	var out []waTypes.JID
	for _, raw := range msg.Mentions {
		j, err := waTypes.ParseJID(raw)
		if err == nil && personJID(j) && strings.Contains(msg.Text, "@"+j.User) {
			out = append(out, j)
		}
	}
	return out
}

// tellPeople sends the user events of the people msg names; w.mu is held.
func (w *WhatsApp) tellPeople(msg *message) {
	w.tellUser(w.person(msg.Sender), false)
	for _, j := range w.mentioned(msg) {
		w.tellUser(w.person(w.keyOf(j)), false)
	}
	if msg.Reply != nil && msg.Reply.Sender != "" {
		w.tellUser(w.person(msg.Reply.Sender), false)
	}
	for _, r := range msg.Reactions {
		w.tellUser(w.person(r.Actor), false)
	}
	if s := msg.Service; s != nil {
		if s.Actor != "" {
			w.tellUser(w.person(s.Actor), false)
		}
		for _, t := range s.Targets {
			w.tellUser(w.person(t), false)
		}
	}
}

// arrived records a message that just came (or was sent from another of
// your devices), stores it and reports it; w.mu is held. It's false when
// the message isn't taken.
func (w *WhatsApp) arrived(c *chat, msg *message) bool {
	w.ensureLoaded(c)
	if w.wasDeleted(c.key, msg.ID, msg.Sender) {
		// Deleted here already: another copy of it doesn't bring it back.
		return false
	}
	old, seen := c.msgs[msg.ID]
	if seen && (!w.same(old.Sender, msg.Sender) || !old.Placeholder) {
		// A message is replaced only by its decryption, once that comes:
		// another with the same id (someone else's, or its sender's again,
		// which would rewrite it unmarked) is dropped. Changes go through
		// edits, which say so.
		return false
	}
	if seen && len(msg.Reactions) == 0 {
		// The same message again (decrypted at last, or sent twice): the
		// reactions it had stay.
		msg.Reactions = old.Reactions
	}
	w.settle(c, msg)
	part := w.keep(c, msg)
	w.store(c, msg)
	c.Activity = max(c.Activity, msg.MS)
	if !seen && !w.isSelf(msg.Sender) && msg.Service == nil && msg.MS > c.ReadUpTo {
		c.Unread++
	}
	w.saveChat(c)
	w.tellPeople(msg)
	w.d.Events.Message(part)
	w.touch(c)
	return true
}

// changed reports a known message after an edit or a reaction; w.mu is
// held.
func (w *WhatsApp) changed(c *chat, msg *message) {
	part := w.keep(c, msg)
	w.store(c, msg)
	w.tellPeople(msg)
	w.d.Events.Message(part)
	if last, ok := c.log.Newest(); ok && last.ID == part.ID {
		w.touch(c)
	}
}

// deleted deletes a message (deleted for everyone, deleted on your phone,
// or its time up) here and from tuimeta, with what was downloaded of it, and
// leaves a tombstone so it never comes back; w.mu is held and c loaded. It
// returns the message, nil if it wasn't kept.
func (w *WhatsApp) deleted(c *chat, id string) *message {
	msg := c.msgs[id]
	if msg == nil && w.st != nil {
		// Kept but not read in: one whose time is up is left out when a
		// chat's messages are read back (ensureLoaded).
		ctx, cancel := dbCtx()
		msg, _ = w.st.message(ctx, c.key, id)
		cancel()
	}
	if msg == nil {
		return nil
	}
	gone := tombstone{id: id, sender: w.resolve(msg.Sender), sure: true}
	if w.st != nil {
		ctx, cancel := dbCtx()
		if err := w.st.deleteMessage(ctx, c.key, id, &gone); err != nil {
			w.storeFailed = true
			hlog.Error("whatsapp: can't delete a message", hlog.Kind(err))
		}
		cancel()
		w.scrub = true
	}
	w.noteDeleted(c.key, gone, true)
	delete(c.msgs, id)
	w.forgetFiles(msg)
	shown := msg.ids
	if len(shown) == 0 {
		// Not read in, but maybe shown this run (an unopened chat's newest).
		shown, _ = w.d.Messages.Known(c.id, id)
	}
	if len(shown) > 0 {
		c.log.Remove(shown...)
		w.d.Events.MessageDeleted(c.id, shown)
	}
	if !w.isSelf(msg.Sender) && msg.Service == nil && msg.MS > c.ReadUpTo && c.Unread > 0 {
		c.Unread--
		w.saveChat(c)
	}
	w.touch(c)
	return msg
}

// messageOf is the message a request names; w.mu is held.
func (w *WhatsApp) messageOf(ref backend.MessageRef) (*chat, *message, error) {
	c, err := w.chatOf(ref.Chat.ID)
	if err != nil {
		return nil, nil, err
	}
	w.ensureLoaded(c)
	msg := c.msgs[ref.NetID]
	if msg == nil {
		return nil, nil, proto.ErrNoMessage
	}
	return c, msg, nil
}
