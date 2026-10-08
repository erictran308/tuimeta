// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"slices"
	"testing"
	"time"

	waTypes "go.mau.fi/whatsmeow/types"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// outgoing is a send as the server hands it over.
func (h *harness) outgoing(c *chat, text string, reply *backend.MessageRef, files ...backend.Upload) *backend.Outgoing {
	temps := h.deps.Messages.Temp(c.id, max(len(files), 1))
	return h.deps.Outbox.New(refOf(c), temps, text, reply, files)
}

func TestASentMessageAnswersWhatItRepliesToAndIsKeptAsSent(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "when?"), aliceLID))
	c := h.chat(aliceLID.String())
	ref := h.ref(c, "A1")
	h.rec.reset()
	out := h.outgoing(c, "at *5*", &ref)
	if err := h.w.Send(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	sent := h.wa.sentMessages()
	if len(sent) != 1 || sent[0].to != aliceLID {
		t.Fatalf("sent %+v", sent)
	}
	x := sent[0].msg.GetExtendedTextMessage()
	ci := x.GetContextInfo()
	if x.GetText() != "at *5*" || ci.GetStanzaID() != "A1" || ci.GetParticipant() != aliceLID.String() || ci.GetQuotedMessage().GetConversation() != "when?" {
		t.Fatalf("message %+v", sent[0].msg)
	}
	var confirmed *proto.MessageSentEvent
	for _, v := range h.rec.all() {
		if e, ok := v.(proto.MessageSentEvent); ok {
			confirmed = &e
		}
	}
	if confirmed == nil || confirmed.Message.Text != "at 5" || !confirmed.Message.Outgoing || confirmed.Message.ReplyTo == nil {
		t.Fatalf("confirmation %+v", confirmed)
	}
	if !slices.ContainsFunc(c.log.All(), func(m proto.Message) bool { return m.ID == confirmed.Message.ID }) {
		t.Error("the sent message isn't in the chat")
	}
}

func TestInAChatWithDisappearingMessagesWhatsSentDisappearsToo(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "hi"), aliceLID))
	c := h.chat(aliceLID.String())
	h.w.mu.Lock()
	c.Ephemeral = 86400
	h.w.mu.Unlock()
	if err := h.w.Send(context.Background(), h.outgoing(c, "gone tomorrow", nil)); err != nil {
		t.Fatal(err)
	}
	m := h.wa.sentMessages()[0].msg
	if m.GetExtendedTextMessage().GetContextInfo().GetExpiration() != 86400 {
		t.Fatalf("sent %+v", m)
	}
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	for _, kept := range c.msgs {
		if kept.Text == "gone tomorrow" && kept.Expires != at(0).Add(time.Second).UnixMilli()+86400_000 {
			t.Errorf("kept to expire at %d", kept.Expires)
		}
	}
}

func pngOf(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func TestFilesGoAsPhotosVideosAudioOrDocumentsWithTheCaptionLast(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -5, "send them"))
	c := h.chat(benLID.String())
	files := []backend.Upload{
		{Name: "a.png", Mime: "image/png", Kind: proto.Photo, Data: pngOf(300, 150), Width: 300, Height: 150},
		{Name: "b.gif", Mime: "image/gif", Kind: proto.GIF, Data: []byte("GIF89a")},
		{Name: "c.pdf", Mime: "application/pdf", Kind: proto.FileMedia, Data: []byte("%PDF")},
	}
	if err := h.w.Send(context.Background(), h.outgoing(c, "the plan", nil, files...)); err != nil {
		t.Fatal(err)
	}
	sent := h.wa.sentMessages()
	if len(sent) != 3 {
		t.Fatalf("sent %d messages", len(sent))
	}
	img := sent[0].msg.GetImageMessage()
	if img == nil || img.GetCaption() != "" || img.GetWidth() != 300 || img.GetDirectPath() != "/up/1" {
		t.Fatalf("photo %+v", sent[0].msg)
	}
	thumb, err := jpeg.DecodeConfig(bytes.NewReader(img.GetJPEGThumbnail()))
	if err != nil || thumb.Width != thumbSide || thumb.Height != thumbSide/2 {
		t.Errorf("thumbnail %+v %v", thumb, err)
	}
	if d := sent[1].msg.GetDocumentMessage(); d == nil || d.GetFileName() != "b.gif" {
		t.Errorf("a GIF goes as a document: %+v", sent[1].msg)
	}
	if d := sent[2].msg.GetDocumentMessage(); d == nil || d.GetCaption() != "the plan" || d.GetMimetype() != "application/pdf" {
		t.Errorf("the last file carries the caption: %+v", sent[2].msg)
	}
}

