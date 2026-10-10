// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"strconv"
	"testing"

	"go.mau.fi/whatsmeow/proto/waArmadilloApplication"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
)

// waDocument is an encrypted file as its sender describes it.
func waDocument(t *testing.T, name string, sha, mediaKey []byte, path string, size uint64, thumb []byte) *waConsumerApplication.ConsumerApplication_Content {
	t.Helper()
	doc := &waConsumerApplication.ConsumerApplication_DocumentMessage{FileName: gproto.String(name)}
	if err := doc.Set(&waMediaTransport.DocumentTransport{
		Integral: &waMediaTransport.DocumentTransport_Integral{Transport: &waMediaTransport.WAMediaTransport{
			Integral: &waMediaTransport.WAMediaTransport_Integral{FileSHA256: sha, FileEncSHA256: []byte{9}, MediaKey: mediaKey, DirectPath: gproto.String(path)},
			Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{
				FileLength: gproto.Uint64(size), Mimetype: gproto.String("application/pdf"),
				Thumbnail: &waMediaTransport.WAMediaTransport_Ancillary_Thumbnail{JPEGThumbnail: thumb},
			},
		}},
		Ancillary: &waMediaTransport.DocumentTransport_Ancillary{},
	}); err != nil {
		t.Fatal(err)
	}
	return &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_DocumentMessage{DocumentMessage: doc}}
}

// quoting is a reply's metadata naming the message it answers, with the
// sender's copy of that message's text.
func quoting(t *testing.T, id, participant, text string) *waMsgApplication.MessageApplication_Metadata {
	t.Helper()
	cm := &waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage{}
	if err := cm.Set(wrapConsumer(waText(text))); err != nil {
		t.Fatal(err)
	}
	return &waMsgApplication.MessageApplication_Metadata{QuotedMessage: &waMsgApplication.MessageApplication_Metadata_QuotedMessage{
		StanzaID: gproto.String(id), Participant: gproto.String(participant),
		Payload: &waMsgApplication.MessageApplication_Payload{Content: &waMsgApplication.MessageApplication_Payload_SubProtocol{
			SubProtocol: &waMsgApplication.MessageApplication_SubProtocolPayload{SubProtocol: cm},
		}},
	}}
}

func TestAnotherSenderStatingAFilesHashCantTakeItOver(t *testing.T) {
	h := newHarness(t)
	h.load()
	sha := []byte("the report's own sha-256")
	h.wa(waMsg(jid(aliceID), jid(aliceID), "F1", -2, waDocument(t, "report.pdf", sha, []byte("alice's key"), "/v/report", 1000, []byte("alice's still")), nil))
	// Ben knows the hash (it's in his copy of the group's message) and
	// states it with his own file, name, size and still.
	h.wa(waMsg(jid(benID), jid(benID), "F2", -1, waDocument(t, "report.exe", sha, []byte("ben's key"), "/v/evil", 300<<20, []byte("ben's still")), nil))
	msgs := h.rec.messages()
	if len(msgs) != 2 || msgs[0].Media == nil || msgs[1].Media == nil || msgs[0].Media.Thumbnail == nil {
		t.Fatalf("messages %+v", msgs)
	}
	alice, ben := msgs[0].Media, msgs[1].Media
	if alice.FileID == ben.FileID || alice.Thumbnail.FileID == ben.Thumbnail.FileID {
		t.Fatalf("Ben's message shares Alice's file %d or still %d", alice.FileID, alice.Thumbnail.FileID)
	}
	ref, _ := h.deps.Files.Get(alice.FileID)
	if ref.Name != "report.pdf" || ref.Size != 1000 || ref.Source.(*waSource).Integral.GetDirectPath() != "/v/report" {
		t.Errorf("Alice's file became %+v", ref)
	}
	still, _ := h.deps.Files.Get(alice.Thumbnail.FileID)
	if string(still.Source.(*inlineSource).Data) != "alice's still" {
		t.Errorf("Alice's still is %q", still.Source.(*inlineSource).Data)
	}
	// The same file forwarded (its hash and key) is still one file.
	h.wa(waMsg(jid(aliceID), jid(aliceID), "F3", 0, waDocument(t, "report.pdf", sha, []byte("alice's key"), "/v/report", 1000, []byte("alice's still")), nil))
	if again := h.rec.messages()[2].Media; again.FileID != alice.FileID {
		t.Errorf("the same file got a new id %d", again.FileID)
	}
}

