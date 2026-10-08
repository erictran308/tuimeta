// SPDX-License-Identifier: AGPL-3.0-or-later

// Package proto holds the objects tuimeta and the helper exchange, as they
// are written on the wire (see PROTOCOL.md).
package proto

import "encoding/json"

// Version is the protocol version the hello line announces.
const Version = 1

// Network is "messenger" or "instagram".
type Network string

const (
	Messenger Network = "messenger"
	Instagram Network = "instagram"
)

// Networks lists every network, in the order hello names them.
var Networks = []Network{Messenger, Instagram}

// Valid reports whether n is one of Networks.
func (n Network) Valid() bool { return n == Messenger || n == Instagram }

// ChatKind is "dm" or "group".
type ChatKind string

const (
	DM    ChatKind = "dm"
	Group ChatKind = "group"
)

// AccountState is where a network's account stands.
type AccountState string

const (
	LoggedOut  AccountState = "logged_out"
	Connecting AccountState = "connecting"
	Ready      AccountState = "ready"
	Errored    AccountState = "error"
)

// MessageState says whether a message reached the network.
type MessageState string

const (
	Sent    MessageState = "sent"
	Pending MessageState = "pending"
	Failed  MessageState = "failed"
)

// MediaKind is what a Media is.
type MediaKind string

const (
	Photo     MediaKind = "photo"
	Video     MediaKind = "video"
	GIF       MediaKind = "gif"
	Sticker   MediaKind = "sticker"
	Audio     MediaKind = "audio"
	Voice     MediaKind = "voice"
	FileMedia MediaKind = "file"
)

// EntityType is a kind of formatting or link inside a message's text.
type EntityType string

const (
	Bold       EntityType = "bold"
	Italic     EntityType = "italic"
	Strike     EntityType = "strike"
	InlineCode EntityType = "code"
	Pre        EntityType = "pre"
	Quote      EntityType = "quote"
	Link       EntityType = "link"
	Mention    EntityType = "mention"
)

// Priority orders downloads: what's on screen first.
type Priority string

const (
	High Priority = "high"
	Low  Priority = "low"
)

// Image is a picture tuimeta can download and draw.
type Image struct {
	FileID int32 `json:"file_id"`
	Width  int   `json:"width"`
	Height int   `json:"height"`
}

// Chat is one conversation in the chat list.
type Chat struct {
	ID          int64    `json:"id"`
	Network     Network  `json:"network"`
	Kind        ChatKind `json:"kind"`
	Title       string   `json:"title"`
	UserID      int64    `json:"user_id,omitempty"`
	Photo       *Image   `json:"photo,omitempty"`
	Order       int64    `json:"order"`
	Unread      int      `json:"unread"`
	Muted       bool     `json:"muted"`
	Archived    bool     `json:"archived"`
	Encrypted   bool     `json:"encrypted"`
	Request     bool     `json:"request"`
	CanSend     bool     `json:"can_send"`
	ReadInbox   int64    `json:"read_inbox"`
	ReadOutbox  int64    `json:"read_outbox"`
	LastMessage *Message `json:"last_message,omitempty"`
}

// User is a person, yourself included.
type User struct {
	ID       int64   `json:"id"`
	Network  Network `json:"network"`
	Name     string  `json:"name"`
	Username string  `json:"username,omitempty"`
	Photo    *Image  `json:"photo,omitempty"`
	IsSelf   bool    `json:"is_self"`
	ActiveAt int64   `json:"active_at,omitempty"`
}

// Entity marks a stretch of a message's text, in UTF-16 code units.
type Entity struct {
	Offset int        `json:"offset"`
	Length int        `json:"length"`
	Type   EntityType `json:"type"`
	URL    string     `json:"url,omitempty"`
	UserID int64      `json:"user_id,omitempty"`
}

// ReplyTo is the message a message answers.
type ReplyTo struct {
	MessageID int64  `json:"message_id,omitempty"`
	SenderID  int64  `json:"sender_id,omitempty"`
	Text      string `json:"text,omitempty"`
}

// Reaction is one emoji and how many people chose it.
type Reaction struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
	Mine  bool   `json:"mine"`
}

// Media is the attachment of a message part.
type Media struct {
	Kind      MediaKind `json:"kind"`
	FileID    int32     `json:"file_id"`
	Thumbnail *Image    `json:"thumbnail,omitempty"`
	Width     int       `json:"width,omitempty"`
	Height    int       `json:"height,omitempty"`
	Duration  int       `json:"duration,omitempty"`
	Name      string    `json:"name,omitempty"`
	Mime      string    `json:"mime,omitempty"`
	Size      int64     `json:"size,omitempty"`
	ViewOnce  bool      `json:"view_once"`
}

// LinkPreview is the card a network attached to a link. It only ever comes
// from the network's own data: the helper never fetches a message's URL.
type LinkPreview struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Image       *Image `json:"image,omitempty"`
}

// Message is one message, or one part of an album.
type Message struct {
	ID            int64        `json:"id"`
	ChatID        int64        `json:"chat_id"`
	SenderID      int64        `json:"sender_id"`
	Outgoing      bool         `json:"outgoing"`
	Date          int64        `json:"date"`
	Text          string       `json:"text"`
	Entities      []Entity     `json:"entities"`
	Album         int64        `json:"album"`
	Media         *Media       `json:"media,omitempty"`
	ReplyTo       *ReplyTo     `json:"reply_to,omitempty"`
	Forwarded     bool         `json:"forwarded"`
	Reactions     []Reaction   `json:"reactions"`
	Edited        bool         `json:"edited"`
	EditableUntil int64        `json:"editable_until,omitempty"`
	Deletable     bool         `json:"deletable"`
	State         MessageState `json:"state"`
	Service       string       `json:"service,omitempty"`
	LinkPreview   *LinkPreview `json:"link_preview,omitempty"`
	Unsupported   string       `json:"unsupported,omitempty"`
}

// MarshalJSON writes empty lists as [] rather than null, and a missing state
// as "sent", since neither field is optional on the wire.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if m.Entities == nil {
		m.Entities = []Entity{}
	}
	if m.Reactions == nil {
		m.Reactions = []Reaction{}
	}
	if m.State == "" {
		m.State = Sent
	}
	return json.Marshal(plain(m))
}

// File is a download's progress.
type File struct {
	ID         int32   `json:"id"`
	Size       int64   `json:"size"`
	Downloaded int64   `json:"downloaded"`
	Done       bool    `json:"done"`
	Path       *string `json:"path"`
	Error      string  `json:"error,omitempty"`
}

// SearchResult is a person or group a search found.
type SearchResult struct {
	ChatID   int64    `json:"chat_id,omitempty"`
	UserID   int64    `json:"user_id,omitempty"`
	Title    string   `json:"title"`
	Username string   `json:"username,omitempty"`
	Kind     ChatKind `json:"kind"`
}
