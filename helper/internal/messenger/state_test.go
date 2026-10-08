// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"errors"
	"testing"

	"github.com/coder/websocket"
	"go.mau.fi/whatsmeow/proto/waArmadilloApplication"
	"go.mau.fi/whatsmeow/proto/waArmadilloXMA"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func lastAccount(h *harness) proto.AccountEvent {
	acc := h.rec.of("account")
	if len(acc) == 0 {
		return proto.AccountEvent{}
	}
	return acc[len(acc)-1].(proto.AccountEvent)
}

func TestTheSocketsUpsAndDownsAreTheAccountsState(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	gen := h.m.gen
	h.m.upGen = gen // connected
	h.m.mu.Unlock()
	var signalled []error
	signal := func(err error) { signalled = append(signalled, err) }
	h.m.onMetaEvent(gen, &messagix.TransientDisconnectEvent{Err: errors.New("eof")}, nil, signal)
	if lastAccount(h).State != proto.Connecting {
		t.Errorf("after a drop: %+v", lastAccount(h))
	}
	h.m.onMetaEvent(gen, &messagix.ReconnectedEvent{}, nil, signal)
	if a := lastAccount(h); a.State != proto.Ready || a.UserID != h.userID(selfID) || a.Name != "Robin Hale" {
		t.Errorf("after reconnecting: %+v", a)
	}
	// Events of a connection that was replaced are ignored.
	h.m.onMetaEvent(gen-1, &messagix.TransientDisconnectEvent{}, nil, signal)
	if lastAccount(h).State != proto.Ready {
		t.Error("an old connection's event counted")
	}
	h.m.onMetaEvent(gen, &messagix.PermanentErrorEvent{Err: websocket.CloseError{Code: dgw.CloseStatusUnauthorized}}, nil, signal)
	a := lastAccount(h)
	if a.State != proto.Errored || a.Error == "" || len(signalled) == 0 {
		t.Errorf("after being logged out elsewhere: %+v", a)
	}
}

func TestEncryptedConnectionTroubleIsReported(t *testing.T) {
	h := newHarness(t)
	h.wa(&events.StreamReplaced{})
	h.wa(&events.TemporaryBan{})
	errs := h.rec.of("error")
	if len(errs) != 2 {
		t.Fatalf("errors %+v", errs)
	}
	for _, e := range errs {
		if e.(proto.ErrorEvent).Network != proto.Messenger || e.(proto.ErrorEvent).Message == "" {
			t.Errorf("error %+v", e)
		}
	}
	h.wa(&events.Connected{})
	h.m.mu.Lock()
	ok := h.m.e2eeOK
	h.m.mu.Unlock()
	if !ok {
		t.Error("connected not recorded")
	}
}

func armadilloMsg(content *waArmadilloApplication.Armadillo_Content) *waArmadilloApplication.Armadillo {
	return &waArmadilloApplication.Armadillo{Payload: &waArmadilloApplication.Armadillo_Payload{
		Payload: &waArmadilloApplication.Armadillo_Payload_Content{Content: content},
	}}
}

func TestMessengerSpecificEncryptedContent(t *testing.T) {
	h := newHarness(t)
	h.load()
	send := func(id string, content *waArmadilloApplication.Armadillo_Content) proto.Message {
		evt := waMsg(jid(aliceID), jid(aliceID), id, 0, waText("x"), nil)
		evt.Message = armadilloMsg(content)
		h.rec.reset()
		h.wa(evt)
		msgs := h.rec.messages()
		if len(msgs) == 0 {
			t.Fatalf("%s: no message", id)
		}
		return msgs[len(msgs)-1]
	}
	once := send("V1", &waArmadilloApplication.Armadillo_Content{Content: &waArmadilloApplication.Armadillo_Content_RavenMessage_{
		RavenMessage: &waArmadilloApplication.Armadillo_Content_RavenMessage{MediaContent: &waArmadilloApplication.Armadillo_Content_RavenMessage_VideoMessage{}},
	}})
	if once.Media == nil || !once.Media.ViewOnce || once.Media.Kind != proto.Video || once.Media.FileID != 0 {
		t.Errorf("view once %+v", once.Media)
	}
	like := send("L1", &waArmadilloApplication.Armadillo_Content{Content: &waArmadilloApplication.Armadillo_Content_CommonSticker_{CommonSticker: &waArmadilloApplication.Armadillo_Content_CommonSticker{}}})
	if like.Text != "👍" {
		t.Errorf("like %+v", like)
	}
	link := send("X1", &waArmadilloApplication.Armadillo_Content{Content: &waArmadilloApplication.Armadillo_Content_ExtendedContentMessage{ExtendedContentMessage: &waArmadilloXMA.ExtendedContentMessage{
		TitleText: gproto.String("Article"), SubtitleText: gproto.String("words"),
		Ctas: []*waArmadilloXMA.ExtendedContentMessage_CTA{{ActionURL: gproto.String("https://example.org/a")}},
	}}})
	if link.LinkPreview == nil || link.LinkPreview.URL != "https://example.org/a" || link.Text != "https://example.org/a" {
		t.Errorf("shared link %+v", link)
	}
	loc := send("X2", &waArmadilloApplication.Armadillo_Content{Content: &waArmadilloApplication.Armadillo_Content_ExtendedContentMessage{ExtendedContentMessage: &waArmadilloXMA.ExtendedContentMessage{
		Ctas: []*waArmadilloXMA.ExtendedContentMessage_CTA{{NativeURL: gproto.String("messenger://location_share?lat=1&long=2")}},
	}}})
	if loc.Unsupported != "[Location]" {
		t.Errorf("location %+v", loc)
	}
	pay := send("P1", &waArmadilloApplication.Armadillo_Content{Content: &waArmadilloApplication.Armadillo_Content_PaymentsTransactionMessage_{}})
	if pay.Unsupported != "[Payment]" {
		t.Errorf("payment %+v", pay)
	}
}

