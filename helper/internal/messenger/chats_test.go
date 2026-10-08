// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestLoadChatsPagesNewestFirstAndAsksForOlderThreads(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{
		dmThread(aliceID, -5, -60, 2),
		groupThread(groupID, "Trip ⛺", -1),
		{ThreadKey: 555, ThreadType: table.ONE_TO_ONE, FolderName: "spam", LastActivityTimestampMs: ms(0)},
	}})
	// Messenger has one more, older page.
	h.meta.pages = []*table.LSTable{{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(benID, -300, -300, 0)}}}

	more, err := h.m.LoadChats(context.Background(), 1)
	if err != nil || !more {
		t.Fatalf("first page: %v %v", more, err)
	}
	chats := h.rec.chats()
	if len(chats) != 1 || chats[0].Title != "Trip ⛺" || chats[0].Kind != proto.Group {
		t.Fatalf("first page %+v", chats)
	}
	h.rec.reset()
	more, err = h.m.LoadChats(context.Background(), 5)
	if err != nil || more {
		t.Fatalf("second page: %v %v", more, err)
	}
	chats = h.rec.chats()
	if len(chats) != 2 || chats[0].Title != "Alice Example" || chats[1].Title != "Ben Carter" {
		t.Fatalf("second page %+v", chats)
	}
	alice := chats[0]
	if alice.Kind != proto.DM || alice.UserID != h.userID(aliceID) || alice.Unread != 2 || alice.Order != ms(-5) ||
		alice.ReadInbox != ids.MessageID(ms(-60), ids.MaxSlot) || alice.Photo == nil || alice.Network != proto.Messenger || !alice.CanSend {
		t.Errorf("alice's chat %+v", alice)
	}
	// The people in a chat are told before it.
	users := h.rec.users()
	if !slices.ContainsFunc(users, func(u proto.User) bool {
		return u.ID == h.userID(aliceID) && u.Name == "Alice Example" && u.Username == "alice.example"
	}) {
		t.Errorf("users %+v", users)
	}
	// Spam never shows.
	for _, c := range append(h.rec.chats(), chats...) {
		if c.ID == h.chat(555).id {
			t.Error("a spam thread was sent")
		}
	}
}

func TestANewChatAfterTheListLoadedIsSentAtOnce(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -5, -5, 0)}})
	h.load()
	h.rec.reset()
	h.apply(&table.LSTable{
		LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(benID, 0, -1, 1)},
		LSInsertMessage:          []*table.LSInsertMessage{textMsg(benID, "mid.$new", benID, 0, "hey")},
	})
	chats := h.rec.chats()
	if len(chats) != 1 || chats[0].Title != "Ben Carter" || chats[0].Unread != 1 {
		t.Fatalf("chats %+v", chats)
	}
	// The message came before the chat, as one change.
	all := h.rec.all()
	mi := slices.IndexFunc(all, func(v any) bool { _, ok := v.(proto.MessageEvent); return ok })
	ci := slices.IndexFunc(all, func(v any) bool { _, ok := v.(proto.ChatEvent); return ok })
	if mi < 0 || ci < mi {
		t.Errorf("order: message %d chat %d", mi, ci)
	}
}

func TestChatStateComesFromTheSocket(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{groupThread(groupID, "", -5)}})
	h.apply(&table.LSTable{LSAddParticipantIdToGroupThread: []*table.LSAddParticipantIdToGroupThread{
		{ThreadKey: groupID, ContactId: aliceID}, {ThreadKey: groupID, ContactId: benID}, {ThreadKey: groupID, ContactId: selfID},
	}})
	h.load()
	if got := h.rec.chats(); got[len(got)-1].Title != "Alice, Ben" {
		t.Errorf("title from members %q", got[len(got)-1].Title)
	}
	last := func() proto.Chat { c := h.rec.chats(); return c[len(c)-1] }
	h.apply(&table.LSTable{LSUpdateThreadMuteSetting: []*table.LSUpdateThreadMuteSetting{{ThreadKey: groupID, MuteExpireTimeMS: -1}}})
	if !last().Muted {
		t.Error("not muted")
	}
	h.apply(&table.LSTable{LSUpdateThreadMuteSetting: []*table.LSUpdateThreadMuteSetting{{ThreadKey: groupID, MuteExpireTimeMS: ms(-1)}}})
	if last().Muted {
		t.Error("a mute that ended still counts")
	}
	h.apply(&table.LSTable{LSSyncUpdateThreadName: []*table.LSSyncUpdateThreadName{{ThreadKey: groupID, ThreadName: "Weekend"}}})
	if last().Title != "Weekend" {
		t.Error("rename")
	}
	h.apply(&table.LSTable{LSMoveThreadToArchivedFolder: []*table.LSMoveThreadToArchivedFolder{{ThreadKey: groupID}}})
	if !last().Archived {
		t.Error("archive")
	}
	h.apply(&table.LSTable{LSDeleteThenInsertMessageRequest: []*table.LSDeleteThenInsertMessageRequest{{ThreadKey: groupID, MessageRequestStatus: 1}}})
	if !last().Request {
		t.Error("request")
	}
	h.apply(&table.LSTable{LSMoveThreadToE2EECutoverFolder: []*table.LSMoveThreadToE2EECutoverFolder{{ThreadKey: groupID}}})
	if !last().Encrypted {
		t.Error("cutover")
	}
	h.rec.reset()
	h.apply(&table.LSTable{LSUpdateTypingIndicator: []*table.LSUpdateTypingIndicator{{ThreadKey: groupID, SenderId: aliceID, IsTyping: true}, {ThreadKey: groupID, SenderId: selfID, IsTyping: true}}})
	typing := h.rec.of("typing")
	if len(typing) != 1 || typing[0].(proto.TypingEvent).UserID != h.userID(aliceID) || !typing[0].(proto.TypingEvent).Typing {
		t.Errorf("typing %+v", typing)
	}
	h.rec.reset()
	h.apply(&table.LSTable{LSRemoveParticipantFromThread: []*table.LSRemoveParticipantFromThread{{ThreadKey: groupID, ParticipantId: selfID}}})
	if removed := h.rec.of("chat_removed"); len(removed) != 1 {
		t.Errorf("leaving the group: %+v", h.rec.all())
	}
}

