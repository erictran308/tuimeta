// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	waTypes "go.mau.fi/whatsmeow/types"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/metatext"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// EditWindow is how long (seconds) Messenger lets you edit a text message.
const EditWindow = 15 * 60

// maxParts is the most attachments one message is shown with: ids.Messages
// gives one message at most this many ids, and a message with more media
// than ids is shown with none.
const maxParts = 64

// chat is a conversation as the backend keeps it. key is the network's id
// for it: a Facebook thread key, or for an encrypted chat the user part of
// its WhatsApp-style JID. A one-to-one chat's key is the other person's id
// either way, so a chat that moved to end-to-end encryption stays one chat;
// an encrypted group's Facebook thread key is mapped to its JID's (fbKey).
type chat struct {
	key   int64
	id    int64
	fbKey int64 // the Facebook thread key, when it differs from key
	kind  proto.ChatKind
	ttype table.ThreadType

	name      string // a group's own name
	picture   string // a group's picture URL
	other     int64  // a dm's other person
	members   []int64
	nicknames map[int64]string
	server    string // the encrypted chat's JID server, when encrypted
	folder    string
	encrypted bool
	archived  bool
	request   bool
	cantReply bool
	// receiptsOff is set once Messenger's thread tables said your read
	// receipts are off in this chat (either of their two flags).
	receiptsOff bool
	muteUntil   int64 // ms; -1 for good
	activity    int64 // ms of the last activity
	readUpTo    int64 // ms: you've read everything up to here
	// receipted is how far read receipts for the encrypted messages went
	// out (from here or another device). Your own messages move readUpTo
	// but not this: replying isn't a receipt for what you didn't open.
	receipted  int64
	theirRead  map[int64]int64
	serverSaid int   // the unread count the server last gave, -1 if none
	serverAt   int64 // ms: the activity that count was as of
	unread     int   // others' messages after readUpTo
	known      bool  // the server described it (not just a message seen)
	fbHistory  bool  // Messenger may have older (unencrypted) messages
	// timer is the encrypted chat's disappearing-messages timer in seconds
	// (0: off), as set at timerAt (unix seconds); what you send carries it.
	timer, timerAt int64

	log  *history.Log
	msgs map[string]*message
	// waIDs finds the encrypted messages by their own ids, which are all a
	// receipt names; several senders may have used one id.
	waIDs map[string][]string
}

func newChat(key int64) *chat {
	return &chat{
		key: key, kind: proto.DM, other: key, serverSaid: -1, fbHistory: true,
		log: history.New(), msgs: map[string]*message{}, theirRead: map[int64]int64{}, nicknames: map[int64]string{},
		waIDs: map[string][]string{},
	}
}

// indexWA records an encrypted message under its own id.
func (c *chat) indexWA(msg *message) {
	if id := msg.wa.id; !slices.Contains(c.waIDs[id], msg.netID) {
		c.waIDs[id] = append(c.waIDs[id], msg.netID)
	}
}

// unindexWA forgets an encrypted message gone from c.
func (c *chat) unindexWA(msg *message) {
	id := msg.wa.id
	if rest := slices.DeleteFunc(c.waIDs[id], func(n string) bool { return n == msg.netID }); len(rest) > 0 {
		c.waIDs[id] = rest
	} else {
		delete(c.waIDs, id)
	}
}

// netID is the chat's id as ids.Store keeps it.
func (c *chat) netID() string { return strconv.FormatInt(c.key, 10) }

// threadKey is the Facebook thread key tasks about the chat go to.
func (c *chat) threadKey() int64 {
	if c.fbKey != 0 {
		return c.fbKey
	}
	return c.key
}

// jid is the encrypted chat's JID.
func (c *chat) jid() waTypes.JID {
	server := c.server
	if server == "" {
		server = waTypes.MessengerServer
		if c.kind == proto.Group {
			server = waTypes.GroupServer
		}
	}
	return waTypes.JID{User: c.netID(), Server: server}
}

// person is someone on Messenger, you included.
type person struct {
	fbid     int64
	id       int64
	name     string
	username string
	avatar   string
	activeAt int64 // unix seconds
	known    bool  // their contact details arrived
	told     bool  // a user event went out
	asked    bool  // their details were asked for
}

func (p *person) netID() string { return strconv.FormatInt(p.fbid, 10) }

// message is one network message, however many parts it's shown as.
type message struct {
	netID       string
	ms          int64
	sender      int64
	text        string // with Meta's formatting markers, as sent
	mentions    []mention
	media       []proto.Media
	reply       *replyRef
	forwarded   bool
	edited      bool
	editCount   int64
	editTS      int64
	service     string
	unsupported string
	preview     *proto.LinkPreview
	reactions   []reaction
	canUnsend   bool
	wa          *waRef // set on an encrypted message
	// placeholder marks an encrypted message that couldn't be decrypted
	// yet: the only one another message with its id replaces.
	placeholder bool
	// expires is when a disappearing message goes, in unix ms (0: never).
	// seenTimer is how long (seconds) one that disappears once seen lasts
	// from then, while it hasn't been.
	expires   int64
	seenTimer int64
	ids       []int64
}

// mention is a stretch of the text naming someone, in UTF-16 units.
type mention struct {
	offset, length int
	fbid           int64
}

