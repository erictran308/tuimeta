// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestAMessageArrivesInItsChatIsShownAndIsKeptForTheNextRun(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "*see* you at 5"), aliceLID))

	msgs := h.rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	m := msgs[0]
	if m.Text != "see you at 5" || len(m.Entities) != 1 || m.Entities[0].Type != proto.Bold {
		t.Errorf("text %q entities %+v", m.Text, m.Entities)
	}
	if m.Outgoing || m.SenderID != h.userID(aliceLID.String()) || m.Date != at(-5).Unix() {
		t.Errorf("message %+v", m)
	}
	if ids.Millis(m.ID) != at(-5).UnixMilli() {
		t.Errorf("id %d isn't from its time", m.ID)
	}
	c := h.chat(aliceLID.String())
	if c == nil || c.kind != proto.DM || c.Unread != 1 {
		t.Fatalf("chat %+v", c)
	}
	chats := h.rec.chats()
	if len(chats) == 0 || chats[len(chats)-1].Title != "Alice Example" || !chats[len(chats)-1].Encrypted || chats[len(chats)-1].Unread != 1 {
		t.Errorf("chat events %+v", chats)
	}

	// The next run reads it back from the store.
	h2 := h.restart()
	c2 := h2.chat(aliceLID.String())
	if c2 == nil || c2.id != c.id {
		t.Fatalf("chat after restart %+v", c2)
	}
	page, err := h2.w.History(context.Background(), refOf(c2), history.Query{Limit: 10})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Text != "see you at 5" {
		t.Fatalf("history after restart: %+v %v", page, err)
	}
}

func refOf(c *chat) backend.ChatRef {
	return backend.ChatRef{ID: c.id, Network: proto.WhatsApp, NetID: c.key}
}

func TestAChatByNumberMovesToTheWhatsAppIdAndKeepsItsIds(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(alicePN, alicePN, "A1", -9, "hi"))
	byNumber := h.chat(alicePN.String())
	if byNumber == nil {
		t.Fatal("no chat by number")
	}
	chatID, userID := byNumber.id, h.userID(alicePN.String())

	// Her next message says who she is on WhatsApp.
	h.event(withAlt(text(alicePN, alicePN, "A2", -8, "it's me"), aliceLID))
	c := h.chat(aliceLID.String())
	if c == nil || c.id != chatID {
		t.Fatalf("chat by id = %+v, want id %d", c, chatID)
	}
	if got := h.userID(aliceLID.String()); got != userID {
		t.Errorf("her user id changed: %d → %d", userID, got)
	}
	if n, key, ok := h.deps.IDs.LookupChat(chatID); !ok || n != proto.WhatsApp || key != aliceLID.String() {
		t.Errorf("ids.json has %v %q %v", n, key, ok)
	}
	if len(c.msgs) != 2 {
		t.Errorf("messages %d", len(c.msgs))
	}
}

func TestOnlyItsSenderEditsAMessageAndAnOlderEditNeverWins(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "helo"), aliceLID))
	edit := func(sender string, min int, body string) *events.Message {
		evt := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: gproto.String("A1")},
			EditedMessage: &waE2E.Message{Conversation: gproto.String(body)}, TimestampMS: gproto.Int64(at(min).UnixMilli()),
		}}}
		evt.Info.Chat, evt.Info.ID, evt.Info.Timestamp = alicePN, "E"+body, at(min)
		evt.Info.Sender = alicePN
		if sender == "ben" {
			evt.Info.Sender = benPN
		}
		return evt
	}
	h.rec.reset()
	h.event(edit("ben", -4, "hacked"))
	if len(h.rec.messages()) != 0 {
		t.Fatal("someone else's edit applied")
	}
	h.event(edit("alice", -3, "hello"))
	h.event(edit("alice", -4, "older"))
	msgs := h.rec.messages()
	if len(msgs) != 1 || msgs[0].Text != "hello" || !msgs[0].Edited {
		t.Fatalf("after edits: %+v", msgs)
	}
}