func TestEditAndDeleteKeepToWhatsAppsWindows(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, selfLID, "S1", -10, "helo"))
	h.event(text(benLID, selfLID, "S2", -3*24*60, "old"))
	h.event(text(benLID, benLID, "B1", -2, "theirs"))
	c := h.chat(benLID.String())
	if err := h.w.EditText(context.Background(), h.ref(c, "S1"), "hello"); err != nil {
		t.Fatal(err)
	}
	edit := h.wa.sentMessages()[0].msg.GetEditedMessage().GetMessage().GetProtocolMessage()
	if edit.GetKey().GetID() != "S1" || edit.GetEditedMessage().GetConversation() != "hello" {
		t.Fatalf("edit %+v", edit)
	}
	if err := h.w.EditText(context.Background(), h.ref(c, "S2"), "x"); err == nil {
		t.Error("an edit past 15 minutes went out")
	}
	if err := h.w.EditText(context.Background(), h.ref(c, "B1"), "x"); err == nil {
		t.Error("someone else's message was edited")
	}
	if err := h.w.Delete(context.Background(), h.ref(c, "S2")); err == nil {
		t.Error("a message days old was deleted for everyone")
	}
	if err := h.w.Delete(context.Background(), h.ref(c, "B1")); err == nil {
		t.Error("someone else's message was deleted")
	}
	if err := h.w.Delete(context.Background(), h.ref(c, "S1")); err != nil {
		t.Fatal(err)
	}
	sent := h.wa.sentMessages()
	if rv := sent[len(sent)-1].msg.GetProtocolMessage(); rv.GetKey().GetID() != "S1" || !rv.GetKey().GetFromMe() {
		t.Errorf("revoke %+v", rv)
	}
	if len(sent) != 2 {
		t.Errorf("%d requests went out", len(sent))
	}
}

func TestYourReactionGoesOutAndShowsAsYours(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(groupJID, benLID, "G1", -2, "lunch?"))
	c := h.chat(groupJID.String())
	h.rec.reset()
	if err := h.w.React(context.Background(), h.ref(c, "G1"), "❤"); err != nil {
		t.Fatal(err)
	}
	r := h.wa.sentMessages()[0].msg.GetReactionMessage()
	if r.GetText() != "❤️" || r.GetKey().GetID() != "G1" || r.GetKey().GetParticipant() != benLID.String() {
		t.Fatalf("reaction %+v", r)
	}
	msgs := h.rec.messages()
	if len(msgs) != 1 || len(msgs[0].Reactions) != 1 || !msgs[0].Reactions[0].Mine {
		t.Fatalf("shown %+v", msgs)
	}
	if err := h.w.React(context.Background(), h.ref(c, "G1"), ""); err != nil {
		t.Fatal(err)
	}
	if msgs = h.rec.messages(); len(msgs[len(msgs)-1].Reactions) != 0 {
		t.Error("your reaction stayed")
	}
}

