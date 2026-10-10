// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"
)

// convOf is one chat of a history sync.
func convOf(chat waTypes.JID, msgs ...*waHistorySync.HistorySyncMsg) *waHistorySync.Conversation {
	return &waHistorySync.Conversation{ID: gproto.String(chat.String()), Messages: msgs}
}

// placeholder is a message from sender that couldn't be decrypted.
func placeholder(chat, sender waTypes.JID, id string, min int) *events.UndecryptableMessage {
	und := &events.UndecryptableMessage{}
	und.Info.Chat, und.Info.Sender, und.Info.ID, und.Info.Timestamp = chat, sender, id, at(min)
	und.Info.IsGroup = chat.Server == waTypes.GroupServer
	return und
}

func TestADeletedMessageNeverComesBack(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -20, "hi"))

	// It couldn't be decrypted, Ben deleted it, then it was decrypted.
	h.event(placeholder(benLID, benLID, "U1", -10))
	h.event(revokeIn(benLID, benLID, "U1", -9))
	h.rec.reset()
	h.event(text(benLID, benLID, "U1", -10, "deleted for everyone"))
	if len(h.rec.messages()) != 0 || h.kept(benLID.String(), "U1") != nil {
		t.Error("a message its sender deleted came back with its decryption")
	}

	// Deleted before this device had it (Ben's, then your phone's), then the
	// phone's history brings it.
	h.event(revokeIn(benLID, benLID, "B9", -8))
	h.event(&events.DeleteForMe{ChatJID: benLID, MessageID: "B8", Timestamp: at(-7)})
	h.event(revokeIn(carolLID, carolLID, "C9", -6)) // a chat not here yet
	h.event(syncOf(waHistorySync.HistorySync_RECENT,
		convOf(benLID, webMsg("B9", false, "", -30, "unsent"), webMsg("B8", false, "", -31, "deleted for me")),
		convOf(carolLID, webMsg("C9", false, "", -30, "unsent too"), webMsg("C8", false, "", -31, "still here")),
	))
	for _, m := range [][2]string{{benLID.String(), "B9"}, {benLID.String(), "B8"}, {carolLID.String(), "C9"}} {
		if h.kept(m[0], m[1]) != nil {
			t.Errorf("%s came back with the phone's history", m[1])
		}
	}
	if h.kept(carolLID.String(), "C8") == nil {
		t.Error("a message no one deleted didn't come")
	}

	// Nor in the next run.
	h2 := h.restart()
	h2.event(text(benLID, benLID, "U1", -10, "deleted for everyone"))
	h2.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(benLID, webMsg("B9", false, "", -30, "unsent"))))
	for _, id := range []string{"U1", "B9", "B8"} {
		if h2.kept(benLID.String(), id) != nil {
			t.Errorf("%s came back in the next run", id)
		}
	}
	if h2.kept(benLID.String(), "B1") == nil {
		t.Error("a message no one deleted is gone")
	}
}

func TestADeletionKeepsOutOnlyItsSendersMessageUnlessItWasKept(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(groupJID, aliceLID, "G0", -30, "hello"))
	// Alice says she deletes G5, which is Ben's: it still comes.
	h.event(revokeIn(groupJID, aliceLID, "G5", -20))
	h.event(syncOf(waHistorySync.HistorySync_RECENT, convOf(groupJID, webMsg("G5", false, benLID.String(), -25, "Ben's"))))
	if m := h.kept(groupJID.String(), "G5"); m == nil || m.Sender != benLID.String() {
		t.Fatalf("someone else's deletion kept Ben's message out: %+v", m)
	}
	// Alice deletes her kept G0: no one's message takes its id any more.
	h.event(revokeIn(groupJID, aliceLID, "G0", -10))
	h.event(text(groupJID, benLID, "G0", -9, "pay 500 to account 1234"))
	if h.kept(groupJID.String(), "G0") != nil {
		t.Error("another sender's message took a deleted message's id")
	}
}

func TestTheHistorysDeletionsDeleteAsLiveOnesDo(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(groupJID, aliceLID, "G1", -30, "one"))
	h.event(text(groupJID, aliceLID, "G2", -29, "two"))
	h.event(text(groupJID, aliceLID, "G3", -28, "three"))
	h.rec.reset()
	stub := &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: gproto.String("G1"), Participant: gproto.String(aliceLID.String())},
		MessageTimestamp: gproto.Uint64(uint64(at(-30).Unix())), MessageStubType: waWeb.WebMessageInfo_REVOKE.Enum(),
	}}
	revoke := func(id, by string) *waHistorySync.HistorySyncMsg {
		return webOf("R"+id, false, by, -5, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: gproto.String(id)},
		}})
	}
	h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(groupJID, stub, revoke("G2", aliceLID.String()), revoke("G3", benLID.String()))))
	if h.kept(groupJID.String(), "G1") != nil || h.kept(groupJID.String(), "G2") != nil {
		t.Error("a deletion in the phone's history was left undone")
	}
	if h.kept(groupJID.String(), "G3") == nil {
		t.Error("someone else's deletion in the history deleted Alice's message")
	}
	if n := len(h.rec.deletions()); n != 2 {
		t.Errorf("%d deletions reported", n)
	}
	// One in the same sync as the message it deletes: neither is kept.
	h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(groupJID, revoke("G4", benLID.String()), webMsg("G4", false, benLID.String(), -40, "gone"))))
	if h.kept(groupJID.String(), "G4") != nil {
		t.Error("a message the same sync deleted was kept")
	}
}