func TestReactionsAreOnePerPersonAndDeletionsOnlyFromTheSender(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "news"), aliceLID))
	react := func(sender, alt interface{ String() string }, emoji string) {
		evt := &events.Message{Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key: &waCommon.MessageKey{ID: gproto.String("A1")}, Text: gproto.String(emoji),
		}}}
		evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = alicePN, alicePN, "R"+emoji, at(-4)
		if sender.String() == selfPN.String() {
			evt.Info.Sender, evt.Info.IsFromMe = selfPN, true
		}
		h.event(evt)
	}
	react(alicePN, aliceLID, "👍")
	react(selfPN, selfLID, "❤️")
	react(alicePN, aliceLID, "😂") // hers changes
	msgs := h.rec.messages()
	last := msgs[len(msgs)-1]
	if len(last.Reactions) != 2 || last.Reactions[0].Emoji != "❤️" || !last.Reactions[0].Mine || last.Reactions[1].Emoji != "😂" {
		t.Errorf("reactions %+v", last.Reactions)
	}
	react(alicePN, aliceLID, "")
	msgs = h.rec.messages()
	if last = msgs[len(msgs)-1]; len(last.Reactions) != 1 {
		t.Errorf("after taking hers back: %+v", last.Reactions)
	}

	revoke := func(sender string) {
		evt := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: gproto.String("A1")},
		}}}
		evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = alicePN, alicePN, "X"+sender, at(-2)
		if sender == "me" {
			evt.Info.Sender, evt.Info.IsFromMe = selfPN, true
		}
		h.event(evt)
	}
	revoke("me") // not mine to delete
	if len(h.rec.deletions()) != 0 {
		t.Fatal("someone else's message was deleted by you")
	}
	revoke("alice")
	if d := h.rec.deletions(); len(d) != 1 || d[0].MessageIDs[0] != last.ID {
		t.Fatalf("deletions %+v", d)
	}
	c := h.chat(aliceLID.String())
	if len(c.msgs) != 0 || c.Unread != 0 {
		t.Errorf("after deletion: %d messages, %d unread", len(c.msgs), c.Unread)
	}
	h2 := h.restart()
	if c2 := h2.chat(aliceLID.String()); c2 != nil {
		h2.w.mu.Lock()
		h2.w.ensureLoaded(c2)
		n := len(c2.msgs)
		h2.w.mu.Unlock()
		if n != 0 {
			t.Error("the deleted message was still kept")
		}
	}
}

func TestMentionsShowTheNameAndBecomeMentionEntities(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa.lids[alicePN] = aliceLID
	evt := &events.Message{Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text:        gproto.String("🎉 thanks @447700900101!"),
		ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{alicePN.String()}},
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp, evt.Info.IsGroup = groupJID, benLID, "G1", at(-1), true
	h.event(evt)
	m := h.rec.messages()[0]
	if m.Text != "🎉 thanks @Alice Example!" {
		t.Fatalf("text %q", m.Text)
	}
	if len(m.Entities) != 1 || m.Entities[0].Type != proto.Mention || m.Entities[0].UserID != h.userID(aliceLID.String()) {
		t.Fatalf("entities %+v", m.Entities)
	}
	// The emoji is two UTF-16 units: the offset counts them that way.
	if m.Entities[0].Offset != 10 || m.Entities[0].Length != len("@Alice Example") {
		t.Errorf("entity %+v", m.Entities[0])
	}
}

func TestViewOnceMediaKeepsNothingThatCouldFetchIt(t *testing.T) {
	h := newHarness(t)
	h.load()
	evt := &events.Message{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Mimetype: gproto.String("image/jpeg"), DirectPath: gproto.String("/v/secret"), MediaKey: []byte("k"),
		FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), JPEGThumbnail: []byte("thumb"), Caption: gproto.String("only once"),
	}}, IsViewOnce: true}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "V1", at(-1)
	h.event(evt)
	m := h.rec.messages()[0]
	if m.Media == nil || !m.Media.ViewOnce || m.Media.FileID != 0 || m.Media.Thumbnail != nil || m.Text != "" {
		t.Fatalf("media %+v text %q", m.Media, m.Text)
	}
	data, _ := json.Marshal(h.chat(benLID.String()).msgs["V1"])
	for _, secret := range []string{"secret", "path", "key", "thumb", "only once"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("kept %q: %s", secret, data)
		}
	}
}

