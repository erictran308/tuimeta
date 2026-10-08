// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"cmp"
	"slices"
	"strings"

	waTypes "go.mau.fi/whatsmeow/types"

	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// WhatsApp's limits on changing a message you sent.
const (
	// EditWindow is how long (seconds) WhatsApp lets you edit a message.
	EditWindow = 15 * 60
	// RevokeWindow is how long (seconds) it lets you delete one for
	// everyone: about two and a half days.
	RevokeWindow = 60 * 60 * 60
)

// chat is a conversation as the backend keeps it. key is its JID as a
// string: a group's, or for a one-to-one chat the other person's key (their
// WhatsApp id, or their phone number while that's all that's known).
type chat struct {
	key  string
	id   int64
	kind proto.ChatKind
	chatRow

	known  bool // the phone or the server described it (not just a message seen)
	loaded bool // its kept messages are in log and msgs
	log    *history.Log
	msgs   map[string]*message // by WhatsApp message id
	last   *message            // the newest, while the messages aren't loaded
}

// chatRow is what's kept of a chat between runs.
type chatRow struct {
	Kind         proto.ChatKind `json:"kind"`
	Name         string         `json:"name,omitempty"`    // a group's subject
	Other        string         `json:"other,omitempty"`   // a dm's person key
	Members      []string       `json:"members,omitempty"` // a group's people keys
	Admins       []string       `json:"admins,omitempty"`  // a group's admins, as WhatsApp last said
	Archived     bool           `json:"archived,omitempty"`
	Pinned       bool           `json:"pinned,omitempty"`
	MuteUntil    int64          `json:"mute_until,omitempty"` // unix ms; -1 for good
	Activity     int64          `json:"activity,omitempty"`   // ms of the last activity
	ReadUpTo     int64          `json:"read_up_to,omitempty"` // ms: you've read everything up to here
	TheirRead    int64          `json:"their_read,omitempty"` // ms: the other side read yours up to here
	Unread       int            `json:"unread,omitempty"`
	MarkedUnread bool           `json:"marked_unread,omitempty"`
	ReadOnly     bool           `json:"read_only,omitempty"` // you can't send (announcements only, or you left)
	Complete     bool           `json:"complete,omitempty"`  // the phone has nothing older
	Ephemeral    uint32         `json:"ephemeral,omitempty"` // the disappearing-messages timer, seconds
	PictureID    string         `json:"picture,omitempty"`
	Synced       bool           `json:"synced,omitempty"` // the phone's state of it at linking was taken in
}

func newChat(key string, kind proto.ChatKind) *chat {
	c := &chat{key: key, kind: kind, log: history.New(), msgs: map[string]*message{}}
	c.Kind = kind
	return c
}

// jid is the chat's JID.
func (c *chat) jid() waTypes.JID {
	j, _ := waTypes.ParseJID(c.key)
	return j
}

// row is what's kept of c.
func (c *chat) row() chatRow {
	r := c.chatRow
	r.Kind = c.kind
	return r
}

// person is someone on WhatsApp, you included. key is their JID as a
// string: their WhatsApp id (…@lid) once known, else their phone number's
// (…@s.whatsapp.net).
type person struct {
	key       string
	id        int64
	phone     string // digits, when known
	name      string // what they're called here (see refreshName)
	contact   bool   // the name is from your address book
	pictureID string
	told      bool // a user event went out with what's known now
}

// message is one WhatsApp message as kept (JSON in the store): what was
// said, already read out of WhatsApp's proto, and what's needed to fetch its
// file.
type message struct {
	ID          string     `json:"id"`
	Sender      string     `json:"sender"` // a person key
	MS          int64      `json:"ms"`
	Text        string     `json:"text,omitempty"`     // with WhatsApp's markers, and mentions as @<number>
	Mentions    []string   `json:"mentions,omitempty"` // the JIDs the text mentions
	Media       *media     `json:"media,omitempty"`
	Reply       *reply     `json:"reply,omitempty"`
	Forwarded   bool       `json:"forwarded,omitempty"`
	Edited      bool       `json:"edited,omitempty"`
	EditMS      int64      `json:"edit_ms,omitempty"`
	Service     *service   `json:"service,omitempty"`
	Unsupported string     `json:"unsupported,omitempty"`
	Preview     *preview   `json:"preview,omitempty"`
	Reactions   []reaction `json:"reactions,omitempty"`
	// Expires is when a disappearing message goes, in unix ms (0: never).
	Expires int64 `json:"expires,omitempty"`
	// Placeholder marks a message that couldn't be decrypted yet: the only
	// kind another delivery of the same id may replace.
	Placeholder bool `json:"placeholder,omitempty"`

	ids []int64 // its protocol ids this run
}

