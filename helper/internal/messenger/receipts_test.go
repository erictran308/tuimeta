// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// waReceiptOf is a receipt as whatsmeow delivers it.
func waReceiptOf(typ waTypes.ReceiptType, chat, from waTypes.JID, fromMe bool, ids ...string) *events.Receipt {
	r := &events.Receipt{MessageIDs: ids, Type: typ}
	r.Chat, r.Sender, r.IsFromMe = chat, from, fromMe
	return r
}

// readState is how far you've read c, and how far receipts went out.
func (h *harness) readState(key int64) (unread int, readUpTo, receipted int64) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	c := h.m.lookupChat(key)
	return c.unread, c.readUpTo, c.receipted
}

func TestSomeoneElsesReadSelfReceiptDoesntMarkTheChatRead(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(waGroup, jid(aliceID), "A1", -2, waText("one"), nil))
	h.wa(waMsg(waGroup, jid(aliceID), "A2", -1, waText("two"), nil))
	msgs := h.rec.messages()
	unread, readUpTo, receipted := h.readState(777)
	h.rec.reset()
	h.wa(waReceiptOf(waTypes.ReceiptTypeReadSelf, waGroup, jid(benID), false, "A2"))
	if len(h.rec.of("read")) != 0 {
		t.Errorf("Ben's read-self receipt read the chat: %+v", h.rec.of("read"))
	}
	if u, r, rc := h.readState(777); u != unread || r != readUpTo || rc != receipted {
		t.Errorf("state moved: unread %d→%d, read %d→%d, receipted %d→%d", unread, u, readUpTo, r, receipted, rc)
	}
	// Reading them here still tells Alice.
	if err := h.m.MarkRead(context.Background(), h.ref(h.chat(777), msgs[len(msgs)-1].ID)); err != nil {
		t.Fatal(err)
	}
	if len(h.e2ee.reads) != 1 || len(h.e2ee.reads[0].ids) != 2 {
		t.Errorf("receipts %+v", h.e2ee.reads)
	}
}

func TestYourOwnReceiptReadsOnlyTheMessagesItNames(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(waGroup, jid(aliceID), "X1", -3, waText("alice's"), nil))
	// Ben reuses Alice's id for a later message.
	h.wa(waMsg(waGroup, jid(benID), "X1", -1, waText("ben's"), nil))
	r := waReceiptOf(waTypes.ReceiptTypeReadSelf, waGroup, fakeOwnJID, true, "X1")
	r.MessageSender = jid(aliceID)
	h.wa(r)
	if _, readUpTo, _ := h.readState(777); readUpTo != ms(-3) {
		t.Errorf("read up to %d, want Alice's message at %d", readUpTo, ms(-3))
	}
	// Someone else's receipt is about your messages only: Ben reading
	// "X1" says nothing about Alice's.
	h.rec.reset()
	h.wa(waReceiptOf(waTypes.ReceiptTypeRead, waGroup, jid(benID), false, "X1"))
	if reads := h.rec.of("read"); len(reads) != 0 {
		t.Errorf("Ben read someone else's message: %+v", reads)
	}
	// Your own device's receipt, grouped (no IsFromMe), still counts.
	h.wa(waMsg(waGroup, jid(aliceID), "X2", 0, waText("more"), nil))
	h.rec.reset()
	h.wa(waReceiptOf(waTypes.ReceiptTypeRead, waGroup, jid(selfID), false, "X2"))
	if reads := h.rec.of("read"); len(reads) != 1 || reads[0].(proto.ReadEvent).Inbox == nil {
		t.Errorf("your own receipt %+v", reads)
	}
}

// readTasks are the Facebook read receipts that went out.
func (h *harness) readTasks() []*socket.ThreadMarkReadTask {
	var out []*socket.ThreadMarkReadTask
	for _, task := range h.meta.sent() {
		if r, ok := task.(*socket.ThreadMarkReadTask); ok {
			out = append(out, r)
		}
	}
	return out
}

func TestReadingAMessageRequestTellsItsSenderNothing(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{
		{ThreadKey: aliceID, ThreadType: table.ONE_TO_ONE, FolderName: "pending", LastActivityTimestampMs: ms(-1)},
		{ThreadKey: benID, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, FolderName: "e2ee_cutover_pending", LastActivityTimestampMs: ms(-1)},
	}})
	h.load()
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$q1", aliceID, -2, "hello stranger")}})
	h.wa(waMsg(jid(benID), jid(benID), "B1", -1, waText("encrypted hello"), nil))
	msgs := h.rec.messages()
	alice, ben := h.chat(aliceID), h.chat(benID)
	if !alice.request || !ben.request {
		t.Fatal("the chats aren't requests")
	}
	ctx := context.Background()
	if err := h.m.MarkRead(ctx, h.ref(alice, msgs[0].ID)); err != nil {
		t.Fatal(err)
	}
	if err := h.m.MarkRead(ctx, h.ref(ben, msgs[1].ID)); err != nil {
		t.Fatal(err)
	}
	if len(h.readTasks()) != 0 || len(h.e2ee.reads) != 0 || len(h.e2ee.selfReads) != 0 {
		t.Errorf("a request's sender was told: %+v %+v", h.readTasks(), h.e2ee.reads)
	}
	if _, readUpTo, _ := h.readState(aliceID); readUpTo != 0 {
		t.Error("the request was marked read")
	}
	// Accepted, it's read as any chat is.
	h.apply(&table.LSTable{LSMoveThreadToInboxAndUpdateParent: []*table.LSMoveThreadToInboxAndUpdateParent{{ThreadKey: aliceID}}})
	if err := h.m.MarkRead(ctx, h.ref(alice, msgs[0].ID)); err != nil {
		t.Fatal(err)
	}
	if len(h.readTasks()) != 1 {
		t.Errorf("reading an accepted request sent %+v", h.readTasks())
	}
}

