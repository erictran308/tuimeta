// SPDX-License-Identifier: AGPL-3.0-or-later

package metatext

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func ent(off, n int, typ proto.EntityType) proto.Entity {
	return proto.Entity{Offset: off, Length: n, Type: typ}
}

func check(t *testing.T, input string, mentions []Mention, wantText string, want ...proto.Entity) {
	t.Helper()
	got, ents := ParseWithMentions(input, mentions)
	if got != wantText {
		t.Errorf("Parse(%q) text = %q, want %q", input, got, wantText)
	}
	if !slices.Equal(ents, want) {
		t.Errorf("Parse(%q) entities = %+v, want %+v", input, ents, want)
	}
}

func TestInlineMarkersBecomeEntitiesAndLeaveTheText(t *testing.T) {
	check(t, "hello *bold* _italic_ ~strike~ `code <x>`", nil,
		"hello bold italic strike code <x>",
		ent(6, 4, proto.Bold), ent(11, 6, proto.Italic), ent(18, 6, proto.Strike), ent(25, 8, proto.InlineCode))
}

func TestTheFirstRuleWinsAndWhatsInsideStaysAsTyped(t *testing.T) {
	// markdown_test.go's nested case: code keeps its stars, bold its
	// underscores.
	check(t, "`*not bold*` and *_bold wins_*", nil,
		"*not bold* and _bold wins_",
		ent(0, 10, proto.InlineCode), ent(15, 11, proto.Bold))
	// Rules go code, bold, italic, strike: bold inside strike claims its
	// stretch first, and the strike around it is dropped.
	check(t, "~*both*~", nil, "~both~", ent(1, 4, proto.Bold))
	check(t, "*~both~*", nil, "~both~", ent(0, 6, proto.Bold))
}

func TestACodeBlockWithALanguageDropsItsFences(t *testing.T) {
	check(t, "before\n```python\nprint(\"<hi>\")\n```\nafter", nil,
		"before\nprint(\"<hi>\")\nafter", ent(7, 13, proto.Pre))
}

func TestCarriageReturnsAreDroppedBeforeReading(t *testing.T) {
	check(t, "before\r\n*bold*\r\n```go\r\nfmt.Println(\"hi\")\r\n```\r\nafter", nil,
		"before\nbold\nfmt.Println(\"hi\")\nafter",
		ent(7, 4, proto.Bold), ent(12, 17, proto.Pre))
}

func TestAnUnknownLanguageLineIsPartOfTheCode(t *testing.T) {
	check(t, "```notreal\nbody\n```", nil, "notreal\nbody", ent(0, 12, proto.Pre))
}

func TestQuoteLinesTogetherAreOneQuote(t *testing.T) {
	check(t, "> first\n> second", nil, "first\nsecond", ent(0, 12, proto.Quote))
	check(t, "> one\nnot quoted\n> two", nil, "one\nnot quoted\ntwo",
		ent(0, 3, proto.Quote), ent(15, 3, proto.Quote))
	check(t, ">", nil, ">")
}

func TestABlockQuoteBetweenMarkers(t *testing.T) {
	check(t, ">>> quoted\nlines\n<<<\nafter", nil, "quoted\nlines\nafter", ent(0, 12, proto.Quote))
}

func TestMarkersInsideWordsAreJustCharacters(t *testing.T) {
	for _, s := range []string{"snake_case_name", "2*3*4 = 24", "a~b~c", "* not bold *", "*unclosed", "``", "*\nbold*"} {
		check(t, s, nil, s)
	}
	// Punctuation after a marker still ends it.
	check(t, "(*really*)!", nil, "(really)!", ent(1, 6, proto.Bold))
}

func TestOffsetsAreUTF16(t *testing.T) {
	check(t, "😀 *hi* 🎉 _yo_", nil, "😀 hi 🎉 yo", ent(3, 2, proto.Bold), ent(9, 2, proto.Italic))
}

func TestPlainTextComesBackUnchanged(t *testing.T) {
	text, ents := Parse("just words, no markers.")
	if text != "just words, no markers." || ents != nil {
		t.Errorf("got %q %+v", text, ents)
	}
}

func TestAMentionKeepsItsTextAndGetsAnEntity(t *testing.T) {
	// "*@Alice*": the mention is held while markers are read, as
	// markdown_test.go's placeholder case.
	check(t, "*@Alice*", []Mention{{Offset: 1, Length: 6, UserID: 7}}, "@Alice",
		ent(0, 6, proto.Bold), proto.Entity{Offset: 0, Length: 6, Type: proto.Mention, UserID: 7})
}

