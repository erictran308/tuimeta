// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func jid(fbid int64) waTypes.JID {
	return waTypes.NewJID(strconv.FormatInt(fbid, 10), waTypes.MessengerServer)
}

var waGroup = waTypes.NewJID("777", waTypes.GroupServer)

// waMsg is an encrypted message as whatsmeow delivers it.
func waMsg(chat, sender waTypes.JID, id string, min int, content *waConsumerApplication.ConsumerApplication_Content, meta *waMsgApplication.MessageApplication_Metadata) *events.FBMessage {
	if meta == nil {
		meta = &waMsgApplication.MessageApplication_Metadata{}
	}
	evt := ownEvent(chat, sender, id, time.UnixMilli(ms(min)), wrapConsumer(content), meta)
	evt.Info.IsFromMe = sender.User == strconv.FormatInt(selfID, 10)
	return evt
}

func waText(text string, mentioned ...string) *waConsumerApplication.ConsumerApplication_Content {
	return &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_MessageText{
		MessageText: &waCommon.MessageText{Text: gproto.String(text), MentionedJID: mentioned},
	}}
}

func key(chat waTypes.JID, id string, fromMe bool, participant string) *waCommon.MessageKey {
	k := &waCommon.MessageKey{RemoteJID: gproto.String(chat.String()), ID: gproto.String(id), FromMe: gproto.Bool(fromMe)}
	if participant != "" {
		k.Participant = gproto.String(participant)
	}
	return k
}

func (h *harness) wa(evt any) {
	h.m.mu.Lock()
	gen := h.m.gen
	h.m.mu.Unlock()
	h.m.onE2EE(gen, evt)
}

func TestAnEncryptedMessageMakesAnEncryptedChatWithItsMentions(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(waGroup, jid(aliceID), "A1", -1, waText("hey @100003@msgr, *look*", "100003@msgr"), nil))
	msgs := h.rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages %+v", h.rec.all())
	}
	m := msgs[0]
	if m.Text != "hey @Ben Carter, look" || m.SenderID != h.userID(aliceID) {
		t.Errorf("message %+v", m)
	}
	want := []proto.Entity{{Offset: 4, Length: 11, Type: proto.Mention, UserID: h.userID(benID)}, {Offset: 17, Length: 4, Type: proto.Bold}}
	if !slices.Equal(m.Entities, want) {
		t.Errorf("entities %+v", m.Entities)
	}
	chats := h.rec.chats()
	if len(chats) == 0 || !chats[len(chats)-1].Encrypted || chats[len(chats)-1].Kind != proto.Group {
		t.Errorf("chat %+v", chats)
	}
	c := h.chat(777)
	if !c.log.Complete() {
		t.Error("an encrypted chat's history should end where it starts here")
	}
}

