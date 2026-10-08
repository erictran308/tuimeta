// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

var plainReader = reader{canon: func(j waTypes.JID) string { return j.ToNonAD().String() }}

func readMsg(m *waE2E.Message) change {
	evt := &events.Message{Message: m}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "X1", at(0)
	return plainReader.read(evt, benLID.String())
}

func TestEveryKindOfMessageReadsAsSomethingToShow(t *testing.T) {
	media := func(md *media) string {
		if md == nil {
			return ""
		}
		return string(md.Kind)
	}
	for name, tc := range map[string]struct {
		msg         *waE2E.Message
		kind        string
		text        string
		unsupported string
	}{
		"text":     {msg: &waE2E.Message{Conversation: gproto.String("hi")}, text: "hi"},
		"photo":    {msg: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: gproto.String("image/jpeg"), Caption: gproto.String("view")}}, kind: "photo", text: "view"},
		"gif":      {msg: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: gproto.Bool(true)}}, kind: "gif"},
		"video":    {msg: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Seconds: gproto.Uint32(3)}}, kind: "video"},
		"round":    {msg: &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Seconds: gproto.Uint32(3)}}, kind: "video"},
		"voice":    {msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: gproto.Bool(true)}}, kind: "voice"},
		"audio":    {msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}, kind: "audio"},
		"document": {msg: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: gproto.String("plan.pdf"), Caption: gproto.String("read")}}, kind: "file", text: "read"},
		"sticker":  {msg: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: gproto.String("image/webp")}}, kind: "sticker"},
		"location": {msg: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}}, unsupported: "[Location]"},
		"contact":  {msg: &waE2E.Message{ContactMessage: &waE2E.ContactMessage{}}, unsupported: "[Contact]"},
		"poll":     {msg: &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{}}, unsupported: "[Poll]"},
		"invite":   {msg: &waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{}}, unsupported: "[Group invite]"},
		"unknown":  {msg: &waE2E.Message{ProductMessage: &waE2E.ProductMessage{}}, unsupported: "[Unsupported message]"},
	} {
		ch := readMsg(tc.msg)
		if ch.kind != added {
			t.Errorf("%s: kind %v", name, ch.kind)
			continue
		}
		if got := media(ch.msg.Media); got != tc.kind || ch.msg.Text != tc.text || ch.msg.Unsupported != tc.unsupported {
			t.Errorf("%s: media %q text %q unsupported %q", name, got, ch.msg.Text, ch.msg.Unsupported)
		}
	}
}

func TestBookkeepingMessagesShowNothing(t *testing.T) {
	for name, m := range map[string]*waE2E.Message{
		"vote":     {PollUpdateMessage: &waE2E.PollUpdateMessage{}},
		"pin":      {PinInChatMessage: &waE2E.PinInChatMessage{}},
		"album":    {AlbumMessage: &waE2E.AlbumMessage{}},
		"keys":     {SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}},
		"hd photo": {ImageMessage: &waE2E.ImageMessage{}, MessageContextInfo: &waE2E.MessageContextInfo{MessageAssociation: &waE2E.MessageAssociation{AssociationType: waE2E.MessageAssociation_HD_IMAGE_DUAL_UPLOAD.Enum()}}},
		"sync":     {ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum()}},
		"nothing":  nil,
	} {
		if ch := readMsg(m); ch.kind != ignored {
			t.Errorf("%s: %+v", name, ch)
		}
	}
}

func TestALinksCardComesFromTheSendersAppAndOnlyForWebLinks(t *testing.T) {
	ch := readMsg(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: gproto.String("look example.com/post"), MatchedText: gproto.String("example.com/post"),
		Title: gproto.String("A\npost"), Description: gproto.String("words"), JPEGThumbnail: []byte("JPEG"),
	}})
	p := ch.msg.Preview
	if p == nil || p.URL != "https://example.com/post" || p.Title != "A post" || string(p.Thumb) != "JPEG" {
		t.Fatalf("preview %+v", p)
	}
	ch = readMsg(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: gproto.String("tap"), MatchedText: gproto.String("javascript:alert(1)"), Title: gproto.String("x"),
	}})
	if ch.msg.Preview != nil {
		t.Errorf("a script link got a card: %+v", ch.msg.Preview)
	}
}

