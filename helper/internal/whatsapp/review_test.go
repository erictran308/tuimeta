// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/proto/waWeb"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestAMessageToABroadcastListYoureOnLandsInTheChatWithItsSender(t *testing.T) {
	h := newHarness(t)
	h.load()
	list := waTypes.NewJID("1700000000", waTypes.BroadcastServer)
	h.event(text(list, benLID, "B1", -2, "to everyone on my list"))
	c := h.chat(benLID.String())
	if c == nil || len(c.msgs) != 1 {
		t.Fatalf("ben's chat %+v", c)
	}
	// One you sent to a list of yours has no chat here.
	h.rec.reset()
	h.event(text(list, selfLID, "S1", -1, "my list"))
	if len(h.rec.messages()) != 0 {
		t.Errorf("your broadcast became a message: %+v", h.rec.messages())
	}
}

func revokeIn(chat, sender waTypes.JID, target string, min int) *events.Message {
	evt := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: gproto.String(target)},
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = chat, sender, "X"+target+sender.User, at(min)
	evt.Info.IsGroup = chat.Server == waTypes.GroupServer
	return evt
}

func TestInAGroupOnlyItsSenderOrAnAdminDeletesAMessage(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(groupJID, benLID, "G1", -5, "mine"))
	h.event(revokeIn(groupJID, aliceLID, "G1", -4))
	if len(h.rec.deletions()) != 0 {
		t.Fatal("a member who isn't an admin deleted someone else's message")
	}
	c := h.chat(groupJID.String())
	h.w.mu.Lock()
	c.Admins = []string{aliceLID.String()}
	h.w.mu.Unlock()
	h.event(revokeIn(groupJID, aliceLID, "G1", -3))
	if len(h.rec.deletions()) != 1 {
		t.Fatal("an admin couldn't delete it")
	}
}

func TestSomeoneElsesMessageWithTheSameIdNeverTakesAMessagesPlace(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(groupJID, benLID, "G1", -5, "the real one"))
	h.event(text(groupJID, aliceLID, "G1", -4, "a forgery"))
	c := h.chat(groupJID.String())
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	if m := c.msgs["G1"]; m == nil || m.Text != "the real one" || m.Sender != benLID.String() {
		t.Fatalf("message %+v", m)
	}
}

func TestASendThatFailsPartwaySaysWhichFilesWentAndWhichDidnt(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -5, "send them"))
	c := h.chat(benLID.String())
	files := []backend.Upload{
		{Name: "a.pdf", Mime: "application/pdf", Kind: proto.FileMedia, Data: []byte("%PDF a")},
		{Name: "b.pdf", Mime: "application/pdf", Kind: proto.FileMedia, Data: []byte("%PDF b")},
	}
	h.rec.reset()
	// The second send fails.
	wrapped := &failSecond{fakeWA: h.wa}
	h.w.mu.Lock()
	h.w.cli = wrapped
	h.w.mu.Unlock()
	out := h.outgoing(c, "", nil, files...)
	if err := h.w.Send(context.Background(), out); err == nil {
		t.Fatal("no error")
	}
	var sent, failed int
	for _, v := range h.rec.all() {
		switch v.(type) {
		case proto.MessageSentEvent:
			sent++
		case proto.MessageFailedEvent:
			failed++
		case proto.MessageDeletedEvent:
			t.Error("a file that didn't go out was reported deleted")
		}
	}
	if sent != 1 || failed != 1 {
		t.Errorf("sent %d, failed %d", sent, failed)
	}
}

// failSecond is a connection whose second send fails.
type failSecond struct {
	*fakeWA
	n int
}

func (f *failSecond) SendMessage(ctx context.Context, to waTypes.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.n++
	if f.n == 2 {
		return whatsmeow.SendResponse{}, context.DeadlineExceeded
	}
	return f.fakeWA.SendMessage(ctx, to, msg, extra...)
}

func TestMarkingAChatReadOnThePhoneEndsItsBeingMarkedUnread(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -5, "hi"))
	h.event(&events.MarkChatAsRead{JID: benLID, Timestamp: at(-4), Action: markRead(true)})
	h.event(&events.MarkChatAsRead{JID: benLID, Timestamp: at(-3), Action: markRead(false)})
	h.rec.reset()
	h.event(&events.MarkChatAsRead{JID: benLID, Timestamp: at(-2), Action: markRead(true)})
	reads := h.rec.reads()
	if len(reads) != 1 || *reads[0].Unread != 0 {
		t.Fatalf("reads %+v", reads)
	}
	if c := h.chat(benLID.String()); c.MarkedUnread {
		t.Error("still marked unread")
	}
}

