// SPDX-License-Identifier: AGPL-3.0-or-later

package ids

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestMessageIDsAreChronologicalWithSlotsForTheSameMillisecond(t *testing.T) {
	m := NewMessages()
	a := m.Assign(1, "a", 1000, 1)
	b := m.Assign(1, "b", 1000, 1)
	c := m.Assign(1, "c", 999, 1)
	d := m.Assign(1, "d", 1001, 1)
	if a[0] != 1000<<8 || b[0] != 1000<<8|1 {
		t.Errorf("same millisecond: %d %d", a[0], b[0])
	}
	if !(c[0] < a[0] && a[0] < b[0] && b[0] < d[0]) {
		t.Errorf("not chronological: %d %d %d %d", c[0], a[0], b[0], d[0])
	}
	if Millis(b[0]) != 1000 {
		t.Errorf("Millis = %d", Millis(b[0]))
	}
	if again := m.Assign(1, "a", 5000, 1); again[0] != a[0] {
		t.Errorf("a known message got a new id: %d", again[0])
	}
	// Another chat has its own slots.
	if other := m.Assign(2, "a", 1000, 1); other[0] != 1000<<8 {
		t.Errorf("other chat: %d", other[0])
	}
}

func TestAFullMillisecondSpillsIntoTheNext(t *testing.T) {
	m := NewMessages()
	var last int64
	for i := range MaxSlot + 2 {
		id := m.Assign(1, string(rune('A'+i)), 7, 1)[0]
		if id <= last {
			t.Fatalf("id %d after %d", id, last)
		}
		last = id
	}
	if Millis(last) != 8 {
		t.Errorf("the 257th id is in ms %d", Millis(last))
	}
}

func TestAnAlbumGetsConsecutiveIDsAndAnyPartFindsTheMessage(t *testing.T) {
	m := NewMessages()
	m.Assign(1, "before", 2000, 1) // takes slot 0
	got := m.Assign(1, "album", 2000, 3)
	want := []int64{2000<<8 | 1, 2000<<8 | 2, 2000<<8 | 3}
	if !slices.Equal(got, want) {
		t.Fatalf("album ids = %v, want %v", got, want)
	}
	for i, id := range got {
		p, ok := m.Lookup(1, id)
		if !ok || p.NetID != "album" || p.Index != i || p.Count != 3 || !slices.Equal(p.IDs, want) {
			t.Errorf("Lookup(%d) = %+v, %v", id, p, ok)
		}
	}
	// A run of three that doesn't fit before a used slot starts after it.
	m.Assign(1, "x", 3000, 1)
	m.Assign(1, "y", 3000, 1)
	m.Assign(2, "gap", 4000, 1)
	two := m.Assign(2, "pair", 4000, 2)
	if two[0] != 4000<<8|1 || two[1] != 4000<<8|2 {
		t.Errorf("pair = %v", two)
	}
	if _, ok := m.Lookup(1, 12345); ok {
		t.Error("an unknown id was found")
	}
}

func TestTemporaryIDsSortLastAndRetire(t *testing.T) {
	m := NewMessages()
	m.Now = func() time.Time { return time.UnixMilli(1000) }
	m.Assign(1, "future", 5000, 1) // the network's clock is ahead
	temps := m.Temp(1, 2)
	if temps[0] <= 5000<<8 || temps[1] != temps[0]+1 {
		t.Fatalf("temps = %v", temps)
	}
	p, ok := m.Lookup(1, temps[1])
	if !ok || !p.Temp || p.Index != 1 {
		t.Fatalf("temp lookup = %+v %v", p, ok)
	}
	m.Retire(1, temps[1])
	if _, ok := m.Lookup(1, temps[1]); ok {
		t.Error("a retired temporary id still names something")
	}
	// Never given out again.
	next := m.Assign(1, "later", Millis(temps[1]), 1)
	if next[0] == temps[0] || next[0] == temps[1] {
		t.Errorf("reused %d", next[0])
	}
}