func TestAQuoteOfAMessageNotKeptNamesNobody(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(waGroup, jid(benID), "B1", -1, waText("noted!"), quoting(t, "never-sent", jid(aliceID).String(), "I owe Ben 500, will pay tonight")))
	msgs := h.rec.messages()
	got := msgs[len(msgs)-1]
	if got.ReplyTo == nil || got.ReplyTo.SenderID != 0 || got.ReplyTo.Text != "I owe Ben 500, will pay tonight" {
		t.Errorf("reply %+v", got.ReplyTo)
	}
	// Nor in your own mouth.
	h.rec.reset()
	h.wa(waMsg(waGroup, jid(benID), "B2", 0, waText("you said it"), quoting(t, "also-never", jid(selfID).String(), "I give up")))
	if r := h.rec.messages()[0].ReplyTo; r == nil || r.SenderID != 0 {
		t.Errorf("a made-up quote was put in your mouth: %+v", r)
	}
	// A stranger named isn't announced or looked up.
	const stranger = 100099
	h.wa(waMsg(jid(aliceID), jid(aliceID), "A1", 0, waText("ok"), quoting(t, "nope", jid(stranger).String(), "words")))
	for _, task := range h.meta.sent() {
		if c, ok := task.(*socket.GetContactsFullTask); ok && c.ContactID == stranger {
			t.Error("the person a quote named was looked up")
		}
	}
	h.m.mu.Lock()
	_, known := h.m.people[stranger]
	h.m.mu.Unlock()
	if known {
		t.Error("the person a quote named became someone tuimeta knows")
	}
	// A quote of a message kept here names its sender.
	h.rec.reset()
	h.wa(waMsg(waGroup, jid(aliceID), "A2", 0, waText("about B1"), quoting(t, "B1", jid(benID).String(), "made up")))
	if r := h.rec.messages()[0].ReplyTo; r == nil || r.SenderID != h.userID(benID) || r.Text != "noted!" {
		t.Errorf("a kept message's quote %+v", r)
	}
}

func TestAGalleryShowsAtMostSixtyFourPhotosAndRegistersNoMore(t *testing.T) {
	h := newHarness(t)
	h.load()
	var images []*waMediaTransport.ImageTransport
	for i := range 70 {
		images = append(images, &waMediaTransport.ImageTransport{Integral: &waMediaTransport.ImageTransport_Integral{Transport: &waMediaTransport.WAMediaTransport{
			Integral:  &waMediaTransport.WAMediaTransport_Integral{FileSHA256: []byte{byte(i)}, MediaKey: []byte{1}, DirectPath: gproto.String("/v/g" + strconv.Itoa(i))},
			Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{Mimetype: gproto.String("image/jpeg")},
		}}})
	}
	gallery := &waArmadilloApplication.Armadillo_Content_ImageGalleryMessage{}
	if err := gallery.Set(images); err != nil {
		t.Fatal(err)
	}
	evt := waMsg(jid(aliceID), jid(aliceID), "G1", 0, waText("x"), nil)
	evt.Message = armadilloMsg(&waArmadilloApplication.Armadillo_Content{Content: &waArmadilloApplication.Armadillo_Content_ImageGalleryMessage_{ImageGalleryMessage: gallery}})
	h.wa(evt)
	if n := len(h.rec.messages()); n != maxParts {
		t.Errorf("shown as %d parts", n)
	}
	files := 0
	for id := range int32(500) {
		if ref, ok := h.deps.Files.Get(id); ok {
			if _, encrypted := ref.Source.(*waSource); encrypted {
				files++
			}
		}
	}
	if files != maxParts {
		t.Errorf("%d files registered", files)
	}
}

// waReaction is a reaction to the message with id from sender.
func waReaction(chat waTypes.JID, id string, fromMe bool, participant, emoji string) *waConsumerApplication.ConsumerApplication_Content {
	return &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_ReactionMessage{
		ReactionMessage: &waConsumerApplication.ConsumerApplication_ReactionMessage{Key: key(chat, id, fromMe, participant), Text: gproto.String(emoji)},
	}}
}