func TestAMessageThatCouldntBeKeptIsntAcknowledged(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.w.st.db.Close()
	h.w.mu.Lock()
	gen := h.w.gen
	h.w.mu.Unlock()
	if h.w.onEvent(gen, text(benLID, benLID, "B1", -1, "lost?")) {
		t.Error("acknowledged a message that wasn't kept")
	}
}

func TestPeopleKeptByNumberAreStillThemselvesInTheNextRun(t *testing.T) {
	h := newHarness(t)
	h.load()
	// Alice's message comes by number, before WhatsApp says who she is.
	h.event(text(groupJID, alicePN, "G1", -5, "by number"))
	h.event(withAlt(text(groupJID, alicePN, "G2", -4, "by id"), aliceLID))
	aliceID := h.userID(aliceLID.String())

	h2 := h.restart()
	c := h2.chat(groupJID.String())
	page, err := h2.w.History(context.Background(), refOf(c), history.Query{Limit: 10})
	if err != nil || len(page.Messages) != 2 {
		t.Fatalf("history %+v %v", page, err)
	}
	for _, m := range page.Messages {
		if m.SenderID != aliceID {
			t.Errorf("%q is from user %d, not alice's %d", m.Text, m.SenderID, aliceID)
		}
	}
}

func TestTwoChatsWithOnePersonBecomeOne(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(alicePN, alicePN, "A1", -5, "by number"))
	h.event(text(aliceLID, aliceLID, "A2", -4, "by id"))
	byNumber, byID := h.chat(alicePN.String()), h.chat(aliceLID.String())
	if byNumber == byID {
		t.Fatal("already one chat")
	}
	h.event(withAlt(text(alicePN, alicePN, "A3", -3, "both"), aliceLID))
	c := h.chat(aliceLID.String())
	if c.id != byID.id || len(c.msgs) != 3 {
		t.Fatalf("chat %d with %d messages", c.id, len(c.msgs))
	}
	removed := false
	for _, v := range h.rec.all() {
		if e, ok := v.(proto.ChatRemovedEvent); ok && e.ChatID == byNumber.id {
			removed = true
		}
	}
	if !removed {
		t.Error("the chat by number stayed in the list")
	}
}

func TestMessagesOfOneSecondKeepTheirOrderAndNoneComesFromTheFuture(t *testing.T) {
	h := newHarness(t)
	same := func(id, body string) *waHistorySync.HistorySyncMsg { return webMsg(id, false, "", -10, body) }
	future := webMsg("F", false, "", 60*24*365, "from the future")
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, &waHistorySync.Conversation{
		ID: gproto.String(benLID.String()),
		// Newest first, as the phone lists them.
		Messages: []*waHistorySync.HistorySyncMsg{future, same("Z3", "third"), same("A2", "second"), same("M1", "first")},
	}))
	check := func(h *harness) {
		t.Helper()
		c := h.chat(benLID.String())
		page, _ := h.w.History(context.Background(), refOf(c), history.Query{Limit: 10})
		var texts []string
		for _, m := range page.Messages {
			texts = append(texts, m.Text)
		}
		if strings.Join(texts, ",") != "first,second,third,from the future" {
			t.Errorf("order %q", texts)
		}
		if last := page.Messages[len(page.Messages)-1]; last.Date > base.Unix() {
			t.Errorf("a message from %d, after now", last.Date)
		}
	}
	check(h)
	check(h.restart())
}

func TestAConnectionWhatsAppEndedForGoodIsntReady(t *testing.T) {
	for name, evt := range map[string]any{
		"replaced": &events.StreamReplaced{},
		"banned":   &events.TemporaryBan{Code: events.TempBanBlockedByUsers, Expire: time.Hour},
		"outdated": &events.ClientOutdated{},
	} {
		h := newHarness(t)
		h.event(evt)
		accs := h.rec.accounts()
		if len(accs) != 1 || accs[0].State != proto.Errored || accs[0].Error == "" {
			t.Errorf("%s: accounts %+v", name, accs)
		}
		if _, err := h.w.connected(); err == nil {
			t.Errorf("%s: still connected", name)
		}
	}
}

