// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"strconv"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// outgoing starts a send as the server would: temporary ids, the pending
// message already reported.
func (h *harness) outgoing(c *chat, text string, files []backend.Upload, reply *backend.MessageRef) *backend.Outgoing {
	n := max(len(files), 1)
	temps := h.deps.Messages.Temp(c.id, n)
	return h.deps.Outbox.New(backend.ChatRef{ID: c.id, Network: proto.Messenger, NetID: c.netID()}, temps, text, reply, files)
}

func sendTask(t *testing.T, tasks []socket.Task) *socket.SendMessageTask {
	t.Helper()
	var send *socket.SendMessageTask
	for _, task := range tasks {
		switch tt := task.(type) {
		case *socket.SendMessageTask:
			send = tt
		case *socket.ThreadMarkReadTask:
			t.Error("sending also marked the chat read")
		}
	}
	if send == nil {
		t.Fatal("no send task")
	}
	return send
}

func TestSendingTextIsConfirmedByTheAnswer(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$q", aliceID, -5, "lunch?")}})
	c := h.chat(aliceID)
	q := h.rec.messages()[0]
	reply := h.ref(c, q.ID)
	h.rec.reset()
	h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
		send := sendTask(t, tasks)
		if send.ThreadId != aliceID || send.Text != "*yes*" || send.SendType != table.TEXT || send.ReplyMetaData == nil || send.ReplyMetaData.ReplyMessageId != "mid.$q" {
			t.Errorf("send %+v", send)
		}
		otid := strconv.FormatInt(send.Otid, 10)
		in := textMsg(aliceID, "mid.$sent", selfID, 1, "*yes*")
		in.OfflineThreadingId = otid
		in.ReplySourceId = "mid.$q"
		return &table.LSTable{
			LSReplaceOptimsiticMessage: []*table.LSReplaceOptimsiticMessage{{OfflineThreadingId: otid, MessageId: "mid.$sent"}},
			LSInsertMessage:            []*table.LSInsertMessage{in},
		}, nil
	}
	out := h.outgoing(c, "*yes*", nil, &reply)
	if err := h.m.Send(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	sent := h.rec.of("message_sent")
	if len(sent) != 1 {
		t.Fatalf("sent %+v", h.rec.all())
	}
	e := sent[0].(proto.MessageSentEvent)
	if e.OldID != out.TempIDs[0] || e.Message.Text != "yes" || ids.Millis(e.Message.ID) != ms(1) || !e.Message.Outgoing || e.Message.ReplyTo == nil || e.Message.ReplyTo.MessageID != q.ID {
		t.Errorf("message_sent %+v", e)
	}
	if len(h.rec.messages()) != 0 {
		t.Error("the confirmation was also a new message")
	}
}

func TestASendConfirmedOnTheSocketFirstIsConfirmedOnce(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	c := h.chat(aliceID)
	h.rec.reset()
	h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
		otid := strconv.FormatInt(sendTask(t, tasks).Otid, 10)
		// The socket brings the message before the answer comes.
		in := textMsg(aliceID, "mid.$s2", selfID, 1, "hi")
		in.OfflineThreadingId = otid
		h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{in}})
		return &table.LSTable{LSReplaceOptimsiticMessage: []*table.LSReplaceOptimsiticMessage{{OfflineThreadingId: otid, MessageId: "mid.$s2"}}}, nil
	}
	if err := h.m.Send(context.Background(), h.outgoing(c, "hi", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.rec.of("message_sent")); n != 1 {
		t.Errorf("confirmed %d times", n)
	}
}

func TestAMessageMessengerRefusesFails(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	c := h.chat(aliceID)
	h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
		otid := strconv.FormatInt(sendTask(t, tasks).Otid, 10)
		return &table.LSTable{LSMarkOptimisticMessageFailed: []*table.LSMarkOptimisticMessageFailed{{OTID: otid, Message: "secret server words"}}}, nil
	}
	err := h.m.Send(context.Background(), h.outgoing(c, "hi", nil, nil))
	pe, ok := err.(*proto.Error)
	if !ok || pe.Code != proto.NetworkError || pe.Message == "secret server words" {
		t.Errorf("err %v", err)
	}
}

