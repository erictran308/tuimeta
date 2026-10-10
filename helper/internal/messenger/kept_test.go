// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"
)

// openKept opens the encrypted chats' store for the harness's account, as a
// connection does once the socket is up, and closes it when the test ends.
func (h *harness) openKept() int {
	h.t.Helper()
	h.m.mu.Lock()
	h.m.sess.WADevice = fakeOwnJID.String()
	gen := h.m.gen
	h.m.mu.Unlock()
	h.m.openStore(gen)
	if h.kept() == nil {
		h.t.Fatal("no store")
	}
	h.t.Cleanup(func() {
		if !h.m.mu.TryLock() {
			return // a test that failed with the lock held
		}
		st := h.m.store
		h.m.store = nil
		h.m.mu.Unlock()
		if st != nil {
			st.Close()
		}
	})
	return gen
}

// kept is the open store.
func (h *harness) kept() *e2eeStore {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.store
}

// restart forgets everything but the store's file, as quitting and starting
// again does, and reads the store back.
func (h *harness) restart(gen int) {
	h.t.Helper()
	h.m.mu.Lock()
	st := h.m.store
	h.m.store = nil
	h.m.reset()
	h.m.self = selfID
	h.m.ready = true
	h.m.mu.Unlock()
	st.Close()
	h.rec.reset()
	h.m.openStore(gen)
}

// rows is everything the store keeps.
func (h *harness) rows() []storedMessage {
	h.t.Helper()
	all, err := h.kept().all(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return all
}

// waPhoto is an encrypted photo with the given hash, media key and place.
func waPhoto(t *testing.T, sha, mediaKey []byte, path string, thumb []byte) *waConsumerApplication.ConsumerApplication_Content {
	t.Helper()
	anc := &waMediaTransport.WAMediaTransport_Ancillary{FileLength: gproto.Uint64(5), Mimetype: gproto.String("image/jpeg")}
	if thumb != nil {
		anc.Thumbnail = &waMediaTransport.WAMediaTransport_Ancillary_Thumbnail{JPEGThumbnail: thumb}
	}
	img := &waConsumerApplication.ConsumerApplication_ImageMessage{}
	if err := img.Set(&waMediaTransport.ImageTransport{
		Integral: &waMediaTransport.ImageTransport_Integral{Transport: &waMediaTransport.WAMediaTransport{
			Integral:  &waMediaTransport.WAMediaTransport_Integral{FileSHA256: sha, FileEncSHA256: []byte{9}, MediaKey: mediaKey, DirectPath: gproto.String(path)},
			Ancillary: anc,
		}},
		Ancillary: &waMediaTransport.ImageTransport_Ancillary{Width: gproto.Uint32(800), Height: gproto.Uint32(600)},
	}); err != nil {
		t.Fatal(err)
	}
	return &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_ImageMessage{ImageMessage: img}}
}

func TestADeletedEncryptedRequestDoesntComeBackAtTheNextStart(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{{
		ThreadKey: aliceID, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, FolderName: "e2ee_cutover_pending",
		LastActivityTimestampMs: ms(-1),
	}}})
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "Q1", -1, waText("buy my crypto"), nil))
	if c := h.chat(aliceID); c == nil || !c.request {
		t.Fatal("the request wasn't one")
	}
	// Deleted on the phone.
	h.apply(&table.LSTable{LSDeleteMessageRequest: []*table.LSDeleteMessageRequest{{ThreadKey: aliceID}}})
	if len(h.rec.of("chat_removed")) != 1 {
		t.Fatalf("removed %+v", h.rec.of("chat_removed"))
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Errorf("the removed chat's messages are still kept: %+v", rows)
	}
	h.restart(gen)
	h.load()
	for _, c := range h.rec.chats() {
		if c.ID == h.chat(aliceID).id {
			t.Errorf("the deleted request came back: %+v", c)
		}
	}
}

func TestAnEncryptedGroupYouLeftDoesntComeBackAtTheNextStart(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(waGroup, jid(aliceID), "G1", -2, waText("welcome"), nil))
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "D1", -1, waText("hi, just us"), nil))
	self := jid(selfID)
	h.wa(&events.GroupInfo{JID: waGroup, Sender: &self, Timestamp: base, Leave: []waTypes.JID{self}})
	if c := h.chat(777); c != nil {
		t.Fatal("the group you left is still there")
	}
	h.restart(gen)
	if c := h.chat(777); c != nil && c.log.Len() > 0 {
		t.Error("the group you left was read back")
	}
	// The other chat is untouched.
	if c := h.chat(aliceID); c == nil || c.log.Len() != 1 {
		t.Error("another chat's kept messages went too")
	}
}

func TestAMergedFacebookThreadKeepsItsEncryptedMessages(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(waGroup, jid(aliceID), "G1", -2, waText("kept"), nil))
	// The group's Facebook thread is folded into the encrypted one: that
	// isn't the chat leaving.
	h.apply(&table.LSTable{
		LSDeleteThenInsertThread:                         []*table.LSDeleteThenInsertThread{groupThread(groupID, "Trip", -5)},
		LSUpdateThreadAuthorityAndMappingWithOTIDFromJID: []*table.LSUpdateThreadAuthorityAndMappingWithOTIDFromJID{{ThreadKey: groupID, ThreadJID: 777}},
	})
	h.restart(gen)
	if c := h.chat(777); c == nil || c.log.Len() != 1 {
		t.Error("the encrypted group's messages were forgotten")
	}
}