func TestEncryptedEditsReactionsAndUnsendsFollowTheRules(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(jid(aliceID), jid(aliceID), "M1", -3, waText("helo"), nil))
	orig := h.rec.messages()[0]
	h.rec.reset()
	edit := func(sender waTypes.JID, text string, at int64) *events.FBMessage {
		return waMsg(jid(aliceID), sender, "E"+text, -2, &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_EditMessage{
			EditMessage: &waConsumerApplication.ConsumerApplication_EditMessage{Key: key(jid(aliceID), "M1", true, ""), Message: &waCommon.MessageText{Text: gproto.String(text)}, TimestampMS: gproto.Int64(at)},
		}}, nil)
	}
	h.wa(edit(jid(aliceID), "hello", 2000))
	h.wa(edit(jid(aliceID), "stale", 1000)) // older than the last edit
	h.wa(edit(jid(selfID), "hijack", 3000)) // not the sender's
	got := h.rec.messages()
	if len(got) != 1 || got[0].ID != orig.ID || got[0].Text != "hello" || !got[0].Edited {
		t.Fatalf("edits %+v", got)
	}
	h.rec.reset()
	h.wa(waMsg(jid(aliceID), jid(selfID), "R1", -1, &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_ReactionMessage{
		// From you, about Alice's message: not "from me" in the key.
		ReactionMessage: &waConsumerApplication.ConsumerApplication_ReactionMessage{Key: key(jid(aliceID), "M1", false, ""), Text: gproto.String("😮")},
	}}, nil))
	got = h.rec.messages()
	if len(got) != 1 || len(got[0].Reactions) != 1 || !got[0].Reactions[0].Mine || got[0].Reactions[0].Emoji != "😮" {
		t.Fatalf("reaction %+v", got)
	}
	revoke := func(sender waTypes.JID, fromMe bool) *events.FBMessage {
		evt := waMsg(jid(aliceID), sender, "X", 0, nil, nil)
		evt.Message = &waConsumerApplication.ConsumerApplication{Payload: &waConsumerApplication.ConsumerApplication_Payload{
			Payload: &waConsumerApplication.ConsumerApplication_Payload_ApplicationData{ApplicationData: &waConsumerApplication.ConsumerApplication_ApplicationData{
				ApplicationContent: &waConsumerApplication.ConsumerApplication_ApplicationData_Revoke{Revoke: &waConsumerApplication.ConsumerApplication_RevokeMessage{Key: key(jid(aliceID), "M1", fromMe, "")}},
			}},
		}}
		return evt
	}
	h.rec.reset()
	h.wa(revoke(jid(selfID), true)) // you can't unsend Alice's message
	if len(h.rec.of("message_deleted")) != 0 {
		t.Error("someone else's message was unsent")
	}
	h.wa(revoke(jid(aliceID), true))
	if del := h.rec.of("message_deleted"); len(del) != 1 || del[0].(proto.MessageDeletedEvent).MessageIDs[0] != orig.ID {
		t.Errorf("unsend %+v", del)
	}
}

func TestEncryptedMediaQuotesAndDownloads(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(jid(aliceID), jid(selfID), "Q1", -5, waText("where?"), nil))
	img := &waConsumerApplication.ConsumerApplication_ImageMessage{Caption: &waCommon.MessageText{Text: gproto.String("here")}}
	if err := img.Set(&waMediaTransport.ImageTransport{
		Integral: &waMediaTransport.ImageTransport_Integral{Transport: &waMediaTransport.WAMediaTransport{
			Integral:  &waMediaTransport.WAMediaTransport_Integral{FileSHA256: []byte{0xab, 0xcd}, FileEncSHA256: []byte{1}, MediaKey: []byte{2}, DirectPath: gproto.String("/v/photo")},
			Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{FileLength: gproto.Uint64(5), Mimetype: gproto.String("image/jpeg")},
		}},
		Ancillary: &waMediaTransport.ImageTransport_Ancillary{Width: gproto.Uint32(800), Height: gproto.Uint32(600)},
	}); err != nil {
		t.Fatal(err)
	}
	quote := &waMsgApplication.MessageApplication_Metadata{QuotedMessage: &waMsgApplication.MessageApplication_Metadata_QuotedMessage{
		StanzaID: gproto.String("Q1"), Participant: gproto.String(jid(selfID).String()),
	}}
	h.wa(waMsg(jid(aliceID), jid(aliceID), "P1", -4, &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_ImageMessage{ImageMessage: img}}, quote))
	msgs := h.rec.messages()
	q, p := msgs[0], msgs[1]
	if p.Text != "here" || p.ReplyTo == nil || p.ReplyTo.MessageID != q.ID || p.ReplyTo.Text != "where?" {
		t.Errorf("reply %+v", p)
	}
	md := p.Media
	if md == nil || md.Kind != proto.Photo || md.FileID == 0 || md.Width != 800 || md.Thumbnail == nil || md.Thumbnail.FileID != md.FileID {
		t.Fatalf("media %+v", md)
	}
	ref, _ := h.deps.Files.Get(md.FileID)
	if ref.Key != "wa:abcd" {
		t.Errorf("key %q", ref.Key)
	}
	h.e2ee.files["/v/photo"] = []byte("JPEG!")
	var buf writeBuf
	if err := h.m.Fetch(context.Background(), ref, &buf); err != nil || string(buf) != "JPEG!" {
		t.Errorf("fetch %q %v", buf, err)
	}
}