func TestSendingFilesUploadsThemAsOneMessage(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	c := h.chat(aliceID)
	h.rec.reset()
	h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
		send := sendTask(t, tasks)
		if send.SendType != table.MEDIA || len(send.AttachmentFBIds) != 2 || send.Text != "caption" {
			t.Errorf("send %+v", send)
		}
		otid := strconv.FormatInt(send.Otid, 10)
		return &table.LSTable{LSReplaceOptimsiticMessage: []*table.LSReplaceOptimsiticMessage{{OfflineThreadingId: otid, MessageId: "mid.$files"}}}, nil
	}
	files := []backend.Upload{
		{Name: "a.png", Mime: "image/png", Kind: proto.Photo, Data: []byte("png-bytes"), Width: 10, Height: 20},
		{Name: "notes.pdf", Mime: "application/pdf", Kind: proto.FileMedia, Data: []byte("%PDF")},
	}
	out := h.outgoing(c, "caption", files, nil)
	if err := h.m.Send(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	if len(h.meta.uploads) != 2 || h.meta.uploads[0].Filename != "a.png" || string(h.meta.uploads[1].MediaData) != "%PDF" {
		t.Errorf("uploads %+v", h.meta.uploads)
	}
	sent := h.rec.of("message_sent")
	if len(sent) != 2 {
		t.Fatalf("sent %+v", sent)
	}
	first, last := sent[0].(proto.MessageSentEvent).Message, sent[1].(proto.MessageSentEvent).Message
	if first.Media == nil || first.Media.FileID == 0 || first.Album != first.ID || last.Text != "caption" || last.Media.Name != "notes.pdf" {
		t.Errorf("parts %+v / %+v", first, last)
	}
	// What was sent can be shown from the copy read for sending.
	var buf writeBuf
	ref, _ := h.deps.Files.Get(first.Media.FileID)
	if err := h.m.Fetch(context.Background(), ref, &buf); err != nil || string(buf) != "png-bytes" {
		t.Errorf("fetch %q %v", buf, err)
	}
}

type writeBuf []byte

func (w *writeBuf) Write(p []byte) (int, error) { *w = append(*w, p...); return len(p), nil }