func TestReadPositionsComeAsReadEvents(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{
		textMsg(aliceID, "mid.$1", selfID, -9, "mine"), textMsg(aliceID, "mid.$2", aliceID, -8, "one"), textMsg(aliceID, "mid.$3", aliceID, -7, "two"),
	}})
	if c := h.rec.chats(); c[len(c)-1].Unread != 2 {
		t.Fatalf("unread %d", c[len(c)-1].Unread)
	}
	h.rec.reset()
	// Read on another device, up to the first of the two.
	h.apply(&table.LSTable{LSMarkThreadReadV2: []*table.LSMarkThreadReadV2{{ThreadKey: aliceID, LastReadWatermarkTimestampMs: ms(-8)}}})
	reads := h.rec.of("read")
	if len(reads) != 1 {
		t.Fatalf("reads %+v", reads)
	}
	r := reads[0].(proto.ReadEvent)
	if r.Inbox == nil || *r.Inbox != ids.MessageID(ms(-8), ids.MaxSlot) || r.Unread == nil || *r.Unread != 1 || r.Outbox != nil {
		t.Errorf("read %+v", r)
	}
	h.rec.reset()
	h.apply(&table.LSTable{LSUpdateReadReceipt: []*table.LSUpdateReadReceipt{{ThreadKey: aliceID, ContactId: aliceID, ReadWatermarkTimestampMs: ms(-9)}}})
	reads = h.rec.of("read")
	if len(reads) != 1 || reads[0].(proto.ReadEvent).Outbox == nil || *reads[0].(proto.ReadEvent).Outbox != ids.MessageID(ms(-9), ids.MaxSlot) {
		t.Errorf("their read %+v", reads)
	}
}

func TestAnEncryptedGroupsFacebookThreadIsTheSameChat(t *testing.T) {
	h := newHarness(t)
	const fbKey, waKey int64 = 123, 777
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{groupThread(fbKey, "Book club", -20)}})
	h.load()
	old := h.chat(fbKey)
	h.rec.reset()
	h.apply(&table.LSTable{LSVerifyHybridThreadExists: []*table.LSVerifyHybridThreadExists{{ThreadKey: fbKey, ThreadJID: waKey, ThreadType: table.ENCRYPTED_OVER_WA_GROUP, LastActivityTimestampMS: ms(-2)}}})
	c := h.chat(fbKey)
	if c == nil || c.key != waKey || c.fbKey != fbKey || !c.encrypted || c.name != "Book club" || c.jid().String() != "777@g.us" {
		t.Fatalf("chat %+v", c)
	}
	if removed := h.rec.of("chat_removed"); len(removed) != 1 || removed[0].(proto.ChatRemovedEvent).ChatID != old.id {
		t.Errorf("the Facebook copy wasn't removed: %+v", removed)
	}
	// Later updates by the Facebook key land on the encrypted chat.
	h.apply(&table.LSTable{LSSyncUpdateThreadName: []*table.LSSyncUpdateThreadName{{ThreadKey: fbKey, ThreadName: "Books"}}})
	if h.chat(waKey).name != "Books" || h.chat(waKey).threadKey() != fbKey {
		t.Error("rename by the Facebook key")
	}
}

