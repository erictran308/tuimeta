// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/proto/waWeb"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// webMsg is a message as a history sync carries it.
func webMsg(id string, fromMe bool, participant string, min int, body string) *waHistorySync.HistorySyncMsg {
	key := &waCommon.MessageKey{ID: gproto.String(id), FromMe: gproto.Bool(fromMe)}
	if participant != "" {
		key.Participant = gproto.String(participant)
	}
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key: key, MessageTimestamp: gproto.Uint64(uint64(at(min).Unix())),
		Message: &waE2E.Message{Conversation: gproto.String(body)},
	}}
}

func syncOf(kind waHistorySync.HistorySync_HistorySyncType, convs ...*waHistorySync.Conversation) *events.HistorySync {
	return &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: kind.Enum(), Conversations: convs}}
}

func TestThePhonesSyncFillsTheChatListWithoutReportingOldMessagesAsNew(t *testing.T) {
	h := newHarness(t)
	dm := &waHistorySync.Conversation{
		ID: gproto.String(alicePN.String()), LidJID: gproto.String(aliceLID.String()),
		UnreadCount: gproto.Uint32(1), ConversationTimestamp: gproto.Uint64(uint64(at(-10).Unix())),
		Messages: []*waHistorySync.HistorySyncMsg{
			webMsg("A3", false, "", -10, "and now?"),
			webMsg("A2", true, "", -20, "fine"),
			webMsg("A1", false, "", -30, "how are you"),
		},
	}
	stub := &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: gproto.String("G0"), Participant: gproto.String(benLID.String())},
		MessageTimestamp: gproto.Uint64(uint64(at(-60).Unix())), MessageStubType: waWeb.WebMessageInfo_GROUP_CHANGE_SUBJECT.Enum(),
		MessageStubParameters: []string{"Trip"},
	}}
	group := &waHistorySync.Conversation{
		ID: gproto.String(groupJID.String()), Name: gproto.String("Trip"), Archived: gproto.Bool(true),
		MuteEndTime: gproto.Uint64(uint64(base.Add(time.Hour).Unix())),
		Messages:    []*waHistorySync.HistorySyncMsg{webMsg("G1", false, benLID.String(), -50, "packed?"), stub},
	}
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, dm, group))
	if len(h.rec.messages()) != 0 {
		t.Fatalf("old messages were reported as new: %+v", h.rec.messages())
	}
	h.load()
	chats := map[string]proto.Chat{}
	for _, c := range h.rec.chats() {
		chats[c.Title] = c
	}
	a, g := chats["Alice Example"], chats["Trip"]
	if a.ID == 0 || a.Unread != 1 || a.LastMessage == nil || a.LastMessage.Text != "and now?" || a.Archived {
		t.Fatalf("alice's chat %+v", a)
	}
	if g.ID == 0 || !g.Archived || !g.Muted || g.Kind != proto.Group {
		t.Fatalf("group %+v", g)
	}
	c := h.chat(aliceLID.String())
	page, _ := h.w.History(context.Background(), refOf(c), history.Query{Limit: 50})
	if len(page.Messages) != 3 || page.Messages[1].Text != "fine" || !page.Messages[1].Outgoing {
		t.Fatalf("history %+v", page.Messages)
	}
	if page.Messages[0].ID >= page.Messages[2].ID {
		t.Error("history isn't oldest first")
	}
	gc := h.chat(groupJID.String())
	gpage, _ := h.w.History(context.Background(), refOf(gc), history.Query{Limit: 50})
	if len(gpage.Messages) != 2 || gpage.Messages[0].Service != "~Ben named the group Trip" {
		t.Fatalf("group history %+v", gpage.Messages)
	}
}