func TestEditDeleteReactAndMuteSendTheirTasks(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$mine", selfID, -5, "helo"), textMsg(aliceID, "mid.$old", selfID, -60, "old")}})
	c := h.chat(aliceID)
	msgs := h.rec.messages() // in time order: the old one first
	old, mine := h.ref(c, msgs[0].ID), h.ref(c, msgs[1].ID)
	ctx := context.Background()

	h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
		if e, ok := tasks[0].(*socket.EditMessageTask); ok {
			return &table.LSTable{LSEditMessage: []*table.LSEditMessage{{MessageID: e.MessageID, Text: e.Text, EditCount: 1}}}, nil
		}
		return &table.LSTable{}, nil
	}
	if err := h.m.EditText(ctx, mine, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := h.m.EditText(ctx, old, "late"); err == nil {
		t.Error("a message older than 15 minutes was edited")
	}
	if err := h.m.React(ctx, mine, "👍🏽"); err != nil {
		t.Fatal(err)
	}
	if err := h.m.React(ctx, mine, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Mute(ctx, backend.ChatRef{ID: c.id}, true); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Delete(ctx, mine); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, task := range h.meta.sent() {
		switch tt := task.(type) {
		case *socket.EditMessageTask:
			kinds = append(kinds, "edit:"+tt.Text)
		case *socket.SendReactionTask:
			if tt.ActorID != selfID || tt.ThreadKey != aliceID {
				t.Errorf("reaction %+v", tt)
			}
			kinds = append(kinds, "react:"+tt.Reaction)
		case *socket.MuteThreadTask:
			kinds = append(kinds, "mute:"+strconv.FormatInt(tt.MuteExpireTimeMS, 10))
		case *socket.DeleteMessageTask:
			kinds = append(kinds, "delete:"+tt.MessageId)
		default:
			t.Errorf("unexpected task %T", task)
		}
	}
	want := []string{"edit:hello", "react:👍🏽", "react:", "mute:-1", "delete:mid.$mine"}
	if len(kinds) != len(want) {
		t.Fatalf("tasks %q", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("task %d = %q, want %q", i, kinds[i], want[i])
		}
	}
	if del := h.rec.of("message_deleted"); len(del) != 1 {
		t.Errorf("deleted %+v", del)
	}
	if chats := h.rec.chats(); !chats[len(chats)-1].Muted {
		t.Error("mute not reported")
	}
	// Someone else's message isn't yours to edit or unsend.
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$theirs", aliceID, -1, "x")}})
	all := h.rec.messages()
	theirs := h.ref(c, all[len(all)-1].ID)
	if h.m.EditText(ctx, theirs, "y") == nil || h.m.Delete(ctx, theirs) == nil {
		t.Error("someone else's message was edited or unsent")
	}
}

func TestSearchFindsPeopleAndOpenDMMakesTheChat(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0), groupThread(groupID, "Trip", -20)}})
	h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
		switch tt := tasks[0].(type) {
		case *socket.SearchUserTask:
			if tt.Query != "a" {
				t.Errorf("query %q", tt.Query)
			}
			return &table.LSTable{LSInsertSearchResult: []*table.LSInsertSearchResult{
				{ResultId: "100002", ThreadType: table.ONE_TO_ONE, DisplayName: "Alice Example", CanViewerMessage: true},
				{ResultId: "4343", ThreadType: table.ONE_TO_ONE, DisplayName: "Dana New", CanViewerMessage: true},
				{ResultId: "4444", ThreadType: table.ONE_TO_ONE, DisplayName: "Blocked", CanViewerMessage: false},
				{ResultId: strconv.FormatInt(groupID, 10), ThreadType: table.GROUP_THREAD, DisplayName: "Trip"},
			}}, nil
		}
		return &table.LSTable{}, nil
	}
	results, err := h.m.Search(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].ChatID != h.chat(aliceID).id || results[1].Title != "Dana New" || results[1].ChatID != 0 || results[2].Kind != proto.Group {
		t.Fatalf("results %+v", results)
	}
	id, err := h.m.OpenDM(context.Background(), backend.UserRef{ID: results[1].UserID, Network: proto.Messenger, NetID: "4343"})
	if err != nil {
		t.Fatal(err)
	}
	c := h.chat(4343)
	if c == nil || c.id != id || !c.encrypted || c.kind != proto.DM {
		t.Fatalf("new chat %+v", c)
	}
	var created, wa bool
	for _, task := range h.meta.sent() {
		switch tt := task.(type) {
		case *socket.CreateThreadTask:
			created = tt.ThreadFBID == 4343
		case *socket.CreateWhatsAppThreadTask:
			wa = tt.WAJID == 4343
		}
	}
	if !created || !wa {
		t.Errorf("create tasks %v %v", created, wa)
	}
	chats := h.rec.chats()
	if last := chats[len(chats)-1]; last.ID != id || last.Title != "Dana New" || !last.Encrypted {
		t.Errorf("chat event %+v", last)
	}
	// Opening it again gives the same chat without asking anything.
	n := len(h.meta.sent())
	again, _ := h.m.OpenDM(context.Background(), backend.UserRef{NetID: "4343"})
	if again != id || len(h.meta.sent()) != n {
		t.Error("open_dm made a second chat")
	}
}