func TestADownloadNeverGrowsPastItsLimit(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "download-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	capped := &cappedFile{File: f, limit: 10}
	if _, err := io.Copy(capped, bytes.NewReader(make([]byte, 11))); !errors.Is(err, errTooBig) {
		t.Errorf("copy: %v", err)
	}
	if _, err := capped.WriteAt([]byte("x"), 10); !errors.Is(err, errTooBig) {
		t.Errorf("write at: %v", err)
	}
	if err := capped.Truncate(11); !errors.Is(err, errTooBig) {
		t.Errorf("truncate: %v", err)
	}
	if info, _ := f.Stat(); info.Size() > 10 {
		t.Errorf("size %d", info.Size())
	}
	h := newHarness(t)
	h.w.mu.Lock()
	md := h.w.mediaOf(&media{Kind: proto.FileMedia, DirectPath: "/x", MediaKey: []byte("k"), FileEncSHA256: []byte("e")})
	h.w.mu.Unlock()
	if md.FileID != 0 {
		t.Error("a file whatsmeow can't check was offered for download")
	}
}

func TestMessagesThatDisappearedWhileTuimetaWasClosedAreGone(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -5, "short-lived"))
	c := h.chat(benLID.String())
	h.w.mu.Lock()
	c.msgs["B1"].Expires = at(-1).UnixMilli()
	h.w.store(c, c.msgs["B1"])
	h.w.mu.Unlock()
	h2 := h.restart()
	kept, _ := h2.w.st.messages(context.Background(), benLID.String())
	if len(kept) != 0 {
		t.Errorf("kept %d", len(kept))
	}
}

func TestALateSyncBlobDoesntUndoWhatChangedSince(t *testing.T) {
	h := newHarness(t)
	h.load()
	conv := func() *waHistorySync.Conversation {
		return &waHistorySync.Conversation{
			ID: gproto.String(benLID.String()), UnreadCount: gproto.Uint32(1),
			Messages: []*waHistorySync.HistorySyncMsg{webMsg("B2", false, "", -10, "two"), webMsg("B1", false, "", -20, "one")},
		}
	}
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, conv()))
	c := h.chat(benLID.String())
	if err := h.w.MarkRead(context.Background(), h.ref(c, "B2")); err != nil {
		t.Fatal(err)
	}
	late := conv()
	late.Archived = gproto.Bool(true)
	h.event(syncOf(waHistorySync.HistorySync_RECENT, late))
	if c.Unread != 0 || c.Archived {
		t.Errorf("unread %d, archived %v", c.Unread, c.Archived)
	}
	reads := len(h.wa.reads)
	h.w.MarkRead(context.Background(), h.ref(c, "B2"))
	if len(h.wa.reads) != reads {
		t.Error("receipts went out again")
	}
}

func TestEventsOfOneSecondAreKeptApart(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(groupJID, benLID, "G1", -10, "hi"))
	sender := benLID
	h.event(&events.GroupInfo{JID: groupJID, Sender: &sender, Timestamp: at(-5), Name: &waTypes.GroupName{Name: "One"}, Topic: &waTypes.GroupTopic{Topic: "x"}})
	c := h.chat(groupJID.String())
	h.w.mu.Lock()
	n := len(c.msgs)
	h.w.mu.Unlock()
	if n != 3 {
		t.Errorf("%d messages, want the message and two events", n)
	}
}

func TestAMissedCallInADmIsFromTheOtherPerson(t *testing.T) {
	h := newHarness(t)
	call := &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: gproto.String("C1"), RemoteJID: gproto.String(benLID.String())},
		MessageTimestamp: gproto.Uint64(uint64(at(-3).Unix())), MessageStubType: waWeb.WebMessageInfo_CALL_MISSED_VOICE.Enum(),
	}}
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, &waHistorySync.Conversation{
		ID: gproto.String(benLID.String()), Messages: []*waHistorySync.HistorySyncMsg{call},
	}))
	c := h.chat(benLID.String())
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	if m := c.msgs["C1"]; m == nil || m.Sender != benLID.String() {
		t.Fatalf("call %+v", m)
	}
	if _, ok := h.w.people[""]; ok {
		t.Error("a person with no id was made")
	}
}

func TestTheStoreFolderHoldsNoDownloadLeftovers(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -1, "x"))
	dir, _ := h.deps.Session.Dir()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "download-") {
			t.Errorf("left %s", filepath.Join(dir, e.Name()))
		}
	}
}

func markRead(read bool) *waSyncAction.MarkChatAsReadAction {
	return &waSyncAction.MarkChatAsReadAction{Read: gproto.Bool(read)}
}