func TestEncryptedReceiptsTypingAndGroupChanges(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(waGroup, jid(selfID), "S1", -3, waText("mine"), nil))
	h.wa(waMsg(waGroup, jid(aliceID), "A1", -2, waText("theirs"), nil))
	h.rec.reset()
	receipt := func(typ waTypes.ReceiptType, from waTypes.JID, id string) *events.Receipt {
		r := &events.Receipt{MessageIDs: []waTypes.MessageID{id}, Type: typ}
		r.Chat, r.Sender = waGroup, from
		return r
	}
	h.wa(receipt(waTypes.ReceiptTypeRead, jid(benID), "S1"))
	h.wa(receipt(waTypes.ReceiptTypeReadSelf, jid(selfID), "A1"))
	h.wa(receipt(waTypes.ReceiptTypeDelivered, jid(benID), "S1"))
	reads := h.rec.of("read")
	if len(reads) != 2 || reads[0].(proto.ReadEvent).Outbox == nil || reads[1].(proto.ReadEvent).Inbox == nil || *reads[1].(proto.ReadEvent).Unread != 0 {
		t.Fatalf("reads %+v", reads)
	}
	cp := &events.ChatPresence{State: waTypes.ChatPresenceComposing}
	cp.Chat, cp.Sender = waGroup, jid(benID)
	h.wa(cp)
	if typing := h.rec.of("typing"); len(typing) != 1 || !typing[0].(proto.TypingEvent).Typing {
		t.Errorf("typing %+v", typing)
	}
	h.rec.reset()
	alice := jid(aliceID)
	h.wa(&events.GroupInfo{JID: waGroup, Sender: &alice, Timestamp: base, Name: &waTypes.GroupName{Name: "Book\nclub"}, Join: []waTypes.JID{jid(benID)}})
	var sentences []string
	for _, m := range h.rec.messages() {
		sentences = append(sentences, m.Service)
	}
	if !slices.Equal(sentences, []string{"Alice Example named the group Book club", "Alice Example added Ben Carter"}) {
		t.Errorf("sentences %q", sentences)
	}
}

func TestSendingInAnEncryptedChatGoesThroughTheEncryptedSocket(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(jid(aliceID), jid(aliceID), "A1", -2, waText("one"), nil))
	h.wa(waMsg(jid(aliceID), jid(aliceID), "A2", -1, waText("two"), nil))
	c := h.chat(aliceID)
	msgs := h.rec.messages()
	h.rec.reset()
	out := h.outgoing(c, "*hi*", nil, nil)
	if err := h.m.Send(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	if len(h.e2ee.sent) != 1 || h.e2ee.sent[0].to.String() != "100002@msgr" {
		t.Fatalf("sent %+v", h.e2ee.sent)
	}
	app := h.e2ee.sent[0].msg.(*waConsumerApplication.ConsumerApplication)
	if app.GetPayload().GetContent().GetMessageText().GetText() != "*hi*" {
		t.Error("the text didn't go as typed")
	}
	sent := h.rec.of("message_sent")
	if len(sent) != 1 || sent[0].(proto.MessageSentEvent).Message.Text != "hi" {
		t.Errorf("sent %+v", sent)
	}
	for _, task := range h.meta.sent() {
		t.Errorf("an encrypted send asked Messenger for %T", task)
	}
	// Reading up to the newest sends one receipt for both of Alice's.
	if err := h.m.MarkRead(context.Background(), h.ref(c, msgs[1].ID)); err != nil {
		t.Fatal(err)
	}
	if len(h.e2ee.reads) != 1 || len(h.e2ee.reads[0].ids) != 2 || !h.e2ee.reads[0].sender.IsEmpty() {
		t.Errorf("receipts %+v", h.e2ee.reads)
	}
	if len(h.meta.sent()) != 0 {
		t.Error("an encrypted read also went to Messenger")
	}
	if err := h.m.SetTyping(context.Background(), backend.ChatRef{ID: c.id}, true); err != nil {
		t.Fatal(err)
	}
	if len(h.e2ee.presences) != 1 || h.e2ee.presences[0] != waTypes.ChatPresenceComposing || len(h.meta.stateless) != 0 {
		t.Errorf("typing %+v %+v", h.e2ee.presences, h.meta.stateless)
	}
}