func TestReadReceiptsGoOutOnlyFromMarkReadAndOnlyOnce(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(groupJID, benLID, "G1", -5, "one"))
	h.event(withAlt(text(groupJID, alicePN, "G2", -4, "two"), aliceLID))
	h.event(text(groupJID, selfLID, "G3", -3, "mine"))
	h.event(text(groupJID, benLID, "G4", -2, "four"))
	c := h.chat(groupJID.String())
	if len(h.wa.reads) != 0 {
		t.Fatal("receipts went out on their own")
	}
	if err := h.w.MarkRead(context.Background(), h.ref(c, "G3")); err != nil {
		t.Fatal(err)
	}
	if len(h.wa.reads) != 2 {
		t.Fatalf("receipts %+v", h.wa.reads)
	}
	for _, r := range h.wa.reads {
		if r.chat != groupJID || len(r.ids) != 1 {
			t.Errorf("receipt %+v", r)
		}
	}
	if c.Unread != 1 {
		t.Errorf("unread after reading up to yours: %d", c.Unread)
	}
	// The same again: nothing new is read, nobody is told.
	if err := h.w.MarkRead(context.Background(), h.ref(c, "G3")); err != nil {
		t.Fatal(err)
	}
	if len(h.wa.reads) != 2 {
		t.Errorf("receipts went out again: %+v", h.wa.reads)
	}
	for _, p := range h.wa.purposes {
		if p != "mark_read" {
			t.Errorf("a receipt went out for %q", p)
		}
	}
}

func TestTypingGoesOutOnlyFromSetTyping(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -5, "hi"))
	c := h.chat(benLID.String())
	if len(h.wa.typing) != 0 {
		t.Fatal("typing went out on its own")
	}
	if err := h.w.SetTyping(context.Background(), refOf(c), true); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SetTyping(context.Background(), refOf(c), false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.wa.typing, []waTypes.ChatPresence{waTypes.ChatPresenceComposing, waTypes.ChatPresencePaused}) {
		t.Errorf("typing %v", h.wa.typing)
	}
}

func TestMuteIsForGoodAndFollowsEveryDevice(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -5, "hi"))
	c := h.chat(benLID.String())
	if err := h.w.Mute(context.Background(), refOf(c), true); err != nil {
		t.Fatal(err)
	}
	chats := h.rec.chats()
	if !slices.Equal(h.wa.mutes, []bool{true}) || !chats[len(chats)-1].Muted || c.MuteUntil != -1 {
		t.Errorf("mutes %v, chat %+v", h.wa.mutes, chats[len(chats)-1])
	}
}

func TestSearchFindsContactsGroupsAndNumbersOnWhatsApp(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(groupJID, benLID, "G1", -2, "hi"))
	g := h.chat(groupJID.String())
	h.w.mu.Lock()
	g.Name = "Alice's party"
	h.w.mu.Unlock()
	res, err := h.w.Search(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, r := range res {
		titles = append(titles, r.Title)
	}
	if !slices.Contains(titles, "Alice Example") || !slices.Contains(titles, "Alice's party") {
		t.Errorf("found %q", titles)
	}
	carol := waTypes.NewJID("447700900199", waTypes.DefaultUserServer)
	h.wa.onWA["+447700900199"] = carol
	res, err = h.w.Search(context.Background(), "+44 7700 900199")
	if err != nil || len(res) != 1 || res[0].Title != "+447700900199" || res[0].Kind != proto.DM || res[0].UserID == 0 {
		t.Fatalf("by number: %+v %v", res, err)
	}
	id, err := h.w.OpenDM(context.Background(), backend.UserRef{ID: res[0].UserID, Network: proto.WhatsApp, NetID: carol.String()})
	if err != nil || id == 0 {
		t.Fatalf("open dm %d %v", id, err)
	}
	if res, _ = h.w.Search(context.Background(), "nobody here"); len(res) != 0 {
		t.Errorf("found %+v", res)
	}
}

func TestTheUnreachableConnectionSaysSoInsteadOfSending(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -5, "hi"))
	c := h.chat(benLID.String())
	h.w.mu.Lock()
	h.w.ready = false
	h.w.mu.Unlock()
	err := h.w.Send(context.Background(), h.outgoing(c, "hello?", nil))
	if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.NetworkError {
		t.Fatalf("got %v", err)
	}
	if len(h.wa.sentMessages()) != 0 {
		t.Error("it was sent anyway")
	}
}