func TestAMessageIdSentAgainNeverRewritesTheMessage(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "B1", -10, waText("I'll pay you back"), nil))
	first := h.rec.messages()[0]
	h.m.receiveWA(waMsg(jid(aliceID), jid(selfID), "R1", -9, waReaction(jid(aliceID), "B1", false, "", "👍"), nil))
	h.rec.reset()
	// Alice's message again, under the same id, with other words.
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "B1", -1, waText("you owe me"), nil))
	if msgs := h.rec.messages(); len(msgs) != 0 {
		t.Errorf("the second copy was shown: %+v", msgs)
	}
	c := h.chat(aliceID)
	h.m.mu.Lock()
	msg := c.msgs[waNetID(jid(aliceID), "B1")]
	text, edited, reactions, at := msg.text, msg.edited, len(msg.reactions), msg.ms
	h.m.mu.Unlock()
	if text != "I'll pay you back" || edited || reactions != 1 || at != ms(-10) {
		t.Errorf("the message became %q (edited %v, %d reactions, at %d)", text, edited, reactions, at)
	}
	h.restart(gen)
	all := h.chat(aliceID).log.All()
	if len(all) != 1 || all[0].ID != first.ID || all[0].Text != "I'll pay you back" || len(all[0].Reactions) != 1 {
		t.Errorf("read back %+v", all)
	}
}

// broadcastTo is the address a message to a broadcast list numbered like
// fbid's chat would come from.
func broadcastTo(fbid int64) waTypes.JID {
	return waTypes.NewJID(strconv.FormatInt(fbid, 10), waTypes.BroadcastServer)
}

func TestEncryptedEventsAddressedOutsideAChatAreDropped(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.wa(waMsg(jid(aliceID), jid(aliceID), "A1", -2, waText("hi"), nil))
	c := h.chat(aliceID)
	h.rec.reset()
	evt := waMsg(broadcastTo(aliceID), jid(benID), "B1", -1, waText("filed under Alice"), nil)
	evt.Info.IsGroup = true
	h.wa(evt)
	u := &events.UndecryptableMessage{}
	u.Info.Chat, u.Info.Sender, u.Info.ID, u.Info.Timestamp, u.Info.IsGroup = broadcastTo(aliceID), jid(benID), "B2", base, true
	h.wa(u)
	// Nor is Ben's message filed in Alice's one-to-one chat.
	h.wa(waMsg(jid(aliceID), jid(benID), "B3", -1, waText("in Alice's chat"), nil))
	if msgs := h.rec.messages(); len(msgs) != 0 {
		t.Errorf("shown: %+v", msgs)
	}
	h.m.mu.Lock()
	to := c.jid().String()
	h.m.mu.Unlock()
	if to != "100002@msgr" {
		t.Errorf("Alice's chat now goes to %s", to)
	}
	if err := h.m.Send(context.Background(), h.outgoing(c, "still you?", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if len(h.e2ee.sent) != 1 || h.e2ee.sent[0].to.String() != "100002@msgr" {
		t.Errorf("sent %+v", h.e2ee.sent)
	}
	// Receipts and typing on such an address aren't about any chat.
	h.rec.reset()
	h.wa(waReceiptOf(waTypes.ReceiptTypeRead, broadcastTo(aliceID), jid(benID), false, "A1"))
	cp := &events.ChatPresence{State: waTypes.ChatPresenceComposing}
	cp.Chat, cp.Sender = broadcastTo(aliceID), jid(benID)
	h.wa(cp)
	if len(h.rec.of("read")) != 0 || len(h.rec.of("typing")) != 0 {
		t.Errorf("events %+v", h.rec.all())
	}
}

func TestABroadcastAddressDoesntMakeAFacebookChatEncrypted(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	evt := waMsg(broadcastTo(aliceID), jid(benID), "B1", -1, waText("x"), nil)
	evt.Info.IsGroup = true
	h.wa(evt)
	h.m.mu.Lock()
	c := h.m.lookupChat(aliceID)
	encrypted, server, sendsEncrypted := c.encrypted, c.server, h.m.sendsEncrypted(c)
	h.m.mu.Unlock()
	if encrypted || server != "" || sendsEncrypted {
		t.Errorf("Alice's Facebook chat became encrypted (%q)", server)
	}
}

func TestAKeptMessageWithABroadcastAddressIsntReadBack(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "A1", -2, waText("hi"), nil))
	app, _ := gproto.Marshal(waMsg(broadcastTo(aliceID), jid(benID), "B1", -1, waText("old"), nil).FBApplication)
	if err := h.kept().put(context.Background(), storedMessage{Chat: broadcastTo(aliceID).String(), Sender: jid(benID).String(), ID: "B1", TS: base, App: app}); err != nil {
		t.Fatal(err)
	}
	h.restart(gen)
	c := h.chat(aliceID)
	h.m.mu.Lock()
	server, n := c.server, len(c.msgs)
	h.m.mu.Unlock()
	if server != waTypes.MessengerServer || n != 1 {
		t.Errorf("read back to %q with %d messages", server, n)
	}
}