func TestAMentionsOwnMarkerCharactersFormatNothing(t *testing.T) {
	check(t, "hi @A_b_c and _x_", []Mention{{Offset: 3, Length: 6, UserID: 9}}, "hi @A_b_c and x",
		proto.Entity{Offset: 3, Length: 6, Type: proto.Mention, UserID: 9}, ent(14, 1, proto.Italic))
	// A name with spaces stays one word for the markers around it.
	check(t, "_@Ann Lee_ hi", []Mention{{Offset: 1, Length: 8, UserID: 2}}, "@Ann Lee hi",
		ent(0, 8, proto.Italic), proto.Entity{Offset: 0, Length: 8, Type: proto.Mention, UserID: 2})
}

func TestEntitiesAfterAMentionMoveWithIt(t *testing.T) {
	check(t, "*x* @Bob *y*", []Mention{{Offset: 4, Length: 4, UserID: 3}}, "x @Bob y",
		ent(0, 1, proto.Bold), proto.Entity{Offset: 2, Length: 4, Type: proto.Mention, UserID: 3}, ent(7, 1, proto.Bold))
	// UTF-16 mention offsets past an emoji.
	check(t, "🙂 @Zoë *ok*", []Mention{{Offset: 3, Length: 4, UserID: 4}}, "🙂 @Zoë ok",
		proto.Entity{Offset: 3, Length: 4, Type: proto.Mention, UserID: 4}, ent(8, 2, proto.Bold))
}

func TestAMentionInsideCodeIsStillPutBack(t *testing.T) {
	check(t, "`@Bob`", []Mention{{Offset: 1, Length: 4, UserID: 5}}, "@Bob",
		ent(0, 4, proto.InlineCode), proto.Entity{Offset: 0, Length: 4, Type: proto.Mention, UserID: 5})
}

func TestBadOrUnknownMentionsChangeNothing(t *testing.T) {
	ms := []Mention{
		{Offset: 0, Length: 3, UserID: 1},  // fine
		{Offset: 2, Length: 3, UserID: 2},  // overlaps the first
		{Offset: 50, Length: 3, UserID: 3}, // past the end
		{Offset: 4, Length: 0, UserID: 4},  // empty
		{Offset: 4, Length: 9, UserID: 0},  // the whole chat: text, no entity
	}
	check(t, "@Al @everyone", ms, "@Al @everyone", proto.Entity{Offset: 0, Length: 3, Type: proto.Mention, UserID: 1})
}

func TestMentionsInAnyOrderAreRead(t *testing.T) {
	ms := []Mention{{Offset: 5, Length: 2, UserID: 2}, {Offset: 0, Length: 2, UserID: 1}}
	check(t, "@A + @B", ms, "@A + @B",
		proto.Entity{Offset: 0, Length: 2, Type: proto.Mention, UserID: 1},
		proto.Entity{Offset: 5, Length: 2, Type: proto.Mention, UserID: 2})
}

func TestAMentionCuttingAnEmojiInHalfTakesTheWholeEmoji(t *testing.T) {
	// "a😀b😀c": the emojis are units 1-2 and 4-5.
	for _, tc := range []struct {
		name     string
		mention  Mention
		wantEnt  proto.Entity
		wantText string
	}{
		{"starts inside", Mention{Offset: 2, Length: 2, UserID: 1}, proto.Entity{Offset: 1, Length: 3, Type: proto.Mention, UserID: 1}, "a😀b😀c"},
		{"ends inside", Mention{Offset: 3, Length: 2, UserID: 1}, proto.Entity{Offset: 3, Length: 3, Type: proto.Mention, UserID: 1}, "a😀b😀c"},
		{"both halves", Mention{Offset: 2, Length: 3, UserID: 1}, proto.Entity{Offset: 1, Length: 5, Type: proto.Mention, UserID: 1}, "a😀b😀c"},
	} {
		text, ents := ParseWithMentions("a😀b😀c", []Mention{tc.mention})
		if text != tc.wantText || !utf8.ValidString(text) || strings.ContainsRune(text, utf8.RuneError) {
			t.Errorf("%s: text %q", tc.name, text)
		}
		if len(ents) != 1 || ents[0] != tc.wantEnt {
			t.Errorf("%s: entities %+v, want %+v", tc.name, ents, tc.wantEnt)
		}
	}
	// Two mentions that only overlap once widened: the second is dropped.
	text, ents := ParseWithMentions("a😀b", []Mention{{Offset: 0, Length: 2, UserID: 1}, {Offset: 2, Length: 2, UserID: 2}})
	if text != "a😀b" || len(ents) != 1 || ents[0].UserID != 1 || ents[0].Length != 3 {
		t.Errorf("overlap after widening: %q %+v", text, ents)
	}
	// Formatting around a widened mention still reads.
	text, ents = ParseWithMentions("*😀x* y", []Mention{{Offset: 2, Length: 1, UserID: 3}})
	if text != "😀x y" || !utf8.ValidString(text) || len(ents) != 2 {
		t.Errorf("with formatting: %q %+v", text, ents)
	}
}