// MaxMentions is how many mentions one message may have, as on WhatsApp:
// each person named is one to look up, and to keep an id for.
const MaxMentions = 256

// mentionSet collects a message's mentions: each once, at most MaxMentions.
type mentionSet struct {
	list []mention
	seen map[mention]bool
}

// add adds mn unless it's there already; false once the set is full.
func (s *mentionSet) add(mn mention) bool {
	if len(s.list) >= MaxMentions {
		return false
	}
	if s.seen == nil {
		s.seen = map[mention]bool{}
	}
	if !s.seen[mn] {
		s.seen[mn] = true
		s.list = append(s.list, mn)
	}
	return len(s.list) < MaxMentions
}

type replyRef struct {
	netID  string
	sender int64
	text   string
}

type reaction struct {
	actor int64
	emoji string
}

// waRef is where an encrypted message lives, for edits, reactions,
// unsending and receipts.
type waRef struct {
	chat   waTypes.JID
	sender waTypes.JID // without the device
	id     string
}

// waNetID is an encrypted message's id among the chat's messages: the
// sender's id and the message's, since message ids are the sender's to pick.
func waNetID(sender waTypes.JID, id string) string { return "wa:" + sender.User + ":" + id }

// setReaction makes emoji actor's one reaction ("" removes it).
func (m *message) setReaction(actor int64, emoji string) bool {
	i := slices.IndexFunc(m.reactions, func(r reaction) bool { return r.actor == actor })
	switch {
	case i >= 0 && emoji == "":
		m.reactions = slices.Delete(m.reactions, i, i+1)
	case i >= 0 && m.reactions[i].emoji == emoji:
		return false
	case i >= 0:
		// A changed reaction is a new one: it goes last.
		m.reactions = append(slices.Delete(m.reactions, i, i+1), reaction{actor, emoji})
	case emoji == "":
		return false
	default:
		m.reactions = append(m.reactions, reaction{actor, emoji})
	}
	return true
}

// maxReaction is the longest reaction, in bytes: one emoji, however it's
// composed.
const maxReaction = 32

// reactionLike reports whether s can be a reaction, as the WhatsApp backend
// has it: "" (taken back), or a short emoji, not words, numbers or spaces
// made to look like part of the message (a count, a time, ticks). Digits and
// punctuation pass only as emoji: with the keycap or the emoji-style
// selector ("1️⃣", "‼️"), or as one of the four emoji that are punctuation
// characters, since Messenger sends reactions without that selector.
func reactionLike(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > maxReaction || !utf8.ValidString(s) {
		return false
	}
	emojiStyle := strings.ContainsAny(s, "⃣️")
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsSpace(r), unicode.IsControl(r):
			return false
		case r == '‼' || r == '⁉' || r == '〰' || r == '〽':
		case (unicode.IsNumber(r) || unicode.IsPunct(r)) && !emojiStyle:
			return false
		}
	}
	return true
}

// tally counts reactions by emoji in the order each was first chosen.
func tally(rs []reaction, self int64) []proto.Reaction {
	var out []proto.Reaction
	for _, r := range rs {
		i := slices.IndexFunc(out, func(x proto.Reaction) bool { return sameEmoji(x.Emoji, r.emoji) })
		if i < 0 {
			out = append(out, proto.Reaction{Emoji: r.emoji})
			i = len(out) - 1
		}
		out[i].Count++
		out[i].Mine = out[i].Mine || r.actor == self
	}
	return out
}

// sameEmoji compares emoji without the variation selector, which Messenger
// drops from some reactions and not others.
func sameEmoji(a, b string) bool {
	strip := func(s string) string { return strings.ReplaceAll(s, "️", "") }
	return strip(a) == strip(b)
}

// parts is how many protocol messages m is shown as.
func (m *message) parts() int { return max(len(m.media), 1) }

// shapeKey is the key m's ids are kept under: its network id, or, once its
// number of parts changed (a confirmation brought more attachments than
// first seen), the id with the count, so it gets ids that fit.
func shapeKey(netID string, n int, first bool) string {
	if first {
		return netID
	}
	return netID + "#" + strconv.Itoa(n)
}

// baseNetID strips what shapeKey added.
func baseNetID(key string) string {
	base, _, _ := strings.Cut(key, "#")
	return base
}

// byOrder sorts chats newest activity first, ties by key.
func byOrder(a, b *chat) int {
	if o := cmp.Compare(b.activity, a.activity); o != 0 {
		return o
	}
	return cmp.Compare(a.key, b.key)
}

// idsFor is where a message's assigned ids are kept in the chat's id space.
func positionOf(ms int64) int64 { return ids.MessageID(ms, ids.MaxSlot) }

// textOf is the text and entities of m, its markers read as formatting and
// its mentions as mention entities.
func textOf(m *message, userID func(fbid int64) int64) (string, []proto.Entity) {
	if m.text == "" {
		return "", nil
	}
	ms := make([]metatext.Mention, 0, len(m.mentions))
	for _, mn := range m.mentions {
		var uid int64
		if mn.fbid > 0 {
			uid = userID(mn.fbid)
		}
		ms = append(ms, metatext.Mention{Offset: mn.offset, Length: mn.length, UserID: uid})
	}
	return metatext.ParseWithMentions(m.text, ms)
}