func TestHistoryPagesFromWhatsKnownAndAsksForOlder(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, 0, 0, 0)}})
	// The initial sync brought the newest three, with more before them.
	var recent []*table.LSUpsertMessage
	for i := 0; i < 3; i++ {
		recent = append(recent, &table.LSUpsertMessage{ThreadKey: aliceID, MessageId: "mid.$n" + string(rune('a'+i)), SenderId: aliceID, TimestampMs: ms(-3 + i), Text: "new"})
	}
	h.apply(&table.LSTable{
		LSInsertNewMessageRange: []*table.LSInsertNewMessageRange{{ThreadKey: aliceID, MinTimestampMs: ms(-3), MaxTimestampMs: ms(-1), HasMoreBefore: true}},
		LSUpsertMessage:         recent,
	})
	h.load()
	c := h.chat(aliceID)
	ref := backend.ChatRef{ID: c.id, Network: proto.Messenger, NetID: c.netID()}

	// Older messages come in the answer to the fetch.
	h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
		f, ok := tasks[0].(*socket.FetchMessagesTask)
		if !ok {
			return &table.LSTable{}, nil
		}
		if f.ThreadKey != aliceID || f.ReferenceMessageId != "mid.$na" || f.ReferenceTimestampMs != ms(-3) {
			t.Errorf("fetch %+v", f)
		}
		var older []*table.LSUpsertMessage
		for i := 0; i < 4; i++ {
			older = append(older, &table.LSUpsertMessage{ThreadKey: aliceID, MessageId: "mid.$o" + string(rune('a'+i)), SenderId: selfID, TimestampMs: ms(-20 + i), Text: "old"})
		}
		return &table.LSTable{
			LSInsertNewMessageRange: []*table.LSInsertNewMessageRange{{ThreadKey: aliceID, MinTimestampMs: ms(-20), MaxTimestampMs: ms(-17), HasMoreBefore: false}},
			LSUpsertMessage:         older,
		}, nil
	}
	newest, err := h.m.History(context.Background(), ref, history.Query{Limit: 2})
	if err != nil || len(newest.Messages) != 2 || !newest.HasMore || newest.Messages[1].ID <= newest.Messages[0].ID {
		t.Fatalf("newest: %+v %v", newest, err)
	}
	if len(h.meta.sent()) != 0 {
		t.Error("a page that's there asked Messenger")
	}
	before := newest.Messages[0].ID
	page, err := h.m.History(context.Background(), ref, history.Query{Before: &before, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 5 || page.HasMore || page.Messages[0].Text != "old" || page.Messages[4].ID >= before {
		t.Fatalf("older page: %d %v %+v", len(page.Messages), page.HasMore, page.Messages)
	}
	if n := len(h.meta.sent()); n != 1 {
		t.Errorf("fetches %d", n)
	}
	// The history pages were never reported as new messages.
	if len(h.rec.messages()) != 0 {
		t.Errorf("history reported as new: %d", len(h.rec.messages()))
	}
	after := page.Messages[1].ID
	later, _ := h.m.History(context.Background(), ref, history.Query{After: &after, Limit: 3})
	if len(later.Messages) != 3 || later.Messages[0].ID <= after || !later.HasMore {
		t.Errorf("after: %+v", later)
	}
	around := page.Messages[2].ID
	mid, _ := h.m.History(context.Background(), ref, history.Query{Around: &around, Limit: 4})
	if len(mid.Messages) != 4 || !slices.ContainsFunc(mid.Messages, func(m proto.Message) bool { return m.ID == around }) {
		t.Errorf("around: %+v", mid)
	}
	got, err := h.m.GetMessage(context.Background(), h.ref(c, around))
	if err != nil || got.ID != around {
		t.Errorf("get_message %+v %v", got, err)
	}
}

func TestAGapAfterAReconnectIsReportedAsNew(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$k", aliceID, -10, "known")}})
	h.load()
	h.rec.reset()
	h.apply(&table.LSTable{
		LSInsertNewMessageRange: []*table.LSInsertNewMessageRange{{ThreadKey: aliceID, HasMoreBefore: true}},
		LSUpsertMessage: []*table.LSUpsertMessage{
			{ThreadKey: aliceID, MessageId: "mid.$k", SenderId: aliceID, TimestampMs: ms(-10), Text: "known"},
			{ThreadKey: aliceID, MessageId: "mid.$missed", SenderId: aliceID, TimestampMs: ms(-1), Text: "while away"},
		},
	})
	got := h.rec.messages()
	if len(got) != 1 || got[0].Text != "while away" {
		t.Errorf("gap: %+v", got)
	}
}

func TestAnUnknownSenderIsAskedForOnce(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{groupThread(groupID, "Trip", -10)}})
	h.load()
	h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
		if g, ok := tasks[0].(*socket.GetContactsFullTask); ok {
			return &table.LSTable{LSDeleteThenInsertContact: []*table.LSDeleteThenInsertContact{{Id: g.ContactID, Name: "Chloé Martin"}}}, nil
		}
		return &table.LSTable{}, nil
	}
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(groupID, "mid.$c1", 4242, -2, "hi"), textMsg(groupID, "mid.$c2", 4242, -1, "again")}})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if slices.ContainsFunc(h.rec.users(), func(u proto.User) bool { return u.Name == "Chloé Martin" }) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	users := h.rec.users()
	if !slices.ContainsFunc(users, func(u proto.User) bool { return u.Name == "Chloé Martin" }) {
		t.Fatalf("users %+v", users)
	}
	asked := 0
	for _, task := range h.meta.sent() {
		if _, ok := task.(*socket.GetContactsFullTask); ok {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times", asked)
	}
}
