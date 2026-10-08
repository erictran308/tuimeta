// SPDX-License-Identifier: AGPL-3.0-or-later

package metatext

// The rules below are mautrix-meta's pkg/msgconv/textfmt/markdown.go
// (Copyright (C) 2026 Tulir Asokan, AGPL-3.0-or-later), kept as close to it
// as possible so text reads the same, with the HTML rendering left out.

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

type textFormat int

const (
	codeBlock textFormat = iota
	blockQuote
	inlineQuote
	inlineCode
	bold
	italic
	strike
)

// entity is the protocol's name for the format.
func (f textFormat) entity() proto.EntityType {
	switch f {
	case codeBlock:
		return proto.Pre
	case blockQuote, inlineQuote:
		return proto.Quote
	case inlineCode:
		return proto.InlineCode
	case bold:
		return proto.Bold
	case italic:
		return proto.Italic
	}
	return proto.Strike
}

// formatRange is a formatted stretch: [start, end) in bytes of the marked
// text, and its text without the markers.
type formatRange struct {
	format     textFormat
	start, end int
	text       string
}

// rules run in order; a later rule's match that overlaps an earlier one's is
// dropped, which is what keeps `*code*` code and *_this_* only bold.
var rules = []func(string) []formatRange{
	findCodeBlocks,
	findBlockQuotes,
	findInlineQuotes,
	func(text string) []formatRange {
		return findInlineDelimited(text, inlineCode, '`', "*_~'\"(", "word*_~,.;:!?'\")", true)
	},
	func(text string) []formatRange {
		return findInlineDelimited(text, bold, '*', "_~'\"(", "_~,.;:!?'\")", false)
	},
	func(text string) []formatRange {
		return findInlineDelimited(text, italic, '_', "*~'\"(", "*~,.;:!?'\")", false)
	},
	func(text string) []formatRange {
		return findInlineDelimited(text, strike, '~', "*_'\"(", "*_,.;:!?'\")", false)
	},
}

func parseFormatting(text string) []formatRange {
	var ranges []formatRange
	// Quote lines merged into the last range, joined to its text once
	// rather than added one by one (which would copy it each time).
	var merged []string
	flush := func() {
		if len(merged) > 0 {
			last := &ranges[len(ranges)-1]
			last.text += "\n" + strings.Join(merged, "\n")
			merged = merged[:0]
		}
	}
	for _, rule := range rules {
		// The stretches earlier rules took, by where they start. They never
		// overlap one another, and a rule's own matches never overlap each
		// other, so a match is checked against these only, by binary
		// search: one look per match, not one per stretch found so far.
		taken := slices.Clone(ranges)
		slices.SortFunc(taken, func(a, b formatRange) int { return cmp.Compare(a.start, b.start) })
		for _, match := range rule(text) {
			if match.start >= match.end || overlaps(taken, match) {
				continue
			}
			if len(ranges) > 0 {
				// Quote lines one after another are one quote.
				last := &ranges[len(ranges)-1]
				if last.format == match.format && last.end+1 == match.start {
					last.end = match.end
					merged = append(merged, match.text)
					continue
				}
			}
			flush()
			ranges = append(ranges, match)
		}
	}
	flush()
	return ranges
}

// overlaps reports whether match overlaps one of taken, which are sorted by
// start and don't overlap one another (so their ends are in order too).
func overlaps(taken []formatRange, match formatRange) bool {
	// The first stretch ending after match starts is the only one that can
	// overlap it without one before it doing so.
	i, _ := slices.BinarySearchFunc(taken, match.start+1, func(r formatRange, end int) int { return cmp.Compare(r.end, end) })
	return i < len(taken) && taken[i].start < match.end
}