func TestSplitAlbumPlacesFieldsOnTheRightParts(t *testing.T) {
	base := proto.Message{
		ChatID: 1, Text: "caption", Entities: []proto.Entity{{Offset: 0, Length: 3, Type: proto.Bold}},
		ReplyTo: &proto.ReplyTo{MessageID: 9}, Forwarded: true, Reactions: []proto.Reaction{{Emoji: "❤️", Count: 1}},
		LinkPreview: &proto.LinkPreview{URL: "https://example.com"}, EditableUntil: 99, Edited: true, Deletable: true, Date: 5,
	}
	media := []proto.Media{{Kind: proto.Photo, FileID: 1}, {Kind: proto.Photo, FileID: 2}, {Kind: proto.Photo, FileID: 3}}
	parts := proto.SplitAlbum(base, media, []int64{10, 11, 12})
	for i, p := range parts {
		first, last := i == 0, i == 2
		if p.Album != 10 || p.Media.FileID != int32(i+1) || p.Date != 5 || !p.Edited || !p.Deletable {
			t.Errorf("part %d shared fields: %+v", i, p)
		}
		if (p.ReplyTo != nil) != first || p.Forwarded != first || (len(p.Reactions) > 0) != first {
			t.Errorf("part %d: reply/forward/reactions belong on the first part only", i)
		}
		if (p.Text != "") != last || (len(p.Entities) > 0) != last || (p.LinkPreview != nil) != last || (p.EditableUntil != 0) != last {
			t.Errorf("part %d: text/entities/preview/editable_until belong on the last part only", i)
		}
	}
	one := proto.SplitAlbum(base, nil, []int64{20})
	if len(one) != 1 || one[0].Album != 0 || one[0].Text != "caption" || one[0].ReplyTo == nil {
		t.Errorf("single message: %+v", one)
	}
}

func TestChatAndUserIDsSurviveARestartAndAreNeverReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ids.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	alice := s.User(proto.Messenger, "100")
	chat := s.Chat(proto.Messenger, "100") // same network id, different kind
	ig := s.Chat(proto.Instagram, "100")
	if alice == chat || chat == ig || alice == ig {
		t.Fatalf("ids collide: %d %d %d", alice, chat, ig)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ids.json mode = %v, %v", info.Mode(), err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.User(proto.Messenger, "100") != alice || s2.Chat(proto.Messenger, "100") != chat || s2.Chat(proto.Instagram, "100") != ig {
		t.Fatal("ids changed across a restart")
	}
	if n, id, ok := s2.LookupChat(ig); !ok || n != proto.Instagram || id != "100" {
		t.Errorf("LookupChat = %v %v %v", n, id, ok)
	}
	if _, _, ok := s2.LookupUser(chat); ok {
		t.Error("a chat id was found as a user")
	}
	s2.Forget(proto.Messenger)
	if _, _, ok := s2.LookupChat(chat); ok {
		t.Error("forgotten chat still known")
	}
	again := s2.Chat(proto.Messenger, "100")
	if again == chat || again == alice || again == ig {
		t.Errorf("an id was reused after Forget: %d", again)
	}
	if err := s2.Flush(); err != nil {
		t.Fatal(err)
	}
	s3, _ := Open(path)
	if s3.Chat(proto.Messenger, "100") != again {
		t.Error("Forget and the new id weren't kept")
	}
}

func TestAnUnreadableIDsFileStartsOverFarAway(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ids.json")
	if err := os.WriteFile(path, []byte("{nonsense"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if id := s.Chat(proto.Messenger, "1"); id < 1_000_000 {
		t.Errorf("counting restarted low: %d", id)
	}
	if _, err := os.Stat(path + ".bad"); err != nil {
		t.Errorf("the bad file wasn't kept aside: %v", err)
	}
}

func TestFileIDsAreStablePerKey(t *testing.T) {
	f := NewFiles()
	a := f.Register(FileRef{Network: proto.Messenger, Key: "k", Size: 10, Source: "url1"})
	b := f.Register(FileRef{Network: proto.Messenger, Key: "k", Source: "url2"})
	c := f.Register(FileRef{Network: proto.Instagram, Key: "k"})
	if a != b || a == c || a <= 0 {
		t.Fatalf("ids %d %d %d", a, b, c)
	}
	ref, _ := f.Get(a)
	if ref.Source != "url2" || ref.Size != 10 {
		t.Errorf("ref = %+v", ref)
	}
}