func TestOlderMessagesAreAskedOfThePhoneAndTheChatEndsWhenItHasNoMore(t *testing.T) {
	h := newHarness(t)
	h.load()
	HistoryWait = 2 * time.Second
	h.event(withAlt(text(alicePN, alicePN, "A9", -1, "latest"), aliceLID))
	c := h.chat(aliceLID.String())
	// The phone answers the first request with an older message, the
	// second with nothing.
	answers := []*waHistorySync.Conversation{
		{ID: gproto.String(aliceLID.String()), Messages: []*waHistorySync.HistorySyncMsg{webMsg("A8", false, "", -100, "older")}},
		{ID: gproto.String(aliceLID.String())},
	}
	go func() {
		answered := 0
		for range 400 {
			if n := len(h.wa.sentMessages()); n > answered && answered < len(answers) {
				h.event(syncOf(waHistorySync.HistorySync_ON_DEMAND, answers[answered]))
				answered++
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	page, err := h.w.History(context.Background(), refOf(c), history.Query{Limit: 20})
	if err != nil || len(page.Messages) != 2 || page.Messages[0].Text != "older" {
		t.Fatalf("page %+v %v", page.Messages, err)
	}
	reqs := h.wa.sentMessages()
	if len(reqs) != 2 {
		t.Fatalf("asked %d times", len(reqs))
	}
	od := reqs[0].msg.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
	if reqs[0].to != selfPN || !reqs[0].extra.Peer || od.GetOldestMsgID() != "A9" || od.GetChatJID() != aliceLID.String() {
		t.Fatalf("asked %+v (%+v)", reqs[0], od)
	}
	if od := reqs[1].msg.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest(); od.GetOldestMsgID() != "A8" {
		t.Errorf("second request from %q", od.GetOldestMsgID())
	}
	// The phone has nothing before that: the chat is complete, and it isn't
	// asked again.
	if !c.log.Complete() {
		t.Fatal("a chat with nothing older isn't complete")
	}
	h.w.History(context.Background(), refOf(c), history.Query{Limit: 20})
	if got := len(h.wa.sentMessages()); got != 2 {
		t.Errorf("the phone was asked again (%d)", got)
	}
}

func TestAPhoneThatDoesntAnswerIsntAskedAgainAtOnce(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A9", -1, "latest"), aliceLID))
	c := h.chat(aliceLID.String())
	page, err := h.w.History(context.Background(), refOf(c), history.Query{Limit: 20})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("what's here is the answer: %+v %v", page, err)
	}
	h.w.History(context.Background(), refOf(c), history.Query{Limit: 20})
	if n := len(h.wa.sentMessages()); n != 1 {
		t.Errorf("asked %d times", n)
	}
}

func TestReadReceiptsMoveTheInboxAndTheOutbox(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "one"), aliceLID))
	h.event(withAlt(text(alicePN, alicePN, "A2", -4, "two"), aliceLID))
	h.event(text(aliceLID, selfLID, "S1", -3, "mine"))
	c := h.chat(aliceLID.String())
	if c.Unread != 2 {
		t.Fatalf("unread %d", c.Unread)
	}
	h.rec.reset()

	// Read on the phone.
	self := &events.Receipt{MessageIDs: []waTypes.MessageID{"A1", "A2"}, Type: waTypes.ReceiptTypeReadSelf, Timestamp: at(-2)}
	self.Chat, self.Sender, self.IsFromMe = aliceLID, selfLID, true
	h.event(self)
	reads := h.rec.reads()
	if len(reads) != 1 || reads[0].Inbox == nil || *reads[0].Unread != 0 || *reads[0].Inbox>>8 != at(-4).UnixMilli() {
		t.Fatalf("read elsewhere: %+v", reads)
	}
	// Alice read yours.
	theirs := &events.Receipt{MessageIDs: []waTypes.MessageID{"S1"}, Type: waTypes.ReceiptTypeRead, Timestamp: at(-1)}
	theirs.Chat, theirs.Sender = alicePN, alicePN
	h.event(theirs)
	reads = h.rec.reads()
	if len(reads) != 2 || reads[1].Outbox == nil || *reads[1].Outbox>>8 != at(-3).UnixMilli() {
		t.Fatalf("they read: %+v", reads)
	}
	// Delivery receipts say nothing.
	delivered := &events.Receipt{MessageIDs: []waTypes.MessageID{"S1"}, Type: waTypes.ReceiptTypeDelivered}
	delivered.Chat, delivered.Sender = alicePN, alicePN
	h.event(delivered)
	if len(h.rec.reads()) != 2 {
		t.Error("a delivery receipt moved something")
	}
}