func TestAttachmentsAreFilesFetchedOnlyWhenAskedAndThumbnailsComeFromTheMessage(t *testing.T) {
	h := newHarness(t)
	h.load()
	evt := &events.Message{Message: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
		Mimetype: gproto.String("video/mp4"), DirectPath: gproto.String("/v/clip"), MediaKey: []byte("k"),
		FileSHA256: []byte{1, 2, 3}, FileEncSHA256: []byte("e"), FileLength: gproto.Uint64(2048), Seconds: gproto.Uint32(9),
		Width: gproto.Uint32(640), Height: gproto.Uint32(360), JPEGThumbnail: []byte("JPEG"), Caption: gproto.String("look"),
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "M1", at(-1)
	h.event(evt)
	m := h.rec.messages()[0]
	md := m.Media
	if md == nil || md.Kind != proto.Video || md.FileID == 0 || md.Duration != 9 || md.Size != 2048 || m.Text != "look" {
		t.Fatalf("media %+v", md)
	}
	if md.Thumbnail == nil || md.Thumbnail.FileID == md.FileID || md.Thumbnail.Width != 640 {
		t.Fatalf("thumbnail %+v", md.Thumbnail)
	}
	if len(h.wa.files) != 0 {
		t.Fatal("something was downloaded")
	}
	ref, _ := h.deps.Files.Get(md.Thumbnail.FileID)
	var buf strings.Builder
	if err := h.w.Fetch(context.Background(), ref, &buf); err != nil || buf.String() != "JPEG" {
		t.Fatalf("thumbnail fetch %q %v", buf.String(), err)
	}
	ref, _ = h.deps.Files.Get(md.FileID)
	if err := h.w.Fetch(context.Background(), ref, &buf); err == nil || !strings.Contains(err.Error(), "no longer") {
		t.Errorf("a file gone from the servers: %v", err)
	}
	h.wa.files["/v/clip"] = []byte("video bytes")
	buf.Reset()
	if err := h.w.Fetch(context.Background(), ref, &buf); err != nil || buf.String() != "video bytes" {
		t.Errorf("fetch %q %v", buf.String(), err)
	}
}

func TestARepliesQuoteNamesTheMessageItAnswers(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "when?"), aliceLID))
	evt := &events.Message{Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: gproto.String("at 5"),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID: gproto.String("A1"), Participant: gproto.String(aliceLID.String()),
			QuotedMessage: &waE2E.Message{Conversation: gproto.String("when?")},
		},
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp, evt.Info.IsFromMe = aliceLID, selfLID, "S1", at(-4), true
	h.event(evt)
	msgs := h.rec.messages()
	first, reply := msgs[0], msgs[1]
	if !reply.Outgoing || reply.ReplyTo == nil || reply.ReplyTo.MessageID != first.ID || reply.ReplyTo.Text != "when?" ||
		reply.ReplyTo.SenderID != h.userID(aliceLID.String()) {
		t.Fatalf("reply %+v / %+v", reply, reply.ReplyTo)
	}
	if reply.EditableUntil != at(-4).Unix()+EditWindow || !reply.Deletable {
		t.Errorf("own text: editable until %d, deletable %v", reply.EditableUntil, reply.Deletable)
	}
	if c := h.chat(aliceLID.String()); c.Unread != 1 {
		t.Errorf("your own message counted as unread: %d", c.Unread)
	}
}

func TestAMessageThatCouldntBeDecryptedIsReplacedWhenItComes(t *testing.T) {
	h := newHarness(t)
	h.load()
	und := &events.UndecryptableMessage{}
	und.Info.Chat, und.Info.Sender, und.Info.ID, und.Info.Timestamp = benLID, benLID, "U1", at(-2)
	h.event(und)
	first := h.rec.messages()[0]
	if !strings.Contains(first.Unsupported, "couldn't be decrypted") {
		t.Fatalf("placeholder %+v", first)
	}
	h.event(text(benLID, benLID, "U1", -2, "here it is"))
	msgs := h.rec.messages()
	if last := msgs[len(msgs)-1]; last.ID != first.ID || last.Text != "here it is" || last.Unsupported != "" {
		t.Fatalf("replaced by %+v", last)
	}
	if c := h.chat(benLID.String()); c.Unread != 1 {
		t.Errorf("counted twice: %d unread", c.Unread)
	}
}

func TestStatusUpdatesChannelsAndBroadcastsArentChats(t *testing.T) {
	h := newHarness(t)
	h.load()
	status := text(waTypes.StatusBroadcastJID, benLID, "S1", -1, "my story")
	h.event(status)
	if len(h.rec.messages()) != 0 || len(h.rec.chats()) != 0 {
		t.Fatal("a status update became a chat")
	}
}
