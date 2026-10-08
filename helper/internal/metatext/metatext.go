// SPDX-License-Identifier: AGPL-3.0-or-later

// Package metatext reads the formatting Messenger and Instagram keep as
// markers in a message's text (*bold*, _italic_, ~strike~, `code`, ```code
// blocks```, "> " quotes and >>> … <<< quote blocks) and turns it into the
// protocol's form: the text without the markers, and entities saying what
// is formatted, in UTF-16 code units of that text.
//
// The rules are mautrix-meta's (pkg/msgconv/textfmt/markdown.go), ported so
// a message reads here as it does in Meta's apps: markers only count at word
// edges, nothing nests (the first rule to claim a stretch wins, and what's
// inside it stays as typed), and mentions are kept out of the way while the
// markers are read, then put back as mention entities.
package metatext

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Mention is a stretch of a message's text, as the network sent it, that
// names someone. Offset and Length are UTF-16 code units of that text, the
// unit Meta gives mention ranges in. UserID is the helper's id for the
// person; 0 (a mention of the whole chat, or someone unknown) keeps the text
// but makes no entity.
type Mention struct {
	Offset, Length int
	UserID         int64
}

// Parse is ParseWithMentions without mentions.
func Parse(text string) (string, []proto.Entity) { return ParseWithMentions(text, nil) }

// ParseWithMentions returns text without its formatting markers, and the
// entities for the formatting and the mentions, ordered by offset. Carriage
// returns are dropped, as Meta's own clients do. Text without markers comes
// back as it was (less any '\r').
func ParseWithMentions(text string, mentions []Mention) (string, []proto.Entity) {
	marked, held := hold(text, mentions)
	marked = strings.ReplaceAll(marked, "\r", "")
	ranges := parseFormatting(marked)
	slices.SortStableFunc(ranges, func(a, b formatRange) int { return cmp.Compare(a.start, b.start) })

	var out strings.Builder
	var spans []span
	prevEnd := 0
	for _, r := range ranges {
		if r.start < prevEnd {
			continue
		}
		out.WriteString(marked[prevEnd:r.start])
		body := r.text
		block := r.format == codeBlock || r.format == blockQuote
		if block && r.end == len(marked) {
			// Nothing follows the block, so the line break that ended it
			// would only leave an empty last line.
			body = strings.TrimSuffix(body, "\n")
		}
		start := out.Len()
		out.WriteString(body)
		end := out.Len()
		if block {
			// A block's text keeps the line break before its closing
			// marker's line, so what follows still starts on a line of its
			// own; the entity stops before it.
			end = start + len(strings.TrimRight(body, "\n"))
		}
		if end > start {
			spans = append(spans, span{start, end, r.format.entity(), 0})
		}
		prevEnd = r.end
	}
	out.WriteString(marked[prevEnd:])
	plain := out.String()

	// Put the mentions back where their placeholders ended up, moving every
	// span after (or around) each one by the difference in length.
	for _, h := range held {
		at := strings.Index(plain, h.placeholder)
		if at < 0 {
			continue
		}
		delta := len(h.text) - len(h.placeholder)
		phEnd := at + len(h.placeholder)
		plain = plain[:at] + h.text + plain[phEnd:]
		for i := range spans {
			if spans[i].start >= phEnd {
				spans[i].start += delta
			}
			if spans[i].end >= phEnd {
				spans[i].end += delta
			}
		}
		if h.userID > 0 && h.text != "" {
			spans = append(spans, span{at, at + len(h.text), proto.Mention, h.userID})
		}
	}

	slices.SortStableFunc(spans, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	var ents []proto.Entity
	for _, s := range spans {
		off, n := proto.UTF16Range(plain, s.start, s.end)
		if n == 0 {
			continue
		}
		ents = append(ents, proto.Entity{Offset: off, Length: n, Type: s.typ, UserID: s.user})
	}
	return plain, ents
}

// span is an entity in byte offsets of the text being built.
type span struct {
	start, end int
	typ        proto.EntityType
	user       int64
}

// heldMention is a mention taken out of the text while markers are read.
type heldMention struct {
	placeholder string
	text        string
	userID      int64
}

// hold swaps each mention's text for a placeholder made of letters, as
// mautrix-meta does, so a name with spaces or marker characters in it can
// neither start nor break formatting, while a mention right next to a
// marker is still read as a word there.
func hold(text string, mentions []Mention) (string, []heldMention) {
	if len(mentions) == 0 {
		return text, nil
	}
	ms := slices.Clone(mentions)
	slices.SortStableFunc(ms, func(a, b Mention) int { return cmp.Compare(a.Offset, b.Offset) })
	u := utf16.Encode([]rune(text))
	var out strings.Builder
	var held []heldMention
	prevEnd := 0
	for _, m := range ms {
		if m.Offset < 0 || m.Offset >= len(u) || m.Length <= 0 {
			continue // outside the text, or empty
		}
		// A range that cuts a character in half (between the two units of
		// an emoji's surrogate pair) is widened to the whole character, so
		// the text is never split into invalid halves.
		start, end := m.Offset, min(m.Offset+m.Length, len(u))
		if lowSurrogate(u[start]) && start > 0 {
			start--
		}
		if end < len(u) && lowSurrogate(u[end]) {
			end++
		}
		if start < prevEnd {
			continue // overlaps the mention before
		}
		ph := placeholder(text, held)
		out.WriteString(string(utf16.Decode(u[prevEnd:start])))
		out.WriteString(ph)
		named := strings.ReplaceAll(string(utf16.Decode(u[start:end])), "\r", "")
		held = append(held, heldMention{placeholder: ph, text: named, userID: m.UserID})
		prevEnd = end
	}
	out.WriteString(string(utf16.Decode(u[prevEnd:])))
	return out.String(), held
}

// lowSurrogate reports whether unit is the second half of a surrogate pair.
func lowSurrogate(unit uint16) bool { return unit >= 0xDC00 && unit <= 0xDFFF }

const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// placeholder is 16 random letters found nowhere in text nor in another
// placeholder.
func placeholder(text string, held []heldMention) string {
	for {
		b := make([]byte, 16)
		for i := range b {
			b[i] = letters[rand.IntN(len(letters))]
		}
		ph := string(b)
		taken := strings.Contains(text, ph)
		for _, h := range held {
			taken = taken || strings.Contains(h.placeholder, ph) || strings.Contains(ph, h.placeholder)
		}
		if !taken {
			return ph
		}
	}
}
