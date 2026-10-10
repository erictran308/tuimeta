// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// restartAt is the next run of the helper, its clock at now and its
// downloads dl's.
func (h *harness) restartAt(now time.Time, dl *downloads) *harness {
	h.t.Helper()
	if err := h.deps.IDs.Flush(); err != nil {
		h.t.Fatal(err)
	}
	h.w.Close()
	return newHarnessAt(h.t, h.dir, now, dl)
}

// photo is a photo named name from sender, which disappears secs after it
// was sent (never, with 0).
func photo(chat, sender waTypes.JID, id string, min int, name string, secs uint32) *events.Message {
	evt := &events.Message{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Mimetype: gproto.String("image/jpeg"), DirectPath: gproto.String("/v/" + name), MediaKey: key32(name + " key"),
		FileSHA256: key32(name), FileEncSHA256: key32(name + " enc"), JPEGThumbnail: []byte("thumbnail of " + name),
		Caption: gproto.String(name), ContextInfo: &waE2E.ContextInfo{Expiration: gproto.Uint32(secs)},
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = chat, sender, id, at(min)
	evt.Info.IsGroup = chat.Server == waTypes.GroupServer
	return evt
}

// keysOf is the keys of msg's files.
func keysOf(t *testing.T, msg *message) []string {
	t.Helper()
	if msg == nil {
		t.Fatal("no message")
	}
	var out []string
	for _, r := range fileRefs(msg) {
		out = append(out, r.Key)
	}
	if len(out) == 0 {
		t.Fatal("a photo with no files")
	}
	return out
}

func (d *downloads) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.removed)
}

// gone reports whether every one of keys was deleted.
func (d *downloads) gone(keys []string) bool {
	removed := d.all()
	for _, k := range keys {
		if !slices.Contains(removed, k) {
			return false
		}
	}
	return true
}

func TestADisappearingMessagesFilesGoWithItWhenItWentWhileClosed(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(photo(benLID, benLID, "P1", -1, "holiday", 3600))
	keys := keysOf(t, h.kept(benLID.String(), "P1"))
	dl := &downloads{}
	h2 := h.restartAt(base.Add(2*time.Hour), dl)
	if !dl.gone(keys) {
		t.Errorf("deleted %q, not all of %q", dl.all(), keys)
	}
	if kept, _ := h2.w.st.messages(context.Background(), benLID.String()); len(kept) != 0 {
		t.Error("it's still kept")
	}
}

func TestADisappearingMessageInAChatNotOpenedGoesFromTheListToo(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -10, "before"))
	h.event(photo(benLID, benLID, "P1", -1, "holiday", 3600))
	keys := keysOf(t, h.kept(benLID.String(), "P1"))
	dl := &downloads{}
	h2 := h.restartAt(base.Add(30*time.Second), dl)
	h2.load()
	chats := h2.rec.chats()
	if len(chats) != 1 || chats[0].LastMessage == nil || chats[0].LastMessage.Text != "holiday" {
		t.Fatalf("the list %+v", chats)
	}
	preview := chats[0].LastMessage.ID
	h2.rec.reset()
	h2.w.now = func() time.Time { return base.Add(2 * time.Hour) }
	h2.w.sweep(h2.w.st)
	if !dl.gone(keys) {
		t.Errorf("deleted %q, not all of %q", dl.all(), keys)
	}
	if d := h2.rec.deletions(); len(d) != 1 || d[0].MessageIDs[0] != preview {
		t.Errorf("deletions %+v", d)
	}
	chats = h2.rec.chats()
	if len(chats) != 1 || chats[0].LastMessage == nil || chats[0].LastMessage.Text != "before" {
		t.Errorf("the list still shows it: %+v", chats)
	}
}

func TestAChatClearedOrDeletedTakesTheFilesOfMessagesWhoseTimeIsUp(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		h := newHarness(t)
		h.load()
		h.event(photo(benLID, benLID, "P1", -1, "holiday", 3600))
		keys := keysOf(t, h.kept(benLID.String(), "P1"))
		dl := &downloads{}
		h2 := h.restartAt(base.Add(30*time.Second), dl)
		h2.w.now = func() time.Time { return base.Add(2 * time.Hour) }
		h2.kept(benLID.String(), "B0") // read in: the photo, out of time, isn't
		if deleted {
			h2.event(&events.DeleteChat{JID: benLID})
		} else {
			h2.event(&events.ClearChat{JID: benLID})
		}
		if !dl.gone(keys) {
			t.Errorf("deleted chat %v: deleted %q, not all of %q", deleted, dl.all(), keys)
		}
	}
}

func TestAFileAnotherMessageStillShowsStays(t *testing.T) {
	h := newHarness(t)
	dl := &downloads{}
	h.w.d.Downloads = dl
	h.load()
	// Alice forwards Ben's photo: the same file, the same thumbnail.
	h.event(photo(benLID, benLID, "P1", -5, "holiday", 0))
	h.event(photo(aliceLID, aliceLID, "P2", -4, "holiday", 0))
	keys := keysOf(t, h.kept(benLID.String(), "P1"))
	if !slices.Equal(keys, keysOf(t, h.kept(aliceLID.String(), "P2"))) {
		t.Fatal("not the same files")
	}
	h.event(revokeIn(benLID, benLID, "P1", -3))
	if len(dl.all()) != 0 {
		t.Fatalf("a file Alice's message shows was deleted: %q", dl.all())
	}
	ref := mustFileOf(t, h, aliceLID, "P2")
	if got, ok := h.deps.Files.Get(ref.FileID); !ok || got.Key != keys[0] {
		t.Error("Alice's photo can't be downloaded")
	}
	h.event(revokeIn(aliceLID, aliceLID, "P2", -2))
	if !dl.gone(keys) {
		t.Errorf("deleted %q once no message shows them", dl.all())
	}
}

