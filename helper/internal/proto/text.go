// SPDX-License-Identifier: AGPL-3.0-or-later

package proto

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// UTF16Len is how many UTF-16 code units s takes, the unit entities count in.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16Units(r)
	}
	return n
}

func utf16Units(r rune) int {
	if r >= 0x10000 && r <= utf8.MaxRune {
		return 2
	}
	return 1
}

// UTF16Range turns the byte range [start, end) of s into an entity's offset
// and length. Ranges outside s are clamped to it.
func UTF16Range(s string, start, end int) (offset, length int) {
	start = min(max(start, 0), len(s))
	end = min(max(end, start), len(s))
	offset = UTF16Len(s[:start])
	return offset, UTF16Len(s[start:end])
}

// EntityFor marks the first sub in text with typ; false if text lacks sub.
func EntityFor(text, sub string, typ EntityType) (Entity, bool) {
	i := strings.Index(text, sub)
	if i < 0 || sub == "" {
		return Entity{}, false
	}
	off, n := UTF16Range(text, i, i+len(sub))
	return Entity{Offset: off, Length: n, Type: typ}, true
}

// Snippet is text on one line, at most max runes, for a reply's quote.
func Snippet(text string, max int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:max-1])) + "…"
}

// SplitAlbum turns one network message into the protocol's messages: one per
// media (or one without any), with ids[i] as each part's id. Parts share
// album (the first id) when there are several. reply_to, forwarded and
// reactions go on the first part; text, entities, link_preview,
// editable_until and unsupported on the last; the rest on every part.
func SplitAlbum(base Message, media []Media, ids []int64) []Message {
	n := max(len(media), 1)
	if len(ids) != n {
		panic("proto.SplitAlbum: one id per part")
	}
	parts := make([]Message, n)
	for i := range n {
		p := base
		p.ID = ids[i]
		p.Album = 0
		if n > 1 {
			p.Album = ids[0]
		}
		p.Media = nil
		if len(media) > 0 {
			m := media[i]
			p.Media = &m
		}
		if i > 0 {
			p.ReplyTo = nil
			p.Forwarded = false
			p.Reactions = nil
		} else {
			if base.ReplyTo != nil {
				r := *base.ReplyTo
				p.ReplyTo = &r
			}
			p.Reactions = slices.Clone(base.Reactions)
		}
		if i < n-1 {
			p.Text = ""
			p.Entities = nil
			p.LinkPreview = nil
			p.EditableUntil = 0
			p.Unsupported = ""
		} else {
			p.Entities = slices.Clone(base.Entities)
			if base.LinkPreview != nil {
				lp := *base.LinkPreview
				p.LinkPreview = &lp
			}
		}
		parts[i] = p
	}
	return parts
}