func TestATimerChangeIsAnEventAndSetsTheChatsTimer(t *testing.T) {
	ch := readMsg(&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum(), EphemeralExpiration: gproto.Uint32(604800),
	}})
	if ch.kind != added || ch.timer == nil || *ch.timer != 604800 || ch.msg.Service == nil {
		t.Fatalf("change %+v", ch)
	}
	h := newHarness(t)
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	if got := h.w.sentence(ch.msg.Service); got != "~Ben turned on disappearing messages: new messages disappear after 7 days" {
		t.Errorf("sentence %q", got)
	}
	if got := h.w.sentence(timerService(selfLID.String(), 0)); got != "You turned off disappearing messages" {
		t.Errorf("sentence %q", got)
	}
}

func TestHistoryEventsBecomeSentencesWithTodaysNames(t *testing.T) {
	h := newHarness(t)
	stub := func(kind waWeb.WebMessageInfo_StubType, params ...string) *message {
		web := &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: gproto.String("S")}, MessageStubType: kind.Enum(), MessageStubParameters: params}
		return plainReader.stub(web, "S", benLID.String(), 1000)
	}
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	for want, m := range map[string]*message{
		"~Ben created the group Trip":      stub(waWeb.WebMessageInfo_GROUP_CREATE, "Trip"),
		"~Ben named the group Trip 2":      stub(waWeb.WebMessageInfo_GROUP_CHANGE_SUBJECT, "Trip\n2"),
		"~Ben added you and Alice Example": stub(waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD, selfLID.String(), alicePN.String()),
		"~Ben removed Alice Example":       stub(waWeb.WebMessageInfo_GROUP_PARTICIPANT_REMOVE, alicePN.String()),
		"Alice Example left":               stub(waWeb.WebMessageInfo_GROUP_PARTICIPANT_LEAVE, alicePN.String()),
		"Missed voice call":                stub(waWeb.WebMessageInfo_CALL_MISSED_VOICE),
		"Missed video call":                stub(waWeb.WebMessageInfo_CALL_MISSED_GROUP_VIDEO),
		"~Ben changed the group's photo":   stub(waWeb.WebMessageInfo_GROUP_CHANGE_ICON),
		"~Ben turned on disappearing messages: new messages disappear after 24 hours": stub(waWeb.WebMessageInfo_CHANGE_EPHEMERAL_SETTING, "86400"),
	} {
		if m == nil {
			t.Errorf("%q: no event", want)
			continue
		}
		if got := h.w.sentence(m.Service); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if m := stub(waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD); m != nil {
		t.Error("an add naming nobody became an event")
	}
	if m := stub(waWeb.WebMessageInfo_E2E_ENCRYPTED); m != nil {
		t.Error("the encryption notice became an event")
	}
}

func TestDurationsReadLikeWhatsAppsOwn(t *testing.T) {
	for secs, want := range map[uint64]string{86400: "24 hours", 604800: "7 days", 7776000: "90 days", 3600: "1 hour", 7200: "2 hours", 300: "5 minutes"} {
		if got := duration(secs); got != want {
			t.Errorf("%d: %q", secs, got)
		}
	}
}

func TestQuotesAreOneLineSnippets(t *testing.T) {
	for want, q := range map[string]*waE2E.Message{
		"line one line two": {Conversation: gproto.String("*line one*\nline two")},
		"Photo":             {ImageMessage: &waE2E.ImageMessage{}},
		"Voice message":     {AudioMessage: &waE2E.AudioMessage{PTT: gproto.Bool(true)}},
		"plan.pdf":          {DocumentMessage: &waE2E.DocumentMessage{FileName: gproto.String("plan.pdf")}},
	} {
		if got := quoteOf(q); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if got := quoteFor(&message{Media: &media{Kind: proto.Sticker}}); got != "Sticker" {
		t.Errorf("sticker %q", got)
	}
}