// mustFileOf is the media of message id in the chat with the person jid, as
// tuimeta was last sent it.
func mustFileOf(t *testing.T, h *harness, jid waTypes.JID, id string) *proto.Media {
	t.Helper()
	c := h.chat(jid.String())
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	m := c.msgs[id]
	if m == nil || m.Media == nil {
		t.Fatalf("%s: no media", id)
	}
	part := h.w.render(c, m)
	return part.Media
}

func TestAFileIsNamedOnlyByAHashAndAKeyOfWhatsAppsSize(t *testing.T) {
	h := newHarness(t)
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	whole := key32("x")
	for name, md := range map[string]*media{
		"short hash": {Kind: proto.FileMedia, DirectPath: "/a", MediaKey: key32("k"), FileSHA256: whole[:31], FileEncSHA256: []byte("e")},
		"long key":   {Kind: proto.FileMedia, DirectPath: "/a", MediaKey: append(key32("k"), 0), FileSHA256: whole, FileEncSHA256: []byte("e")},
	} {
		if out := h.w.mediaOf(md); out.FileID != 0 {
			t.Errorf("%s: offered for download", name)
		}
	}
}

func TestWhatTheStoresBoundTakesTakesItsFiles(t *testing.T) {
	h := newHarness(t)
	dl := &downloads{}
	h.w.d.Downloads = dl
	h.load()
	h.event(text(benLID, benLID, "B0", -100, "first"))
	c := h.chat(benLID.String())
	many := make([]*message, MaxStoredPerChat+1)
	for i := range many {
		many[i] = &message{ID: "M" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('A'+i/26%26)) + string(rune('0'+i/676)),
			Sender: benLID.String(), MS: at(-90).UnixMilli() + int64(i)*1000, Text: "x"}
	}
	shown := &message{ID: "S1", Sender: benLID.String(), MS: at(-95).UnixMilli(), Media: photoMedia("shown")}
	hidden := &message{ID: "H1", Sender: benLID.String(), MS: at(-96).UnixMilli(), Media: photoMedia("hidden")}
	h.w.mu.Lock()
	h.w.keep(c, shown)
	h.w.store(c, hidden, shown)
	h.w.store(c, many...)
	h.w.mu.Unlock()
	if !dl.gone(keysOf(t, hidden)) {
		t.Errorf("the files of a message the bound took stayed: %q", dl.all())
	}
	if slices.ContainsFunc(dl.all(), func(k string) bool { return slices.Contains(keysOf(t, shown), k) }) {
		t.Fatal("the file of a message shown went at once")
	}
	h.w.Close()
	if !dl.gone(keysOf(t, shown)) {
		t.Errorf("the files of a shown message the bound took stayed after the run: %q", dl.all())
	}
}

func photoMedia(name string) *media {
	return &media{Kind: proto.Photo, DirectPath: "/v/" + name, MediaKey: key32(name + " key"), FileSHA256: key32(name),
		FileEncSHA256: key32(name + " enc"), Thumb: []byte("thumbnail of " + name)}
}

func TestADeviceThePhoneUnlinkedGoesOnLosingWhatDisappears(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(photo(benLID, benLID, "P1", -1, "holiday", 3600))
	keys := keysOf(t, h.kept(benLID.String(), "P1"))
	h.event(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	h.deps.IDs.Flush()
	h.w.Close()

	w, _ := fresh(t, h.dir)
	dl := &downloads{}
	w.d.Downloads = dl
	w.dial = func(int, *wastore.Device) error { t.Error("it tried to connect"); return nil }
	w.Start(context.Background())
	w.keeping.Wait()
	w.mu.Lock()
	st := w.st
	w.now = func() time.Time { return base.Add(2 * time.Hour) }
	w.mu.Unlock()
	if st == nil {
		t.Fatal("the unlinked device's store isn't open")
	}
	w.sweep(st)
	if !dl.gone(keys) {
		t.Errorf("deleted %q, not all of %q", dl.all(), keys)
	}
	if kept, _ := st.messages(context.Background(), benLID.String()); len(kept) != 0 {
		t.Error("it's still kept")
	}
}

func TestAnEditThatChangesALinksCardDeletesTheOldCardsPicture(t *testing.T) {
	h := newHarness(t)
	dl := &downloads{}
	h.w.d.Downloads = dl
	h.load()
	evt := &events.Message{Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: gproto.String("look example.com/a"), MatchedText: gproto.String("example.com/a"),
		Title: gproto.String("A"), JPEGThumbnail: []byte("card picture"),
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "B1", at(-5)
	h.event(evt)
	keys := keysOf(t, h.kept(benLID.String(), "B1"))
	edit := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: gproto.String("B1")},
		EditedMessage: words("look"), TimestampMS: gproto.Int64(at(-4).UnixMilli()),
	}}}
	edit.Info.Chat, edit.Info.Sender, edit.Info.ID, edit.Info.Timestamp = benLID, benLID, "E1", at(-4)
	h.event(edit)
	if !dl.gone(keys) {
		t.Errorf("deleted %q, not the old card's %q", dl.all(), keys)
	}
}