func TestEncryptedMessagesAreKeptAndReadBackNextTime(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.sess.WADevice = fakeOwnJID.String()
	gen := h.m.gen
	h.m.mu.Unlock()
	h.m.openStore(gen)
	if h.m.store == nil {
		t.Fatal("no store")
	}
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "K1", -3, waText("kept"), nil))
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "K2", -2, waText("unsent later"), nil))
	revoke := waMsg(jid(aliceID), jid(aliceID), "K3", -1, nil, nil)
	revoke.Message = &waConsumerApplication.ConsumerApplication{Payload: &waConsumerApplication.ConsumerApplication_Payload{
		Payload: &waConsumerApplication.ConsumerApplication_Payload_ApplicationData{ApplicationData: &waConsumerApplication.ConsumerApplication_ApplicationData{
			ApplicationContent: &waConsumerApplication.ConsumerApplication_ApplicationData_Revoke{Revoke: &waConsumerApplication.ConsumerApplication_RevokeMessage{Key: key(jid(aliceID), "K2", true, "")}},
		}},
	}}
	h.m.receiveWA(revoke)
	st := h.m.store
	h.m.mu.Lock()
	h.m.store = nil
	h.m.reset()
	h.m.self = selfID
	p := h.m.person(aliceID)
	p.name, p.known = "Alice Example", true
	h.m.mu.Unlock()
	st.Close()
	h.rec.reset()

	h.m.openStore(gen)
	defer h.m.store.Close()
	if len(h.rec.messages()) != 0 {
		t.Error("kept messages were reported as new")
	}
	c := h.chat(aliceID)
	all := c.log.All()
	if len(all) != 1 || all[0].Text != "kept" || !c.encrypted {
		t.Fatalf("read back %+v", all)
	}
	h.load()
	if chats := h.rec.chats(); len(chats) != 1 || chats[0].LastMessage == nil || chats[0].LastMessage.Text != "kept" {
		t.Errorf("chat %+v", chats)
	}
}

func TestAnotherAccountsKeptMessagesAreNeverShown(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.sess.WADevice = "555:3@msgr" // someone else's device
	gen := h.m.gen
	h.m.mu.Unlock()
	path, _ := h.deps.Session.Path(storeFile)
	st, err := openStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := gproto.Marshal(waMsg(jid(aliceID), jid(aliceID), "Z", -1, waText("not yours"), nil).FBApplication)
	_ = st.put(context.Background(), storedMessage{Chat: jid(aliceID).String(), Sender: jid(aliceID).String(), ID: "Z", TS: base, App: app})
	st.Close()
	h.m.openStore(gen)
	defer h.m.store.Close()
	if c := h.chat(aliceID); c != nil && c.log.Len() > 0 {
		t.Error("another account's messages were read back")
	}
}

func TestEncryptedGroupsAreAskedForTheirDetails(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.e2ee.group = &waTypes.GroupInfo{JID: waGroup, GroupName: waTypes.GroupName{Name: "Secret plans"}, Participants: []waTypes.GroupParticipant{{JID: jid(aliceID)}, {JID: jid(selfID)}}}
	h.wa(waMsg(waGroup, jid(aliceID), "G1", -1, waText("psst"), nil))
	group := func() (string, int) {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		c := h.m.lookupChat(777)
		return c.name, len(c.members)
	}
	waitFor(t, "the group's details", func() bool { name, _ := group(); return name == "Secret plans" })
	if _, members := group(); members != 2 {
		t.Errorf("members %d", members)
	}
}
