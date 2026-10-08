// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/instameow"
	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// send runs a send as the server would: temporary ids, then Send.
func (h *harness) send(key any, text string, reply *backend.MessageRef, files ...backend.Upload) (*backend.Outgoing, error) {
	chat := h.chatRef(key)
	temps := h.deps.Messages.Temp(chat.ID, max(len(files), 1))
	out := h.deps.Outbox.New(chat, temps, text, reply, files)
	err := h.b.Send(h.t.Context(), out)
	if err != nil {
		out.Failed(err)
	}
	return out, err
}

func sentEvents(r *recorder) []map[string]any {
	var out []map[string]any
	for _, e := range r.events("message_sent") {
		out = append(out, e)
	}
	return out
}

func TestSendingTextGoesAsTypedAndConfirmsWithTheFinalMessage(t *testing.T) {
	h := loaded(t, textMsg("mid.q", mayaFBID, tsBase, "when?"))
	reply := h.msgRef(mayaFBID, "mid.q")
	out, err := h.send(mayaFBID, "*see* you at 5", &reply)
	if err != nil {
		t.Fatal(err)
	}
	req := h.api.last("SendMessage").(*slidetypes.SendTextRequest)
	if req.Text.Value != "*see* you at 5" || *req.IGThreadIGID != dmIGID || *req.ReplyToMessageID != "mid.q" || req.OfflineThreadingID == "" {
		t.Errorf("request = %+v", req)
	}
	sent := sentEvents(h.rec)
	if len(sent) != 1 || sent[0]["old_id"].(float64) != float64(out.TempIDs[0]) {
		t.Fatalf("message_sent = %v", sent)
	}
	m := sent[0]["message"].(map[string]any)
	// Instagram reads the stars as bold, and so does its copy here.
	if m["text"] != "see you at 5" || len(m["entities"].([]any)) != 1 || m["outgoing"] != true || m["state"] != "sent" || m["reply_to"].(map[string]any)["message_id"].(float64) != float64(reply.ID) {
		t.Errorf("sent message = %v", m)
	}
	if m["editable_until"] == nil {
		t.Error("your new text isn't editable")
	}
	noCalls(t, h.api, "MarkRead", "MarkReadValidation", "SetTyping")
}

func TestASendTheSocketConfirmsFirstIsReportedOnce(t *testing.T) {
	h := loaded(t)
	h.api.sendText = func(req *slidetypes.SendTextRequest) (*slidetypes.SendTextResponse, error) {
		echo := fmt.Sprintf(`{"id":"mid.echo","sender_fbid":"1000","timestamp_ms":"%d","offline_threading_id":%q,
			"content":{"__typename":"SlideMessageText","text_body":%q}}`, tsBase+9000, req.OfflineThreadingID, req.Text.Value)
		_ = h.api.emit(delta(t, "SlideUQPPNewMessage", dmIGID, `"message":`+echo))
		return &slidetypes.SendTextResponse{Message: sentMessage("mid.echo", tsBase+9000)}, nil
	}
	if _, err := h.send(mayaFBID, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if sent := sentEvents(h.rec); len(sent) != 1 || sent[0]["message"].(map[string]any)["text"] != "hello" {
		t.Errorf("message_sent = %v", sent)
	}
	if msgs := h.rec.events("message"); len(msgs) != 0 {
		t.Errorf("the socket's copy was also reported: %v", msgs)
	}
	// After, a copy from the socket is an ordinary update.
	h.rec.reset()
	echo := fmt.Sprintf(`{"id":"mid.echo","sender_fbid":"1000","timestamp_ms":"%d","content":{"__typename":"SlideMessageText","text_body":"hello"},
		"reactions":[{"reaction":"👍","sender_fbid":"2002"}]}`, tsBase+9000)
	_ = h.api.emit(delta(t, "SlideUQPPNewMessage", dmIGID, `"message":`+echo))
	if msgs := h.rec.events("message"); len(msgs) != 1 {
		t.Errorf("update = %v", msgs)
	}
}

func TestFilesAreUploadedOneMessageEachAndTheCaptionFollows(t *testing.T) {
	h := loaded(t)
	photo := backend.Upload{Name: "IMG_1.jpg", Mime: "image/jpeg", Kind: proto.Photo, Data: []byte("jpeg"), Width: 4, Height: 3}
	clip := backend.Upload{Name: "clip.mp4", Mime: "video/mp4", Kind: proto.Video, Data: []byte("mp4")}
	out, err := h.send(mayaFBID, "from Saturday", nil, photo, clip)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.api.names()[len(h.api.names())-5:], ","); got != "Upload,SendMedia,Upload,SendMedia,SendMessage" {
		t.Errorf("requests = %s", got)
	}
	req := h.api.last("SendMedia").(*slidetypes.SendMediaRequest)
	if req.ThreadID != dmIGID || req.AttachmentFBID != "5550003" {
		t.Errorf("media request = %+v", req)
	}
	sent := sentEvents(h.rec)
	if len(sent) != 2 || sent[0]["old_id"].(float64) != float64(out.TempIDs[0]) || sent[1]["old_id"].(float64) != float64(out.TempIDs[1]) {
		t.Fatalf("message_sent = %v", sent)
	}
	if m := sent[0]["message"].(map[string]any)["media"].(map[string]any); m["kind"] != "photo" || m["name"] != "IMG_1.jpg" {
		t.Errorf("first file = %v", m)
	}
	// The caption is a message of its own after the files.
	if msgs := h.rec.events("message"); len(msgs) != 1 || msgs[0]["message"].(map[string]any)["text"] != "from Saturday" {
		t.Errorf("caption = %v", msgs)
	}
}