// media is a message's attachment, with what whatsmeow needs to download
// and decrypt it. A view-once photo or video keeps none of that.
type media struct {
	Kind     proto.MediaKind `json:"kind"`
	Mime     string          `json:"mime,omitempty"`
	Name     string          `json:"name,omitempty"`
	Size     int64           `json:"size,omitempty"`
	Width    int             `json:"w,omitempty"`
	Height   int             `json:"h,omitempty"`
	Seconds  int             `json:"secs,omitempty"`
	ViewOnce bool            `json:"view_once,omitempty"`

	DirectPath    string `json:"path,omitempty"`
	MediaKey      []byte `json:"key,omitempty"`
	FileSHA256    []byte `json:"sha256,omitempty"`
	FileEncSHA256 []byte `json:"enc_sha256,omitempty"`
	MediaType     string `json:"type,omitempty"` // whatsmeow's MediaType ("WhatsApp Image Keys"…)

	Thumb       []byte `json:"thumb,omitempty"` // a small JPEG the sender attached
	ThumbWidth  int    `json:"thumb_w,omitempty"`
	ThumbHeight int    `json:"thumb_h,omitempty"`
}

// reply is the message a message answers.
type reply struct {
	ID     string `json:"id"`
	Sender string `json:"sender,omitempty"` // a person key
	Text   string `json:"text,omitempty"`   // a snippet of it, from the quote
}

// preview is a link's card, as the sender's app made it.
type preview struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Thumb       []byte `json:"thumb,omitempty"`
}

// reaction is one person's one reaction.
type reaction struct {
	Actor string `json:"actor"`
	Emoji string `json:"emoji"`
}

// service is an event drawn in the middle of the chat. The sentence is made
// when it's shown, from the names known then.
type service struct {
	Kind    string   `json:"kind"`
	Actor   string   `json:"actor,omitempty"`   // a person key
	Targets []string `json:"targets,omitempty"` // person keys
	Text    string   `json:"text,omitempty"`    // a group name, a timer…
}

// setReaction makes emoji actor's one reaction ("" removes it).
func (m *message) setReaction(actor, emoji string) bool {
	i := slices.IndexFunc(m.Reactions, func(r reaction) bool { return r.Actor == actor })
	switch {
	case i >= 0 && emoji == "":
		m.Reactions = slices.Delete(m.Reactions, i, i+1)
	case i >= 0 && sameEmoji(m.Reactions[i].Emoji, emoji):
		return false
	case i >= 0:
		// A changed reaction is a new one: it goes last.
		m.Reactions = append(slices.Delete(m.Reactions, i, i+1), reaction{actor, emoji})
	case emoji == "":
		return false
	default:
		m.Reactions = append(m.Reactions, reaction{actor, emoji})
	}
	return true
}

// sameEmoji compares emoji without the variation selector, which some apps
// drop and others keep.
func sameEmoji(a, b string) bool {
	strip := func(s string) string { return strings.ReplaceAll(s, "️", "") }
	return strip(a) == strip(b)
}

// tally counts reactions by emoji in the order each was first chosen.
func tally(rs []reaction, self func(string) bool) []proto.Reaction {
	var out []proto.Reaction
	for _, r := range rs {
		i := slices.IndexFunc(out, func(x proto.Reaction) bool { return sameEmoji(x.Emoji, r.Emoji) })
		if i < 0 {
			out = append(out, proto.Reaction{Emoji: r.Emoji})
			i = len(out) - 1
		}
		out[i].Count++
		out[i].Mine = out[i].Mine || self(r.Actor)
	}
	return out
}

// byOrder sorts chats newest activity first, ties by key.
func byOrder(a, b *chat) int {
	if o := cmp.Compare(b.Activity, a.Activity); o != 0 {
		return o
	}
	return cmp.Compare(a.key, b.key)
}