func TestConsumerContentTuimetaDoesntShowGetsALabel(t *testing.T) {
	h := newHarness(t)
	h.load()
	labels := map[string]*waConsumerApplication.ConsumerApplication_Content{
		"[Location]":      {Content: &waConsumerApplication.ConsumerApplication_Content_LocationMessage{}},
		"[Live location]": {Content: &waConsumerApplication.ConsumerApplication_Content_LiveLocationMessage{}},
		"[Contact]":       {Content: &waConsumerApplication.ConsumerApplication_Content_ContactMessage{}},
		"[Poll]":          {Content: &waConsumerApplication.ConsumerApplication_Content_PollCreationMessage{}},
	}
	i := 0
	for want, content := range labels {
		i++
		h.rec.reset()
		h.wa(waMsg(jid(aliceID), jid(aliceID), "C"+string(rune('0'+i)), 0, content, nil))
		if msgs := h.rec.messages(); len(msgs) != 1 || msgs[0].Unsupported != want {
			t.Errorf("%s: %+v", want, msgs)
		}
	}
	h.rec.reset()
	h.wa(waMsg(jid(aliceID), jid(aliceID), "T1", 0, &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_ExtendedTextMessage{
		ExtendedTextMessage: &waConsumerApplication.ConsumerApplication_ExtendedTextMessage{
			Text: &waCommon.MessageText{Text: gproto.String("read https://example.net/p")}, MatchedText: gproto.String("https://example.net/p"),
			Title: gproto.String("A page"), Description: gproto.String("about it"),
		},
	}}, nil))
	msgs := h.rec.messages()
	if len(msgs) != 1 || msgs[0].LinkPreview == nil || msgs[0].LinkPreview.Title != "A page" || msgs[0].Text != "read https://example.net/p" {
		t.Errorf("extended text %+v", msgs)
	}
}

func TestAMessageThatCouldntBeDecryptedWaitsForTheRealOne(t *testing.T) {
	h := newHarness(t)
	h.load()
	u := &events.UndecryptableMessage{}
	u.Info.Chat, u.Info.Sender, u.Info.ID, u.Info.Timestamp = jid(aliceID), jid(aliceID), "U1", base
	h.wa(u)
	first := h.rec.messages()
	if len(first) != 1 || first[0].Unsupported == "" {
		t.Fatalf("placeholder %+v", first)
	}
	h.rec.reset()
	evt := waMsg(jid(aliceID), jid(aliceID), "U1", 0, waText("now readable"), nil)
	h.wa(evt)
	got := h.rec.messages()
	if len(got) != 1 || got[0].ID != first[0].ID || got[0].Text != "now readable" || got[0].Unsupported != "" {
		t.Errorf("real message %+v", got)
	}
	// One hidden on purpose isn't shown.
	h.rec.reset()
	hidden := &events.UndecryptableMessage{DecryptFailMode: events.DecryptFailHide}
	hidden.Info.Chat, hidden.Info.Sender, hidden.Info.ID = jid(aliceID), jid(aliceID), "U2"
	h.wa(hidden)
	if len(h.rec.messages()) != 0 {
		t.Error("a hidden failure was shown")
	}
}

var _ = waTypes.EmptyJID
