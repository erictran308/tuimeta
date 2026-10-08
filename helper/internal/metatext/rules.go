// SPDX-License-Identifier: AGPL-3.0-or-later

package metatext

// The rules below are mautrix-meta's pkg/msgconv/textfmt/markdown.go
// (Copyright (C) 2026 Tulir Asokan, AGPL-3.0-or-later), kept as close to it
// as possible so text reads the same, with the HTML rendering left out.

import (
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
	for _, rule := range rules {
		for _, match := range rule(text) {
			if match.start >= match.end || overlaps(ranges, match) {
				continue
			}
			if len(ranges) > 0 {
				// Quote lines one after another are one quote.
				last := &ranges[len(ranges)-1]
				if last.format == match.format && last.end+1 == match.start {
					last.end = match.end
					last.text += "\n" + match.text
					continue
				}
			}
			ranges = append(ranges, match)
		}
	}
	return ranges
}

func overlaps(ranges []formatRange, match formatRange) bool {
	for _, existing := range ranges {
		if match.end > existing.start && match.start < existing.end {
			return true
		}
	}
	return false
}

func findInlineDelimited(text string, format textFormat, delimiter byte, prefixChars, suffixChars string, code bool) []formatRange {
	matches := make([]formatRange, 0)
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
		searchEnd := lineEnd(text, open+1)
		found := false
		for closeSearch := open + 1; closeSearch < searchEnd; {
			closeRel := strings.IndexByte(text[closeSearch:searchEnd], delimiter)
			if closeRel < 0 {
				break
			}
			end := closeSearch + closeRel
			content := text[open+1 : end]
			if validInlineContent(content, code) && hasSuffix(text, end+1, suffixChars) {
				matches = append(matches, formatRange{format: format, start: open, end: end + 1, text: content})
				offset = end + 1
				found = true
				break
			}
			closeSearch = end + 1
		}
		if !found {
			offset = open + 1
		}
	}
	return matches
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

func validInlineContent(content string, code bool) bool {
	if content == "" || strings.ContainsRune(content, '\n') || strings.ContainsRune(content, ' ') {
		return false
	}
	if code {
		return true
	}
	first, _ := utf8.DecodeRuneInString(content)
	last, _ := utf8.DecodeLastRuneInString(content)
	return !unicode.IsSpace(first) && !unicode.IsSpace(last)
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
		if match, ok := codeBlockAt(text, start, fenceEnd, text[start:fenceEnd]); ok {
			matches = append(matches, match)
			offset = match.end
		} else {
			offset = start + 1
		}
	}
	return matches
}

func codeBlockAt(text string, start, fenceEnd int, fence string) (formatRange, bool) {
	if lineStop, nextLine, ok := nextLineBreak(text, fenceEnd); ok {
		if closeStart, closeEnd, ok := closingFence(text, nextLine, fence); ok {
			infoLine := text[fenceEnd:nextLine]
			language := strings.ToLower(strings.TrimSpace(text[fenceEnd:lineStop]))
			code := text[nextLine:closeStart]
			if language != "" && !codeBlockLanguages[language] {
				// Not a language name: the first line is code too.
				code = infoLine + code
			}
			return formatRange{format: codeBlock, start: start, end: closeEnd, text: code}, true
		}
	}
	closeStart, closeEnd, ok := closingFence(text, fenceEnd, fence)
	if !ok || closeStart == fenceEnd {
		return formatRange{}, false
	}
	return formatRange{format: codeBlock, start: start, end: closeEnd, text: text[fenceEnd:closeStart]}, true
}

func closingFence(text string, offset int, fence string) (start, end int, ok bool) {
	for {
		rel := strings.Index(text[offset:], fence)
		if rel < 0 {
			return 0, 0, false
		}
		start = offset + rel
		if end, ok = consumeTail(text, start+len(fence)); ok {
			return start, end, true
		}
		offset = start + 1
	}
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
	for offset := 0; offset < len(text); {
		start := linePrefix(text, offset, ">>>")
		if start < 0 {
			break
		}
		contentStart := start + 3
		if contentStart < len(text) && text[contentStart] == ' ' {
			contentStart++
		}
		closeStart, closeEnd, ok := blockQuoteClose(text, contentStart)
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

func blockQuoteClose(text string, offset int) (int, int, bool) {
	for {
		closeStart := linePrefix(text, offset, "<<<")
		if closeStart < 0 {
			return 0, 0, false
		}
		stop, nextLine := lineBounds(text, closeStart)
		if strings.TrimSpace(text[closeStart+3:stop]) == "" {
			return closeStart, nextLine, true
		}
		offset = nextLine
	}
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