func findInlineDelimited(text string, format textFormat, delimiter byte, prefixChars, suffixChars string, code bool) []formatRange {
	matches := make([]formatRange, 0)
	// Whether a marker can close a stretch doesn't depend on the marker that
	// opened it (the stretch's last character and what follows the marker),
	// so the closers are found once and walked with one pointer, and each
	// word's end is found once: the scan stays linear in the text, whatever
	// a sender puts in it.
	var closers []int
	for i := strings.IndexByte(text, delimiter); i >= 0; {
		if closes(text, i, suffixChars, code) {
			closers = append(closers, i)
		}
		next := strings.IndexByte(text[i+1:], delimiter)
		if next < 0 {
			break
		}
		i += 1 + next
	}
	next := 0
	wordFrom, wordEnd := -1, -1
	for offset := 0; offset < len(text); {
		openRel := strings.IndexByte(text[offset:], delimiter)
		if openRel < 0 {
			break
		}
		open := offset + openRel
		if !hasPrefix(text, open, prefixChars) {
			offset = open + 1
			continue
		}
		if open+1 < wordFrom || open+1 > wordEnd {
			wordFrom, wordEnd = open+1, lineEnd(text, open+1)
		}
		// The first closer leaving at least one character inside, on the
		// same word.
		for next < len(closers) && closers[next] < open+2 {
			next++
		}
		if next < len(closers) && closers[next] < wordEnd && opens(text, open, code) {
			end := closers[next]
			matches = append(matches, formatRange{format: format, start: open, end: end + 1, text: text[open+1 : end]})
			offset = end + 1
			continue
		}
		offset = open + 1
	}
	return matches
}

// opens reports whether the stretch after the marker at open may be
// formatted: not starting with a space (code may).
func opens(text string, open int, code bool) bool {
	if code {
		return true
	}
	first, _ := utf8.DecodeRuneInString(text[open+1:])
	return !unicode.IsSpace(first)
}

// closes reports whether the marker at end may close a stretch: one that
// doesn't end with a space (code may), followed by a space, the end or one
// of suffixChars.
func closes(text string, end int, suffixChars string, code bool) bool {
	if !code {
		last, _ := utf8.DecodeLastRuneInString(text[:end])
		if unicode.IsSpace(last) {
			return false
		}
	}
	return hasSuffix(text, end+1, suffixChars)
}

func hasPrefix(text string, offset int, prefixChars string) bool {
	if offset == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:offset])
	return unicode.IsSpace(prev) || strings.ContainsRune(prefixChars, prev)
}

func hasSuffix(text string, offset int, suffixChars string) bool {
	if offset >= len(text) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(text[offset:])
	if unicode.IsSpace(next) || strings.ContainsRune(suffixChars, next) {
		return true
	}
	return strings.HasPrefix(suffixChars, "word") && isASCIIWord(next)
}

func isASCIIWord(c rune) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z')
}

func lineEnd(text string, offset int) int {
	for offset < len(text) {
		c, size := utf8.DecodeRuneInString(text[offset:])
		if c == '\n' || c == ' ' {
			return offset
		}
		offset += size
	}
	return len(text)
}

func findCodeBlocks(text string) []formatRange {
	matches := make([]formatRange, 0)
	scan := &codeScan{text: text, closes: map[string][]fenceAt{}}
	for offset := 0; offset < len(text); {
		startRel := strings.Index(text[offset:], "```")
		if startRel < 0 {
			break
		}
		start := offset + startRel
		fenceEnd := start
		for fenceEnd < len(text) && text[fenceEnd] == '`' {
			fenceEnd++
		}
		if match, ok := scan.codeBlockAt(start, fenceEnd, text[start:fenceEnd]); ok {
			matches = append(matches, match)
			offset = match.end
		} else {
			offset = start + 1
		}
	}
	return matches
}

// codeScan remembers, for one text, where each fence can close and where
// the line goes on to, so a fence that never closes doesn't make the rest of
// the text be searched again for every opening one.
type codeScan struct {
	text   string
	closes map[string][]fenceAt // per fence, every place it closes, in order
	// The last line break found, and the offsets it's the answer for.
	breakFrom, breakStop, breakNext int
	breakOK, breakKnown             bool
}

type fenceAt struct{ start, end int }

