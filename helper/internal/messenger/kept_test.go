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