func TestAnEncryptedChatReadHereIsStillReadAtTheNextStart(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	// Messenger's own row for the chat never learns of encrypted receipts:
	// it goes on saying the chat was read before Alice wrote, and that two
	// messages are unread.
	thread := &table.LSDeleteThenInsertThread{
		ThreadKey: aliceID, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, FolderName: "inbox",
		LastActivityTimestampMs: ms(-1), LastReadWatermarkTimestampMs: ms(-10), UnreadMessageCount: 2,
	}
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{thread}})
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "R1", -3, waText("one"), nil))
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "R2", -2, waText("two"), nil))
	msgs := h.rec.messages()
	h.m.receiveWA(waMsg(jid(aliceID), jid(selfID), "R3", -1, waText("my answer"), nil))
	if err := h.m.MarkRead(context.Background(), h.ref(h.chat(aliceID), msgs[len(msgs)-1].ID)); err != nil {
		t.Fatal(err)
	}
	if unread, _, _ := h.readState(aliceID); unread != 0 {
		t.Fatalf("unread %d after reading", unread)
	}

	// The store is read back before Messenger's stale row comes again, and
	// after it.
	for _, rowFirst := range []bool{false, true} {
		h.m.mu.Lock()
		st := h.m.store
		h.m.store = nil
		h.m.reset()
		h.m.self = selfID
		h.m.ready = true
		h.m.mu.Unlock()
		st.Close()
		h.rec.reset()
		if rowFirst {
			h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{thread}})
			h.m.openStore(gen)
		} else {
			h.m.openStore(gen)
			h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{thread}})
		}
		h.load()
		unread, readUpTo, receipted := h.readState(aliceID)
		if unread != 0 || readUpTo != ms(-2) || receipted != ms(-2) {
			t.Errorf("row first %v: unread %d, read up to %d, receipted %d; want 0, %d, %d", rowFirst, unread, readUpTo, receipted, ms(-2), ms(-2))
		}
		chats := h.rec.chats()
		if len(chats) == 0 || chats[len(chats)-1].Unread != 0 {
			t.Errorf("row first %v: tuimeta was told %+v", rowFirst, chats)
		}
	}
	// What's read isn't told again.
	h.e2ee.reads = nil
	msgs = h.rec.messages()
	for _, msg := range msgs {
		if err := h.m.MarkRead(context.Background(), h.ref(h.chat(aliceID), msg.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.e2ee.reads) != 0 {
		t.Errorf("read again: %+v", h.e2ee.reads)
	}
}

func TestAnEncryptedChatsReadPointGoesWithTheChat(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "F1", -2, waText("one"), nil))
	msgs := h.rec.messages()
	if err := h.m.MarkRead(context.Background(), h.ref(h.chat(aliceID), msgs[0].ID)); err != nil {
		t.Fatal(err)
	}
	h.apply(&table.LSTable{LSDeleteThread: []*table.LSDeleteThread{{ThreadKey: aliceID}}})
	reads, err := h.kept().reads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 0 {
		t.Errorf("the deleted chat's read point is still kept: %v", reads)
	}
	h.restart(gen)
	if c := h.chat(aliceID); c != nil && c.readUpTo != 0 {
		t.Errorf("the deleted chat came back read up to %d", c.readUpTo)
	}
}

func TestAPanicWhileApplyingAnEncryptedMessageLeavesTheBackendUsable(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "P0", -2, waPhoto(t, []byte{1}, []byte{2}, "/v/p0", nil), nil))
	// No file ids to give: registering the photo's file panics.
	h.m.mu.Lock()
	files := h.m.d.Files
	h.m.d.Files = nil
	h.m.mu.Unlock()
	h.wa(waMsg(jid(aliceID), jid(aliceID), "P1", -1, waPhoto(t, []byte{3}, []byte{4}, "/v/p1", nil), nil))
	if !h.m.mu.TryLock() {
		t.Fatal("receiving left the lock held")
	}
	h.m.mu.Unlock()

	// Reading the store back panics the same way.
	func() {
		defer func() { _ = recover() }()
		h.restart(gen)
	}()
	if !h.m.mu.TryLock() {
		t.Fatal("reading back left the lock held")
	}
	replaying, dirty := h.m.replaying, h.m.dirty
	h.m.d.Files = files
	h.m.mu.Unlock()
	if replaying || dirty != nil {
		t.Error("reading back was left half done")
	}
	h.rec.reset()
	h.wa(waMsg(jid(aliceID), jid(aliceID), "P2", 0, waText("still here"), nil))
	if msgs := h.rec.messages(); len(msgs) != 1 || msgs[0].Text != "still here" {
		t.Errorf("after a panic, a message arrived as %+v", msgs)
	}
}