func (s *codeScan) codeBlockAt(start, fenceEnd int, fence string) (formatRange, bool) {
	if lineStop, nextLine, ok := s.nextLineBreak(fenceEnd); ok {
		if closeStart, closeEnd, ok := s.closingFence(nextLine, fence); ok {
			infoLine := s.text[fenceEnd:nextLine]
			language := strings.ToLower(strings.TrimSpace(s.text[fenceEnd:lineStop]))
			code := s.text[nextLine:closeStart]
			if language != "" && !codeBlockLanguages[language] {
				// Not a language name: the first line is code too.
				code = infoLine + code
			}
			return formatRange{format: codeBlock, start: start, end: closeEnd, text: code}, true
		}
	}
	closeStart, closeEnd, ok := s.closingFence(fenceEnd, fence)
	if !ok || closeStart == fenceEnd {
		return formatRange{}, false
	}
	return formatRange{format: codeBlock, start: start, end: closeEnd, text: s.text[fenceEnd:closeStart]}, true
}

// closingFence is the first place from offset on where fence closes a block:
// the fence, then only spaces to the end of the line.
func (s *codeScan) closingFence(offset int, fence string) (start, end int, ok bool) {
	list, known := s.closes[fence]
	if !known {
		list = []fenceAt{}
		for from := 0; ; {
			rel := strings.Index(s.text[from:], fence)
			if rel < 0 {
				break
			}
			at := from + rel
			if end, ok := consumeTail(s.text, at+len(fence)); ok {
				list = append(list, fenceAt{at, end})
			}
			from = at + 1
		}
		s.closes[fence] = list
	}
	i, _ := slices.BinarySearchFunc(list, offset, func(f fenceAt, o int) int { return cmp.Compare(f.start, o) })
	if i == len(list) {
		return 0, 0, false
	}
	return list[i].start, list[i].end, true
}

// nextLineBreak is nextLineBreak(s.text, offset), found once for every
// offset before the same break.
func (s *codeScan) nextLineBreak(offset int) (stop, nextLine int, ok bool) {
	if !s.breakKnown || offset < s.breakFrom || offset > s.breakStop {
		s.breakKnown, s.breakFrom = true, offset
		s.breakStop, s.breakNext, s.breakOK = nextLineBreak(s.text, offset)
		if !s.breakOK {
			s.breakStop = len(s.text)
		}
	}
	if !s.breakOK {
		return 0, 0, false
	}
	return s.breakStop, s.breakNext, true
}

// consumeTail accepts only spaces up to the end of the line, and takes the
// line break too.
func consumeTail(text string, offset int) (int, bool) {
	for offset < len(text) {
		c, size := utf8.DecodeRuneInString(text[offset:])
		if c == '\n' || c == ' ' {
			return offset + size, true
		} else if !unicode.IsSpace(c) {
			return 0, false
		}
		offset += size
	}
	return offset, true
}

func findBlockQuotes(text string) []formatRange {
	matches := make([]formatRange, 0)
	closers := blockQuoteClosers(text)
	for offset := 0; offset < len(text); {
		start := linePrefix(text, offset, ">>>")
		if start < 0 {
			break
		}
		contentStart := start + 3
		if contentStart < len(text) && text[contentStart] == ' ' {
			contentStart++
		}
		closeStart, closeEnd, ok := closers.from(contentStart)
		if !ok {
			offset = contentStart
			continue
		}
		content := text[contentStart:closeStart]
		if !containsNonSpace(content) {
			offset = closeEnd
			continue
		}
		matches = append(matches, formatRange{format: blockQuote, start: start, end: closeEnd, text: content})
		offset = closeEnd
	}
	return matches
}

// quoteClosers are the places a block quote can close, in order: "<<<" at
// a line's start with nothing after it on the line. They're found once per
// text, so quotes that never close don't make it be searched again each.
type quoteClosers []fenceAt

func blockQuoteClosers(text string) quoteClosers {
	var out quoteClosers
	for offset := 0; offset < len(text); {
		closeStart := linePrefix(text, offset, "<<<")
		if closeStart < 0 {
			break
		}
		stop, nextLine := lineBounds(text, closeStart)
		if strings.TrimSpace(text[closeStart+3:stop]) == "" {
			out = append(out, fenceAt{closeStart, nextLine})
		}
		// No closer starts before nextLine: one must follow a space or a
		// line break, and there's none between closeStart and stop.
		offset = max(nextLine, closeStart+1)
	}
	return out
}

