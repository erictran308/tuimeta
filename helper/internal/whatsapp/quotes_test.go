// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// carolLID is a third member of the test group.
var carolLID = waTypes.NewJID("100000000000004", waTypes.HiddenUserServer)

// replyTo is a text from sender in chat answering id, naming participant
// (none if empty) and quoting quote.
func replyTo(chat, sender waTypes.JID, id string, min int, body, target string, participant waTypes.JID, quote string) *events.Message {
	ci := &waE2E.ContextInfo{StanzaID: gproto.String(target), QuotedMessage: &waE2E.Message{Conversation: gproto.String(quote)}}
	if !participant.IsEmpty() {
		ci.Participant = gproto.String(participant.String())
	}
	evt := &events.Message{Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: gproto.String(body), ContextInfo: ci}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = chat, sender, id, at(min)
	evt.Info.IsGroup = chat.Server == waTypes.GroupServer
	evt.Info.IsFromMe = sender.User == selfPN.User || sender.User == selfLID.User
	return evt
}

func lastMessage(t *testing.T, h *harness) proto.Message {
	t.Helper()
	msgs := h.rec.messages()
	if len(msgs) == 0 {
		t.Fatal("no message reported")
	}
	return msgs[len(msgs)-1]
}

func TestAMessagesTextIsReadOnceHoweverOftenItsShown(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -5, "*hi* `there`"))
	first := h.kept(benLID.String(), "B1").parsed
	if first == nil {
		t.Fatal("not read")
	}
	for i, emoji := range []string{"👍", "", "😂"} {
		evt := &events.Message{Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key: &waCommon.MessageKey{ID: gproto.String("B1")}, Text: gproto.String(emoji),
		}}}
		evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "R"+string(rune('0'+i)), at(-4)
		h.event(evt)
	}
	if got := h.kept(benLID.String(), "B1").parsed; got != first {
		t.Error("a reaction had the text read again")
	}
	if msgs := h.rec.messages(); len(msgs) != 4 || msgs[3].Text != "hi there" || len(msgs[3].Entities) != 2 {
		t.Fatalf("shown as %+v", msgs)
	}
	// The newest of a chat not opened, shown in the list at each load.
	h2 := h.restart()
	c := h2.chat(benLID.String())
	h2.w.mu.Lock()
	defer h2.w.mu.Unlock()
	h2.w.chatObject(c)
	again := c.last.parsed
	if out := h2.w.chatObject(c); c.last.parsed != again || out.LastMessage.Text != "hi there" {
		t.Error("the chat list had the text read again")
	}
}

func TestAQuoteOfAKeptEventIsTheEventNotTheRepliersWords(t *testing.T) {
	h := newHarness(t)
	h.load()
	own := timer(benLID, selfLID, 86400)
	own.Info.ID, own.Info.IsFromMe, own.Info.Timestamp = "T1", true, at(-30)
	h.event(own)
	event := lastMessage(t, h)
	self := h.userID(selfLID.String())
	const forged = "I'll send you 500 tonight"
	for i, named := range []waTypes.JID{selfLID, benLID, {}} {
		h.rec.reset()
		h.event(replyTo(benLID, benLID, "B"+string(rune('1'+i)), -10+i, "so?", "T1", named, forged))
		r := lastMessage(t, h).ReplyTo
		if r == nil {
			t.Fatalf("naming %v: no quote", named)
		}
		if r.Text == forged && (r.SenderID == self || r.MessageID == event.ID) {
			t.Errorf("naming %v: Ben's words are quoted as yours: %+v", named, r)
		}
		if named == selfLID && (r.MessageID != event.ID || r.SenderID != self || r.Text != event.Service) {
			t.Errorf("naming you: the quote isn't your event as it is: %+v", r)
		}
	}
}

func TestAQuoteIsLinkedOnlyToAKeptMessageOfTheSenderItNames(t *testing.T) {
	h := newHarness(t)
	h.load()
	// Ben sends a message with the id of one of Alice's this device doesn't
	// have.
	h.event(text(groupJID, benLID, "Q", -10, "pay 500 to account 1234"))
	bens := lastMessage(t, h)
	h.event(replyTo(groupJID, carolLID, "C1", -9, "yes, agreed", "Q", aliceLID, "lunch at noon?"))
	r := lastMessage(t, h).ReplyTo
	if r == nil || r.SenderID != 0 || r.MessageID == bens.ID || r.Text != "lunch at noon?" {
		t.Errorf("a reply to Alice's message quotes Ben's: %+v", r)
	}
	// A reply that names Ben is his.
	h.event(replyTo(groupJID, carolLID, "C2", -8, "what account?", "Q", benLID, "whatever he says"))
	r = lastMessage(t, h).ReplyTo
	if r == nil || r.MessageID != bens.ID || r.SenderID != h.userID(benLID.String()) || r.Text != "pay 500 to account 1234" {
		t.Errorf("a reply to Ben's message: %+v", r)
	}
	// One that names nobody is linked to nothing.
	h.event(replyTo(groupJID, carolLID, "C3", -7, "?", "Q", waTypes.EmptyJID, "something"))
	if r = lastMessage(t, h).ReplyTo; r == nil || r.MessageID != 0 || r.SenderID != 0 {
		t.Errorf("a reply naming nobody: %+v", r)
	}
}