func TestWithReadReceiptsOffOnlyYourOwnDevicesAreTold(t *testing.T) {
	for name, thread := range map[string]*table.LSTable{
		"flag": {LSUpdateOrInsertThread: []*table.LSUpdateOrInsertThread{{ThreadKey: aliceID, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, IsReadReceiptsDisabled: true}}},
		"v2":   {LSUpdateOrInsertThread: []*table.LSUpdateOrInsertThread{{ThreadKey: aliceID, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, ReadReceiptsDisabledV2: 1}}},
		"full": {LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{{ThreadKey: aliceID, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, FolderName: "inbox", ReadReceiptsDisabledV2: float64(1)}}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.load()
			h.apply(thread)
			// A later description leaving the flag out doesn't turn them on.
			h.apply(&table.LSTable{LSUpdateOrInsertThread: []*table.LSUpdateOrInsertThread{{ThreadKey: aliceID, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE}}})
			h.wa(waMsg(jid(aliceID), jid(aliceID), "V1", -2, waText("one"), nil))
			h.wa(waMsg(jid(aliceID), jid(aliceID), "V2", -1, waText("two"), nil))
			msgs := h.rec.messages()
			c := h.chat(aliceID)
			if err := h.m.MarkRead(context.Background(), h.ref(c, msgs[0].ID)); err != nil {
				t.Fatal(err)
			}
			if err := h.m.MarkRead(context.Background(), h.ref(c, msgs[1].ID)); err != nil {
				t.Fatal(err)
			}
			self := h.e2ee.selfReads
			if len(h.e2ee.reads) != 0 || len(self) != 2 || !slices.Equal(self[0].ids, []string{"V1"}) || !slices.Equal(self[1].ids, []string{"V2"}) {
				t.Errorf("receipts %+v, to your devices %+v", h.e2ee.reads, self)
			}
		})
	}
}

func TestAnEncryptedGroupsChangesArentReadOnFacebook(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(waGroup, jid(aliceID), "A1", -2, waText("hi"), nil))
	alice := jid(aliceID)
	h.wa(&events.GroupInfo{JID: waGroup, Sender: &alice, Timestamp: base, Name: &waTypes.GroupName{Name: "Book club"}})
	msgs := h.rec.messages()
	change := msgs[len(msgs)-1]
	if change.Service == "" {
		t.Fatalf("messages %+v", msgs)
	}
	if err := h.m.MarkRead(context.Background(), h.ref(h.chat(777), change.ID)); err != nil {
		t.Fatal(err)
	}
	if len(h.readTasks()) != 0 {
		t.Errorf("an encrypted group was read on Facebook: %+v", h.readTasks())
	}
	if len(h.e2ee.reads) != 1 || !slices.Equal(h.e2ee.reads[0].ids, []string{"A1"}) {
		t.Errorf("receipts %+v", h.e2ee.reads)
	}
}

func TestAReceiptNamingThousandsOfIdsIsQuick(t *testing.T) {
	h := newHarness(t)
	h.load()
	const kept = 3000
	for i := range kept {
		h.wa(waMsg(jid(aliceID), jid(selfID), "S"+strconv.Itoa(i), i-kept, waText("x"), nil))
	}
	ids := make([]string, 0, 1<<16)
	for i := range 1<<16 - 1 {
		ids = append(ids, "nothing-"+strconv.Itoa(i))
	}
	ids = append(ids, "S"+strconv.Itoa(kept-1))
	h.rec.reset()
	start := time.Now()
	h.wa(waReceiptOf(waTypes.ReceiptTypeRead, jid(aliceID), jid(aliceID), false, ids...))
	took := time.Since(start)
	reads := h.rec.of("read")
	if len(reads) != 1 || reads[0].(proto.ReadEvent).Outbox == nil || *reads[0].(proto.ReadEvent).Outbox != positionOf(ms(-1)) {
		t.Fatalf("reads %+v", reads)
	}
	// Each id is looked up, not compared with every message (that took
	// over a second here).
	if took > 250*time.Millisecond {
		t.Errorf("a receipt held the backend for %v", took)
	}
}