// from is the first closer at or after offset.
func (q quoteClosers) from(offset int) (start, end int, ok bool) {
	i, _ := slices.BinarySearchFunc(q, offset, func(f fenceAt, o int) int { return cmp.Compare(f.start, o) })
	if i == len(q) {
		return 0, 0, false
	}
	return q[i].start, q[i].end, true
}

func findInlineQuotes(text string) []formatRange {
	if strings.TrimSpace(text) == ">" {
		return nil
	}
	matches := make([]formatRange, 0)
	for lineStart := 0; lineStart < len(text); {
		stop, nextLine := lineBounds(text, lineStart)
		if strings.HasPrefix(text[lineStart:stop], ">") {
			contentStart := lineStart + 1
			if contentStart < stop && text[contentStart] == ' ' {
				contentStart++
			}
			matches = append(matches, formatRange{format: inlineQuote, start: lineStart, end: stop, text: text[contentStart:stop]})
		}
		lineStart = nextLine
	}
	return matches
}

func linePrefix(text string, offset int, prefix string) int {
	for offset < len(text) {
		rel := strings.Index(text[offset:], prefix)
		if rel < 0 {
			return -1
		}
		idx := offset + rel
		if lineStart(text, idx) {
			return idx
		}
		offset = idx + 1
	}
	return -1
}

func lineStart(text string, offset int) bool {
	if offset == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:offset])
	return prev == '\n' || prev == ' '
}

func lineBounds(text string, offset int) (stop, nextLine int) {
	for offset < len(text) {
		c, size := utf8.DecodeRuneInString(text[offset:])
		if c == '\n' || c == ' ' {
			return offset, offset + size
		}
		offset += size
	}
	return len(text), len(text)
}

func nextLineBreak(text string, offset int) (stop, nextLine int, ok bool) {
	for offset < len(text) {
		c, size := utf8.DecodeRuneInString(text[offset:])
		if c == '\n' || c == ' ' {
			return offset, offset + size, true
		}
		offset += size
	}
	return 0, 0, false
}

func containsNonSpace(text string) bool {
	for _, c := range text {
		if !unicode.IsSpace(c) {
			return true
		}
	}
	return false
}

// codeBlockLanguages are the names Meta accepts after an opening fence;
// any other first line is part of the code.
var codeBlockLanguages = map[string]bool{
	"asp": true, "aspx": true, "bash": true, "c": true, "clj": true,
	"cljc": true, "cljx": true, "clojure": true, "coffeescript": true,
	"cplusplus": true, "cpp": true, "c++": true, "cql": true, "cs": true,
	"csharp": true, "css": true, "curl": true, "d": true, "dart": true,
	"diff": true, "dockerfile": true, "ecmascript": true, "erl": true,
	"erlang": true, "go": true, "gql": true, "gradle": true, "graphql": true,
	"groovy": true, "handlebars": true, "hbs": true, "html": true, "http": true,
	"java": true, "javascript": true, "jl": true, "jruby": true, "js": true,
	"json": true, "jsx": true, "julia": true, "kotlin": true, "kt": true,
	"less": true, "liquid": true, "lua": true, "macruby": true, "markdown": true,
	"ml": true, "mssql": true, "mysql": true, "node": true, "objc": true,
	"objc++": true, "objcpp": true, "objectivec": true,
	"objectivecplusplus": true, "objectivecpp": true, "ocaml": true, "perl": true,
	"pgsql": true, "php": true, "pl": true, "plsql": true, "postgres": true,
	"postgresql": true, "powershell": true, "ps1": true, "py": true,
	"python": true, "r": true, "rake": true, "rb": true, "rbx": true, "rs": true,
	"ruby": true, "rust": true, "sass": true, "scala": true, "scss": true,
	"sh": true, "shell": true, "sol": true, "solidity": true, "sql": true,
	"sqlite": true, "styl": true, "stylus": true, "swift": true, "ts": true,
	"typescript": true, "xhtml": true, "xml": true, "yaml": true, "yml": true,
	"zsh": true,
}