func TestFilesInstagramWontTakeAreRefusedBeforeAnythingIsSent(t *testing.T) {
	h := loaded(t)
	pdf := backend.Upload{Name: "a.pdf", Mime: "application/pdf", Kind: proto.FileMedia, Data: []byte("%PDF")}
	_, err := h.send(mayaFBID, "", nil, pdf)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.Unsupported {
		t.Errorf("error = %v", err)
	}
	big := backend.Upload{Name: "big.jpg", Mime: "image/jpeg", Kind: proto.Photo, Data: make([]byte, maxImageSize+1)}
	if _, err := h.send(mayaFBID, "", nil, big); !errors.As(err, &pe) || !strings.Contains(pe.Message, "8 MB") {
		t.Errorf("error = %v", err)
	}
	if h.api.count("Upload") != 0 {
		t.Errorf("requests = %v", h.api.names())
	}
	if failed := h.rec.events("message_failed"); len(failed) != 2 {
		t.Errorf("message_failed = %v", failed)
	}
}

func TestAnsweringAMessageRequestAcceptsItFirst(t *testing.T) {
	h := newHarness(t)
	req := strings.Replace(dmThread(), `"system_folder":"INBOX"`, `"system_folder":"PENDING"`, 1)
	h.login(mailbox(t, "", false, req))
	if _, err := h.b.LoadChats(t.Context(), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := h.send(mayaFBID, "hi", nil); err != nil {
		t.Fatal(err)
	}
	names := h.api.names()
	if names[len(names)-2] != "AcceptMessageRequest" || names[len(names)-1] != "SendMessage" {
		t.Errorf("requests = %v", names)
	}
}

func TestEditingUnsendingReactingAndMutingNameTheRightThings(t *testing.T) {
	now := time.Now().UnixMilli()
	h := loaded(t, textMsg("mid.mine", selfFBID, now, "on my way"), textMsg("mid.theirs", mayaFBID, now-1000, "ok"))
	h.b.mu.Lock()
	c := h.b.chats[mayaFBID]
	h.b.putLog(c, c.msgs["mid.mine"])
	h.b.mu.Unlock()
	mine, theirs := h.msgRef(mayaFBID, "mid.mine"), h.msgRef(mayaFBID, "mid.theirs")

	if err := h.b.EditText(t.Context(), mine, "on my way!"); err != nil {
		t.Fatal(err)
	}
	edit := h.api.last("EditMessage").(*slidetypes.EditMessageRequest)
	if edit.ThreadID != dmIGID || edit.TargetMessageID != "mid.mine" || edit.Body.Value != "on my way!" {
		t.Errorf("edit = %+v", edit)
	}
	if msgs := h.rec.events("message"); len(msgs) != 1 || msgs[0]["message"].(map[string]any)["edited"] != true {
		t.Errorf("edited = %v", msgs)
	}
	// The socket's copy of the same edit doesn't count twice.
	_ = h.api.emit(delta(t, "SlideUQPPEditMessage", dmIGID, `"message_id":"mid.mine","text_body":"on my way!"`))
	if c.msgs["mid.mine"].editCount != 1 {
		t.Errorf("edit counted %d times", c.msgs["mid.mine"].editCount)
	}
	if err := h.b.EditText(t.Context(), theirs, "x"); err == nil {
		t.Error("someone else's message was edited")
	}

	if err := h.b.React(t.Context(), theirs, "❤️"); err != nil {
		t.Fatal(err)
	}
	r := h.api.last("SendReaction").(*slidetypes.CreateReactionRequest)
	if r.Input.Emoji != "❤" || r.Input.MessageID != "mid.theirs" || r.Input.ThreadID != dmIGID || r.Input.ReactionStatus != slidetypes.ReactionStatusCreated {
		t.Errorf("reaction = %+v", r.Input)
	}
	if err := h.b.React(t.Context(), theirs, ""); err != nil {
		t.Fatal(err)
	}
	r = h.api.last("SendReaction").(*slidetypes.CreateReactionRequest)
	if r.Input.Emoji != "❤" || r.Input.ReactionStatus != slidetypes.ReactionStatusDeleted {
		t.Errorf("removal = %+v", r.Input)
	}

	if err := h.b.Mute(t.Context(), h.chatRef(mayaFBID), true); err != nil {
		t.Fatal(err)
	}
	if m := h.api.last("MuteThread").(*slidetypes.MuteThreadRequest); m.ThreadID != dmIGID || m.MuteSeconds != -1 {
		t.Errorf("mute = %+v", m)
	}

	h.rec.reset()
	if err := h.b.Delete(t.Context(), mine); err != nil {
		t.Fatal(err)
	}
	u := h.api.last("UnsendMessage").(*slidetypes.UnsendMessageRequest)
	if u.MessageID != "mid.mine" || u.SendData.ThreadID != dmLong {
		t.Errorf("unsend = %+v", u)
	}
	if d := h.rec.events("message_deleted"); len(d) != 1 {
		t.Errorf("message_deleted = %v", d)
	}
	if err := h.b.Delete(t.Context(), theirs); err == nil {
		t.Error("someone else's message was unsent")
	}
}

func TestSearchFindsPeopleAndOpenDMMakesTheChat(t *testing.T) {
	h := loaded(t)
	h.api.search = decode[*slidetypes.SearchResponse](t, `{"xfb_combinedIGSearchQuery":{"search_messenger_contact_v2":[
		{"interop_messaging_user_fbid":"2002","username":"maya.lens","full_name":"Maya Lopez","pk":"34000000002"},
		{"interop_messaging_user_fbid":"3005","username":"lena.v","full_name":"Lena Vogel","pk":"34000000005"}]}}`)
	results, err := h.b.Search(t.Context(), "l")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].ChatID != h.chatID(mayaFBID) || results[1].ChatID != 0 || results[1].Username != "lena.v" ||
		results[2].Kind != proto.Group {
		t.Errorf("results = %+v", results)
	}
	// Lena has no thread yet: the chat starts with the first text.
	id, err := h.b.OpenDM(t.Context(), backend.UserRef{ID: results[1].UserID, Network: proto.Instagram, NetID: "3005"})
	if err != nil || id != h.chatID(3005) {
		t.Fatalf("open_dm = %d, %v", id, err)
	}
	if c := chatEvents(h.rec); len(c) == 0 || c[len(c)-1]["title"] != "New Person" || c[len(c)-1]["kind"] != "dm" {
		t.Errorf("chat = %v", c)
	}
	if _, err := h.send(int64(3005), "hi Lena", nil); err != nil {
		t.Fatal(err)
	}
	req := h.api.last("SendMessage").(*slidetypes.SendTextRequest)
	if req.IGThreadIGID != nil || len(req.RecipientIGIDs) != 1 || req.RecipientIGIDs[0] != "3005" {
		t.Errorf("first message = %+v", req)
	}
	// One Instagram knows comes with its history.
	h.api.threadIDs[noorFBID] = &instameow.ThreadIGIDs{LongID: "noor-long", ShortID: "noor-igid"}
	h.api.threads["noor-igid"] = threadInfo(t, thread("noor-igid", fmt.Sprint(noorFBID), "noor-long", false, "", []string{noorJSON}, tsBase,
		[]string{textMsg("mid.n", noorFBID, tsBase, "hey")}, "", false, ""))
	id, err = h.b.OpenDM(t.Context(), backend.UserRef{NetID: fmt.Sprint(noorFBID)})
	if err != nil || id != h.chatID(noorFBID) {
		t.Fatalf("open_dm = %d, %v", id, err)
	}
}

func TestASendWhoseAnswerIsLostCountsIfTheSocketBroughtIt(t *testing.T) {
	h := loaded(t)
	h.api.sendText = func(req *slidetypes.SendTextRequest) (*slidetypes.SendTextResponse, error) {
		echo := fmt.Sprintf(`{"id":"mid.lost","sender_fbid":"1000","timestamp_ms":"%d","offline_threading_id":%q,
			"content":{"__typename":"SlideMessageText","text_body":"still here"}}`, tsBase+7000, req.OfflineThreadingID)
		_ = h.api.emit(delta(t, "SlideUQPPNewMessage", dmIGID, `"message":`+echo))
		return nil, errors.New("connection reset")
	}
	if _, err := h.send(mayaFBID, "still here", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent := sentEvents(h.rec); len(sent) != 1 {
		t.Errorf("message_sent = %v", sent)
	}
	if failed := h.rec.events("message_failed"); len(failed) != 0 {
		t.Errorf("message_failed = %v", failed)
	}
}
