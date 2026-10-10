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
	plain, spans := putBack(out.String(), spans, held)

	slices.SortStableFunc(spans, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	var ents []proto.Entity
	// The spans are in order of where they start, so the UTF-16 offset is
	// carried from one to the next rather than counted from the text's
	// start each time.
	at, units := 0, 0
	for _, s := range spans {
		start := min(max(s.start, 0), len(plain))
		end := min(max(s.end, start), len(plain))
		if start > at {
			units += proto.UTF16Len(plain[at:start])
			at = start
		}
		n := proto.UTF16Len(plain[start:end])
		if n == 0 {
			continue
		}
		ents = append(ents, proto.Entity{Offset: units, Length: n, Type: s.typ, UserID: s.user})
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
	type stretch struct {
		start, end int
		userID     int64
	}
	var kept []stretch
	prevEnd := 0
	for _, m := range ms {
		if m.Offset < 0 || m.Offset >= len(u) || m.Length <= 0 {
			continue // outside the text, or empty
		}
		// The offset is inside the text, so however long a length the
		// sender gave, adding at most what's left of the text can't
		// overflow: the range stops at the text's end.
		start, end := m.Offset, m.Offset+min(m.Length, len(u)-m.Offset)
		// A range that cuts a character in half (between the two units of
		// an emoji's surrogate pair) is widened to the whole character, so
		// the text is never split into invalid halves.
		if lowSurrogate(u[start]) && start > 0 {
			start--
		}
		if end < len(u) && lowSurrogate(u[end]) {
			end++
		}
		if start < prevEnd {
			continue // overlaps the mention before
		}
		kept = append(kept, stretch{start, end, m.UserID})
		prevEnd = end
	}
	phs := placeholders(text, len(kept))
	var out strings.Builder
	held := make([]heldMention, len(kept))
	prevEnd = 0
	for i, k := range kept {
		out.WriteString(string(utf16.Decode(u[prevEnd:k.start])))
		out.WriteString(phs[i])
		named := strings.ReplaceAll(string(utf16.Decode(u[k.start:k.end])), "\r", "")
		held[i] = heldMention{placeholder: phs[i], text: named, userID: k.userID}
		prevEnd = k.end
	}
	out.WriteString(string(utf16.Decode(u[prevEnd:])))
	return out.String(), held
}

// putBack puts the mentions back where their placeholders ended up in
// plain, in one pass over it, and moves each span by the difference in
// length of the placeholders before (or around) it. The mentions' own spans
// come after the formatting's, so where both start at once the formatting
// stays first.
func putBack(plain string, spans []span, held []heldMention) (string, []span) {
	if len(held) == 0 {
		return plain, spans
	}
	which := make(map[string]int, len(held))
	for i, h := range held {
		which[h.placeholder] = i
	}
	// From each placeholder's end in plain on, positions move by the
	// difference in length of all the placeholders so far.
	type move struct{ from, by int }
	var moves []move
	var mentions []span
	var out strings.Builder
	out.Grow(len(plain))
	prev, by := 0, 0
	eachLetterRun(plain, func(at int) bool {
		i, ok := which[plain[at:at+placeholderLen]]
		if !ok {
			return false
		}
		delete(which, held[i].placeholder) // each is put back once
		out.WriteString(plain[prev:at])
		if h := held[i]; h.userID > 0 && h.text != "" {
			mentions = append(mentions, span{out.Len(), out.Len() + len(h.text), proto.Mention, h.userID})
		}
		out.WriteString(held[i].text)
		prev = at + placeholderLen
		by += len(held[i].text) - placeholderLen
		moves = append(moves, move{prev, by})
		return true
	})
	out.WriteString(plain[prev:])
	moved := func(at int) int {
		// The placeholders ending at or before at move it.
		n, _ := slices.BinarySearchFunc(moves, at+1, func(m move, at int) int { return cmp.Compare(m.from, at) })
		if n == 0 {
			return at
		}
		return at + moves[n-1].by
	}
	for i := range spans {
		spans[i].start, spans[i].end = moved(spans[i].start), moved(spans[i].end)
	}
	return out.String(), append(spans, mentions...)
}

// lowSurrogate reports whether unit is the second half of a surrogate pair.
func lowSurrogate(unit uint16) bool { return unit >= 0xDC00 && unit <= 0xDFFF }

const (
	letters        = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	placeholderLen = 16
)

// placeholders are n different strings of 16 random letters, none of them
// found in text. The text is looked through once for all of them (a pass
// again only in the unlikely case one of them was in it), not once for each.
func placeholders(text string, n int) []string {
	out := make([]string, n)
	taken := make(map[string]int, n)
	fresh := func(i int) {
		for {
			var b [placeholderLen]byte
			for j := range b {
				b[j] = letters[rand.IntN(len(letters))]
			}
			if ph := string(b[:]); taken[ph] == 0 {
				taken[ph] = i + 1
				out[i] = ph
				return
			}
		}
	}
	for i := range out {
		fresh(i)
	}
	for again := n > 0; again; {
		again = false
		eachLetterRun(text, func(at int) bool {
			if i := taken[text[at:at+placeholderLen]] - 1; i >= 0 {
				delete(taken, out[i])
				fresh(i)
				again = true
			}
			return false
		})
	}
	return out
}

// eachLetterRun calls found with the start of every 16 letters in a row in
// s, in order. When found says it took those letters, the next look starts
// after them.
func eachLetterRun(s string, found func(at int) bool) {
	run := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			run++
		} else {
			run = 0
			continue
		}
		if run >= placeholderLen && found(i+1-placeholderLen) {
			run = 0
		}
	}
}
