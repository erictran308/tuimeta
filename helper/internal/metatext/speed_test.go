// SPDX-License-Identifier: AGPL-3.0-or-later

package metatext

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// slowInlineDelimited is the rule as mautrix-meta writes it, kept to check
// the linear one finds exactly the same stretches.
func slowInlineDelimited(text string, format textFormat, delimiter byte, prefixChars, suffixChars string, code bool) []formatRange {
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
			if slowValidInlineContent(content, code) && hasSuffix(text, end+1, suffixChars) {
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

func slowValidInlineContent(content string, code bool) bool {
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

func TestTheLinearRuleFindsWhatTheOriginalFinds(t *testing.T) {
	alphabet := []string{"*", "_", "~", "`", "a", "b", " ", "\n", "(", ")", ".", "é", "\t", "'"}
	rng := rand.New(rand.NewPCG(1, 2))
	type rule struct {
		f              textFormat
		d              byte
		prefix, suffix string
		code           bool
	}
	ruleSet := []rule{
		{inlineCode, '`', "*_~'\"(", "word*_~,.;:!?'\")", true},
		{bold, '*', "_~'\"(", "_~,.;:!?'\")", false},
		{italic, '_', "*~'\"(", "*~,.;:!?'\")", false},
		{strike, '~', "*_'\"(", "*_,.;:!?'\")", false},
	}
	for range 20000 {
		var b strings.Builder
		for range rng.IntN(40) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		text := b.String()
		for _, r := range ruleSet {
			got := findInlineDelimited(text, r.f, r.d, r.prefix, r.suffix, r.code)
			want := slowInlineDelimited(text, r.f, r.d, r.prefix, r.suffix, r.code)
			if !slices.Equal(got, want) {
				t.Fatalf("%q with %q: got %+v, want %+v", text, string(r.d), got, want)
			}
		}
	}
}

// TestNoTextTakesLongToRead feeds the shapes that made each rule slow (a
// marker that opens again and again without closing) at the longest a
// message can be: reading one must stay well under a second.
func TestNoTextTakesLongToRead(t *testing.T) {
	const size = 64 << 10
	texts := map[string]string{}
	for name, unit := range map[string]string{
		"bold":        "(*a",
		"code":        "`a",
		"fences":      "```a",
		"open fences": "```\n",
		"quotes":      ">>>\n",
		"quote lines": "> a\n",
		"mixed":       "*_~`a",
		"words":       "*a* ",
	} {
		texts[name] = strings.Repeat(unit, size/len(unit))
	}
	// One run of backticks is a fence at every backtick, each one shorter.
	texts["backtick run"] = strings.Repeat("`", size)
	texts["backtick run, then a word"] = strings.Repeat("`", size-2) + " a"
	// Runs each longer than the last close the fence before; runs each
	// shorter than the last close none of the fences in it.
	var growing, shrinking strings.Builder
	for n := 3; growing.Len() < size; n++ {
		growing.WriteString(strings.Repeat("`", n) + " ")
	}
	texts["growing runs"] = growing.String()[:size]
	for n := 361; n >= 3 && shrinking.Len() < size; n-- {
		shrinking.WriteString(strings.Repeat("`", n) + "\n")
	}
	texts["shrinking runs"] = shrinking.String()
	for name, text := range texts {
		start := time.Now()
		Parse(text)
		if took := time.Since(start); took > slowdown*50*time.Millisecond {
			t.Errorf("%s: %v for %d bytes", name, took, len(text))
		}
	}
}

// TestManyMentionsDontMakeReadingSlow gives the longest message as many
// mentions as fit in it: putting them back must stay linear in the text and
// the mentions.
func TestManyMentionsDontMakeReadingSlow(t *testing.T) {
	const size = 64 << 10
	every := func(n, step, offset, length int) []Mention {
		ms := make([]Mention, n)
		for i := range ms {
			ms[i] = Mention{Offset: i*step + offset, Length: length, UserID: int64(i%1000 + 1)}
		}
		return ms
	}
	for name, tc := range map[string]struct {
		text      string
		mentions  []Mention
		plain     string
		formatted int
	}{
		"32768 side by side": {strings.Repeat("@a", size/2), every(size/2, 2, 0, 2), strings.Repeat("@a", size/2), 0},
		"65536 one letter":   {strings.Repeat("a", size), every(size, 1, 0, 1), strings.Repeat("a", size), 0},
		"each one bold":      {strings.Repeat("*@a* b ", size/7), every(size/7, 7, 1, 2), strings.Repeat("@a b ", size/7), size / 7},
		"spread out":         {strings.Repeat("@a b ", size/5), every(size/5, 5, 0, 2), strings.Repeat("@a b ", size/5), 0},
	} {
		start := time.Now()
		text, ents := ParseWithMentions(tc.text, tc.mentions)
		if took := time.Since(start); took > slowdown*50*time.Millisecond {
			t.Errorf("%s: %v for %d mentions in %d bytes", name, took, len(tc.mentions), len(tc.text))
		}
		if text != tc.plain || len(ents) != len(tc.mentions)+tc.formatted {
			t.Errorf("%s: %d bytes, %d entities", name, len(text), len(ents))
		}
	}
}
func slowCodeBlocks(text string) []formatRange {
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
		if match, ok := slowCodeBlockAt(text, start, fenceEnd, text[start:fenceEnd]); ok {
			matches = append(matches, match)
			offset = match.end
		} else {
			offset = start + 1
		}
	}
	return matches
}

func slowCodeBlockAt(text string, start, fenceEnd int, fence string) (formatRange, bool) {
	if lineStop, nextLine, ok := nextLineBreak(text, fenceEnd); ok {
		if closeStart, closeEnd, ok := slowClosingFence(text, nextLine, fence); ok {
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
	closeStart, closeEnd, ok := slowClosingFence(text, fenceEnd, fence)
	if !ok || closeStart == fenceEnd {
		return formatRange{}, false
	}
	return formatRange{format: codeBlock, start: start, end: closeEnd, text: text[fenceEnd:closeStart]}, true
}

func slowClosingFence(text string, offset int, fence string) (start, end int, ok bool) {
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

func TestTheCodeBlockRuleFindsWhatTheOriginalFinds(t *testing.T) {
	alphabet := []string{"`", "``", "```", "a", " ", "\n", "\t", "go", "x"}
	rng := rand.New(rand.NewPCG(3, 4))
	for range 50000 {
		var b strings.Builder
		for range rng.IntN(30) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		text := b.String()
		if got, want := findCodeBlocks(text), slowCodeBlocks(text); !slices.Equal(got, want) {
			t.Fatalf("%q: got %+v, want %+v", text, got, want)
		}
	}
}

func TestLongFencesCloseWhereTheOriginalClosesThem(t *testing.T) {
	// Longer texts with runs of many lengths, so a fence has to pass over
	// closers too short for it, near and far.
	rng := rand.New(rand.NewPCG(7, 8))
	tails := []string{" ", "\n", "\t", "a", " ", "go\n", "\t\n", ""}
	for range 3000 {
		var b strings.Builder
		for range rng.IntN(60) {
			b.WriteString(strings.Repeat("`", rng.IntN(9)))
			b.WriteString(tails[rng.IntN(len(tails))])
		}
		text := b.String()
		if got, want := findCodeBlocks(text), slowCodeBlocks(text); !slices.Equal(got, want) {
			t.Fatalf("%q: got %+v, want %+v", text, got, want)
		}
	}
}
func slowParseFormatting(text string) []formatRange {
	var ranges []formatRange
	for _, rule := range slowRules {
		for _, match := range rule(text) {
			if match.start >= match.end || slowOverlaps(ranges, match) {
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

func slowOverlaps(ranges []formatRange, match formatRange) bool {
	for _, existing := range ranges {
		if match.end > existing.start && match.start < existing.end {
			return true
		}
	}
	return false
}

func slowBlockQuotes(text string) []formatRange {
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
		closeStart, closeEnd, ok := slowBlockQuoteClose(text, contentStart)
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

func slowBlockQuoteClose(text string, offset int) (int, int, bool) {
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

var slowRules = []func(string) []formatRange{
	slowCodeBlocks,
	slowBlockQuotes,
	findInlineQuotes,
	func(text string) []formatRange {
		return slowInlineDelimited(text, inlineCode, 0x60, "*_~'\"(", "word*_~,.;:!?'\")", true)
	},
	func(text string) []formatRange {
		return slowInlineDelimited(text, bold, 0x2a, "_~'\"(", "_~,.;:!?'\")", false)
	},
	func(text string) []formatRange {
		return slowInlineDelimited(text, italic, 0x5f, "*~'\"(", "*~,.;:!?'\")", false)
	},
	func(text string) []formatRange {
		return slowInlineDelimited(text, strike, 0x7e, "*_'\"(", "*_,.;:!?'\")", false)
	},
}

func TestTheWholeParserFindsWhatTheOriginalFinds(t *testing.T) {
	alphabet := []string{"*", "_", "~", "`", "```", ">>>", "<<<", "> ", "a", " ", "\n", "go", "é", "(", "."}
	rng := rand.New(rand.NewPCG(5, 6))
	for range 50000 {
		var b strings.Builder
		for range rng.IntN(40) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		text := b.String()
		if got, want := parseFormatting(text), slowParseFormatting(text); !slices.Equal(got, want) {
			t.Fatalf("%q: got %+v, want %+v", text, got, want)
		}
	}
}