func TestASecondMessageWithAnIdInOneSyncNeverReplacesTheFirst(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP,
		convOf(benLID, webMsg("X1", true, "", -20, "my original"), webMsg("X1", false, "", -10, "Ben's")),
		convOf(groupJID, webMsg("G1", false, aliceLID.String(), -20, "Alice's"), webMsg("G1", false, benLID.String(), -10, "Ben's")),
	))
	h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(groupJID, webMsg("G2", false, aliceLID.String(), -40, "Alice's"), webMsg("G2", false, benLID.String(), -35, "Ben's"))))
	check := func(h *harness) {
		t.Helper()
		for _, c := range []struct {
			chat waTypes.JID
			id   string
			want string
		}{{benLID, "X1", "my original"}, {groupJID, "G1", "Alice's"}, {groupJID, "G2", "Alice's"}} {
			if m := h.kept(c.chat.String(), c.id); m == nil || m.Text != c.want {
				t.Errorf("%s: %+v", c.id, m)
			}
		}
	}
	check(h)
	check(h.restart())
}

// editEntry is an edit as the phone's history may hold it: its own entry,
// wrapped as WhatsApp's apps send it.
func editEntry(id string, fromMe bool, participant string, min int, target string, content *waE2E.Message) *waHistorySync.HistorySyncMsg {
	return webOf(id, fromMe, participant, min, &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: gproto.String(target)},
			EditedMessage: content, TimestampMS: gproto.Int64(at(min).UnixMilli()),
		},
	}}})
}

func words(s string) *waE2E.Message { return &waE2E.Message{Conversation: gproto.String(s)} }

func TestAnEditInThePhonesHistoryIsAnEditByItsRules(t *testing.T) {
	h := newHarness(t)
	h.load()
	// Ben's edit of Alice's message comes a page before the message.
	h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(groupJID, editEntry("E1", false, benLID.String(), -10, "G7", words("I owe Ben 500")))))
	h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(groupJID, webMsg("G7", false, aliceLID.String(), -12, "lunch at 1?"))))
	if m := h.kept(groupJID.String(), "G7"); m == nil || m.Sender != aliceLID.String() || m.Text != "lunch at 1?" || m.Edited {
		t.Fatalf("Alice's message %+v", m)
	}
	// Alice's own edit, after it in the same sync, and one that came first
	// and waited: each is an edit.
	h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(groupJID,
		editEntry("E2", false, aliceLID.String(), -50, "G8", words("lunch at 2?")),
		webMsg("G8", false, aliceLID.String(), -55, "lunch at 1?"),
	)))
	h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(groupJID, editEntry("E3", false, aliceLID.String(), -58, "G9", words("see you")))))
	h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(groupJID, webMsg("G9", false, aliceLID.String(), -60, "see u"))))
	for id, want := range map[string]string{"G8": "lunch at 2?", "G9": "see you"} {
		if m := h.kept(groupJID.String(), id); m == nil || m.Text != want || !m.Edited || m.Sender != aliceLID.String() {
			t.Errorf("%s: %+v", id, m)
		}
	}
	// An edit changes only the text, however it's made.
	photo := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: gproto.String("now a photo"), DirectPath: gproto.String("/p"),
		MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e")}}
	h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, convOf(groupJID,
		webMsg("G6", false, aliceLID.String(), -70, "plain"),
		editEntry("E4", false, aliceLID.String(), -69, "G6", photo),
	)))
	if m := h.kept(groupJID.String(), "G6"); m == nil || m.Media != nil || m.Text != "now a photo" || !m.Edited {
		t.Errorf("an edit made a photo: %+v", m)
	}
	if h.kept(groupJID.String(), "E4") != nil || h.kept(groupJID.String(), "E1") != nil {
		t.Error("an edit was kept as a message of its own")
	}
}

func TestEditsLeaveEventsAndOldMessagesAsTheyWere(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(timer(benLID, benLID, 86400)) // id "T" + Ben's
	h.event(text(benLID, benLID, "B1", -60, "the deal is 100"))
	h.rec.reset()
	edit := func(target string, min int, body string) *events.Message {
		evt := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: gproto.String(target)},
			EditedMessage: words(body), TimestampMS: gproto.Int64(at(min).UnixMilli()),
		}}}
		evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "E"+target, at(min)
		return evt
	}
	h.event(edit("T"+benLID.User, 0, "Ben turned off disappearing messages"))
	h.event(revokeIn(benLID, benLID, "T"+benLID.User, 0))
	h.event(edit("B1", 0, "the deal is 1000"))
	if len(h.rec.messages()) != 0 || len(h.rec.deletions()) != 0 {
		t.Errorf("an event or an hour-old message changed: %+v %+v", h.rec.messages(), h.rec.deletions())
	}
	h.event(edit("B1", -50, "the deal is 120"))
	if msgs := h.rec.messages(); len(msgs) != 1 || msgs[0].Text != "the deal is 120" || !msgs[0].Edited {
		t.Errorf("an edit in WhatsApp's window: %+v", msgs)
	}
}