func TestChangesMadeOnThePhoneFollowHere(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "hi"), aliceLID))
	h.event(withAlt(text(alicePN, alicePN, "A2", -4, "still there?"), aliceLID))
	h.rec.reset()
	last := func() proto.Chat {
		cs := h.rec.chats()
		if len(cs) == 0 {
			t.Fatal("no chat event")
		}
		return cs[len(cs)-1]
	}
	h.event(&events.Mute{JID: alicePN, Action: &waSyncAction.MuteAction{Muted: gproto.Bool(true), MuteEndTimestamp: gproto.Int64(-1)}})
	if !last().Muted {
		t.Error("not muted")
	}
	h.event(&events.Archive{JID: aliceLID, Action: &waSyncAction.ArchiveChatAction{Archived: gproto.Bool(true)}})
	if !last().Archived {
		t.Error("not archived")
	}
	h.event(&events.MarkChatAsRead{JID: aliceLID, Action: &waSyncAction.MarkChatAsReadAction{Read: gproto.Bool(false)}})
	if last().Unread == 0 {
		t.Error("marked unread shows nothing unread")
	}
	h.event(&events.MarkChatAsRead{JID: aliceLID, Timestamp: at(-1), Action: &waSyncAction.MarkChatAsReadAction{Read: gproto.Bool(true)}})
	if last().Unread != 0 {
		t.Errorf("marked read: %+v", last())
	}
	h.event(&events.DeleteForMe{ChatJID: aliceLID, MessageID: "A1"})
	if d := h.rec.deletions(); len(d) != 1 {
		t.Errorf("deleted for me: %+v", d)
	}
	h.event(&events.DeleteChat{JID: alicePN})
	removed := false
	for _, v := range h.rec.all() {
		if _, ok := v.(proto.ChatRemovedEvent); ok {
			removed = true
		}
	}
	if !removed || h.chat(aliceLID.String()) != nil {
		t.Error("the chat deleted on the phone stayed")
	}
}

func TestGroupChangesBecomeEventsInTheChat(t *testing.T) {
	h := newHarness(t)
	h.load()
	evt := text(groupJID, benLID, "G1", -10, "hi all")
	h.event(evt)
	h.rec.reset()
	sender := benLID
	h.event(&events.GroupInfo{
		JID: groupJID, Sender: &sender, Timestamp: at(-5),
		Name: &waTypes.GroupName{Name: "Book club"},
		Join: []waTypes.JID{aliceLID},
	})
	var sentences []string
	for _, m := range h.rec.messages() {
		sentences = append(sentences, m.Service)
	}
	want := []string{"~Ben named the group Book club", "~Ben added WhatsApp user"}
	if strings.Join(sentences, "|") != strings.Join(want, "|") {
		// Alice is known by her WhatsApp id only here, and her number isn't.
		t.Errorf("events %q", sentences)
	}
	chats := h.rec.chats()
	if len(chats) == 0 || chats[len(chats)-1].Title != "Book club" {
		t.Errorf("chat %+v", chats)
	}
	h.rec.reset()
	h.event(&events.GroupInfo{JID: groupJID, Timestamp: at(-4), Leave: []waTypes.JID{selfLID}})
	chats = h.rec.chats()
	if len(chats) == 0 || chats[len(chats)-1].CanSend {
		t.Error("you left, but can still send")
	}
	if msgs := h.rec.messages(); len(msgs) != 1 || msgs[0].Service != "You left" {
		t.Errorf("leaving: %+v", msgs)
	}
}

func TestSomeoneTypingIsReportedAndYouNeverAre(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "hi"), aliceLID))
	h.rec.reset()
	typing := &events.ChatPresence{State: waTypes.ChatPresenceComposing}
	typing.Chat, typing.Sender, typing.SenderAlt = alicePN, alicePN, aliceLID
	h.event(typing)
	var got []proto.TypingEvent
	for _, v := range h.rec.all() {
		if e, ok := v.(proto.TypingEvent); ok {
			got = append(got, e)
		}
	}
	if len(got) != 1 || !got[0].Typing || got[0].UserID != h.userID(aliceLID.String()) {
		t.Fatalf("typing %+v", got)
	}
	mine := &events.ChatPresence{State: waTypes.ChatPresenceComposing}
	mine.Chat, mine.Sender = alicePN, selfLID
	h.event(mine)
	if n := len(h.rec.all()); n != 1 {
		t.Errorf("your own typing was reported (%d events)", n)
	}
}

func TestTheDeviceBeingUnlinkedSaysToLinkAgain(t *testing.T) {
	h := newHarness(t)
	h.event(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	accs := h.rec.accounts()
	if len(accs) != 1 || accs[0].State != proto.Errored || !strings.Contains(accs[0].Error, "link it again") {
		t.Fatalf("account %+v", accs)
	}
	if _, err := h.w.connected(); err == nil {
		t.Error("still connected")
	}
}
