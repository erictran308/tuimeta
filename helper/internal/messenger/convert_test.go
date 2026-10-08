// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"slices"
	"strings"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestAnIncomingMessageHasItsMarkersAsEntitiesAndItsMentions(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{groupThread(groupID, "Trip", -10)}})
	h.load()
	h.rec.reset()
	m := textMsg(groupID, "mid.$a1", aliceID, -1, "😀 *see* @Ben Carter at _5_")
	// Mention offsets are UTF-16: the emoji is two units.
	m.MentionIds, m.MentionOffsets, m.MentionLengths, m.MentionTypes = "100003", "9", "11", "p"
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{m}})

	got := h.rec.messages()
	if len(got) != 1 {
		t.Fatalf("messages: %+v", got)
	}
	msg := got[0]
	if msg.Text != "😀 see @Ben Carter at 5" {
		t.Errorf("text %q", msg.Text)
	}
	want := []proto.Entity{
		{Offset: 3, Length: 3, Type: proto.Bold},
		{Offset: 7, Length: 11, Type: proto.Mention, UserID: h.userID(benID)},
		{Offset: 22, Length: 1, Type: proto.Italic},
	}
	if !slices.Equal(msg.Entities, want) {
		t.Errorf("entities %+v, want %+v", msg.Entities, want)
	}
	if msg.SenderID != h.userID(aliceID) || msg.Outgoing || msg.Date != ms(-1)/1000 || ids.Millis(msg.ID) != ms(-1) {
		t.Errorf("message %+v", msg)
	}
	if msg.EditableUntil != 0 || msg.Deletable {
		t.Errorf("someone else's message is editable or deletable: %+v", msg)
	}
	// The chat moved, and its unread count went up.
	chats := h.rec.chats()
	if len(chats) != 1 || chats[0].Unread != 1 || chats[0].LastMessage == nil || chats[0].LastMessage.ID != msg.ID {
		t.Errorf("chat after the message: %+v", chats)
	}
}

func TestSeveralAttachmentsBecomeAnAlbumWithTheCaptionLast(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.rec.reset()
	blob := func(fbid string, i int64, kind table.AttachmentType, mime string) *table.LSInsertBlobAttachment {
		return &table.LSInsertBlobAttachment{
			ThreadKey: aliceID, MessageId: "mid.$album", AttachmentFbid: fbid, AttachmentType: kind,
			PlayableUrl: "https://video.xx.fbcdn.net/v/" + fbid + "?oe=1", PlayableUrlMimeType: mime,
			PreviewUrl: "https://scontent.xx.fbcdn.net/v/p" + fbid + ".jpg?oe=1", PreviewUrlMimeType: "image/jpeg",
			PreviewWidth: 640, PreviewHeight: 480, Filesize: 1000 + i, Filename: "IMG_" + fbid + ".jpg", PlayableDurationMs: 3500,
		}
	}
	tbl := &table.LSTable{
		LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$album", aliceID, -2, "our *trip*")},
		LSInsertBlobAttachment: []*table.LSInsertBlobAttachment{
			blob("11", 1, table.AttachmentTypeImage, "image/jpeg"),
			blob("12", 2, table.AttachmentTypeVideo, "video/mp4"),
			blob("11", 1, table.AttachmentTypeImage, "image/jpeg"), // Messenger's duplicate
			blob("13", 3, table.AttachmentTypeAnimatedImage, "image/gif"),
		},
	}
	h.apply(tbl)
	parts := h.rec.messages()
	if len(parts) != 3 {
		t.Fatalf("parts: %d", len(parts))
	}
	for i, p := range parts {
		if p.Album != parts[0].ID || (i > 0 && p.ID != parts[i-1].ID+1) {
			t.Errorf("part %d ids: %d album %d", i, p.ID, p.Album)
		}
		if (p.Text != "") != (i == 2) {
			t.Errorf("part %d text %q", i, p.Text)
		}
		if p.Media == nil || p.Media.FileID == 0 || p.Media.Thumbnail == nil {
			t.Fatalf("part %d media %+v", i, p.Media)
		}
	}
	if parts[0].Media.Kind != proto.Photo || parts[1].Media.Kind != proto.Video || parts[2].Media.Kind != proto.GIF {
		t.Errorf("kinds %v %v %v", parts[0].Media.Kind, parts[1].Media.Kind, parts[2].Media.Kind)
	}
	if v := parts[1].Media; v.Duration != 3 || v.Width != 640 || v.Size != 1002 || v.Mime != "video/mp4" {
		t.Errorf("video %+v", v)
	}
	if parts[2].Text != "our trip" || len(parts[2].Entities) != 1 {
		t.Errorf("caption %q %+v", parts[2].Text, parts[2].Entities)
	}
	// Files are named by the attachment, not the expiring URL.
	ref, ok := h.deps.Files.Get(parts[0].Media.FileID)
	if !ok || ref.Key != "fb:11" || ref.Name != "IMG_11.jpg" {
		t.Errorf("file ref %+v", ref)
	}
}

func TestARepliesQuoteAndTheMessageItAnswers(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$q", selfID, -5, "when do we *leave*?")}})
	first := h.rec.messages()[0]
	h.rec.reset()
	reply := textMsg(aliceID, "mid.$r", aliceID, -4, "at 5")
	reply.ReplySourceId, reply.ReplyToUserId, reply.ReplyMessageText = "mid.$q", selfID, "when do we *leave*?"
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{reply}})
	got := h.rec.messages()[0]
	if got.ReplyTo == nil || got.ReplyTo.MessageID != first.ID || got.ReplyTo.SenderID != h.userID(selfID) || got.ReplyTo.Text != "when do we leave?" {
		t.Errorf("reply_to %+v", got.ReplyTo)
	}
	// Your own text message can be edited for 15 minutes, and unsent.
	if !first.Outgoing || first.EditableUntil != ms(-5)/1000+EditWindow || !first.Deletable {
		t.Errorf("own message %+v", first)
	}
}

func TestReactionsAreOnePerPersonAndYoursAreMarked(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{groupThread(groupID, "Trip", -10)}})
	h.load()
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(groupID, "mid.$x", benID, -3, "hi")}})
	h.rec.reset()
	h.apply(&table.LSTable{LSUpsertReaction: []*table.LSUpsertReaction{
		{ThreadKey: groupID, MessageId: "mid.$x", ActorId: aliceID, Reaction: "❤"},
		{ThreadKey: groupID, MessageId: "mid.$x", ActorId: selfID, Reaction: "❤️"},
		{ThreadKey: groupID, MessageId: "mid.$x", ActorId: aliceID, Reaction: "😂"}, // replaces Alice's
	}})
	msgs := h.rec.messages()
	last := msgs[len(msgs)-1]
	want := []proto.Reaction{{Emoji: "❤️", Count: 1, Mine: true}, {Emoji: "😂", Count: 1}}
	if !slices.Equal(last.Reactions, want) {
		t.Errorf("reactions %+v", last.Reactions)
	}
	h.apply(&table.LSTable{LSDeleteReaction: []*table.LSDeleteReaction{{ThreadKey: groupID, MessageId: "mid.$x", ActorId: selfID}}})
	msgs = h.rec.messages()
	if r := msgs[len(msgs)-1].Reactions; len(r) != 1 || r[0].Emoji != "😂" || r[0].Mine {
		t.Errorf("after removing yours: %+v", r)
	}
}

func TestEditsAndUnsendsUpdateTheMessage(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$e", aliceID, -3, "helo")}})
	orig := h.rec.messages()[0]
	h.rec.reset()
	h.apply(&table.LSTable{LSEditMessage: []*table.LSEditMessage{{MessageID: "mid.$e", Text: "*hello*", EditCount: 1}}})
	got := h.rec.messages()
	if len(got) != 1 || got[0].ID != orig.ID || got[0].Text != "hello" || !got[0].Edited || len(got[0].Entities) != 1 {
		t.Fatalf("edited: %+v", got)
	}
	// An older edit arriving late changes nothing.
	h.rec.reset()
	h.apply(&table.LSTable{LSEditMessage: []*table.LSEditMessage{{MessageID: "mid.$e", Text: "stale", EditCount: 0}}})
	h.apply(&table.LSTable{LSDeleteMessage: []*table.LSDeleteMessage{{ThreadKey: aliceID, MessageId: "mid.$e"}}})
	del := h.rec.of("message_deleted")
	if len(del) != 1 || del[0].(proto.MessageDeletedEvent).MessageIDs[0] != orig.ID {
		t.Errorf("deleted: %+v", del)
	}
	if c := h.chat(aliceID); c.log.Len() != 0 {
		t.Error("the unsent message is still in the history")
	}
}

func TestAdminMessagesAreServiceSentences(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{groupThread(groupID, "Trip", -10)}})
	h.load()
	h.rec.reset()
	m := textMsg(groupID, "mid.$adm", aliceID, -1, "Alice named the group\n*Trip*")
	m.IsAdminMessage = true
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{m}})
	got := h.rec.messages()[0]
	if got.Service != "Alice named the group *Trip*" || got.Text != "" || got.Deletable {
		t.Errorf("service %+v", got)
	}
	if c := h.rec.chats(); c[len(c)-1].Unread != 0 {
		t.Error("an event counted as unread")
	}
}

func TestLinkPreviewsComeFromMessengersOwnDataOnly(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.rec.reset()
	h.apply(&table.LSTable{
		LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$link", aliceID, -1, "")},
		LSInsertXmaAttachment: []*table.LSInsertXmaAttachment{{
			ThreadKey: aliceID, MessageId: "mid.$link", AttachmentFbid: "77",
			TitleText: "A post", SubtitleText: "about\nthings", PreviewUrl: "https://external.xx.fbcdn.net/safe_image.php?d=1",
			PreviewWidth: 600, PreviewHeight: 315,
		}},
		LSInsertAttachmentCta: []*table.LSInsertAttachmentCta{{
			AttachmentFbid: "77", MessageId: "mid.$link", Type_: "xma_web_url",
			ActionUrl: "https://l.facebook.com/l.php?u=https%3A%2F%2Fexample.com%2Fpost&h=AT0",
		}},
	})
	got := h.rec.messages()[0]
	p := got.LinkPreview
	if p == nil || p.URL != "https://example.com/post" || p.Title != "A post" || p.Description != "about things" || p.Image == nil || p.Image.FileID == 0 {
		t.Fatalf("preview %+v", p)
	}
	// A message that's only a link has the link as its text.
	if got.Text != "https://example.com/post" {
		t.Errorf("text %q", got.Text)
	}
}

func TestContentTuimetaDoesntShowGetsALabel(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.rec.reset()
	xma := func(id, cta string) *table.LSTable {
		return &table.LSTable{
			LSInsertMessage:       []*table.LSInsertMessage{textMsg(aliceID, id, aliceID, -1, "")},
			LSInsertXmaAttachment: []*table.LSInsertXmaAttachment{{ThreadKey: aliceID, MessageId: id, AttachmentFbid: id + "x", TitleText: "x"}},
			LSInsertAttachmentCta: []*table.LSInsertAttachmentCta{{AttachmentFbid: id + "x", MessageId: id, Type_: cta}},
		}
	}
	h.apply(xma("mid.$poll", "xma_poll_details_card"))
	h.apply(xma("mid.$loc", "xma_live_location_sharing"))
	h.apply(xma("mid.$call", "xma_rtc_missed_audio"))
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$empty", aliceID, -1, "")}})
	var labels []string
	for _, m := range h.rec.messages() {
		labels = append(labels, m.Unsupported)
	}
	want := []string{"[Poll]", "[Location]", "[Call]", "[Unsupported message]"}
	if !slices.Equal(labels, want) {
		t.Errorf("labels %q", labels)
	}
}

func TestStickersLikesVoiceAndViewOnce(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.rec.reset()
	like := textMsg(aliceID, "mid.$like", aliceID, -4, "")
	like.StickerId = thumbsUpLarge
	h.apply(&table.LSTable{
		LSInsertMessage:           []*table.LSInsertMessage{like, textMsg(aliceID, "mid.$st", aliceID, -3, "")},
		LSInsertStickerAttachment: []*table.LSInsertStickerAttachment{{MessageId: "mid.$like", AttachmentFbid: "s1", PreviewUrl: "https://scontent.xx.fbcdn.net/s1.png"}, {MessageId: "mid.$st", AttachmentFbid: "s2", PreviewUrl: "https://scontent.xx.fbcdn.net/s2.png", PreviewUrlMimeType: "image/png"}},
	})
	h.apply(&table.LSTable{
		LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$voice", aliceID, -2, ""), textMsg(aliceID, "mid.$once", aliceID, -1, "")},
		LSInsertBlobAttachment: []*table.LSInsertBlobAttachment{
			{MessageId: "mid.$voice", AttachmentFbid: "v1", AttachmentType: table.AttachmentTypeAudio, Filename: "audioclip-1.mp4", PlayableUrl: "https://cdn.fbsbx.com/v1.mp4", PlayableUrlMimeType: "audio/mpeg", PlayableDurationMs: 4200},
			{MessageId: "mid.$once", AttachmentFbid: "o1", AttachmentType: table.AttachmentTypeEphemeralImage, PlayableUrl: "https://scontent.xx.fbcdn.net/o1.jpg"},
		},
	})
	got := h.rec.messages()
	if len(got) != 4 {
		t.Fatalf("messages %d", len(got))
	}
	if got[0].Text != "👍" || got[0].Media != nil {
		t.Errorf("like %+v", got[0])
	}
	if s := got[1].Media; s == nil || s.Kind != proto.Sticker || s.FileID == 0 || s.Width != stickerSize {
		t.Errorf("sticker %+v", s)
	}
	if v := got[2].Media; v == nil || v.Kind != proto.Voice || v.Duration != 4 {
		t.Errorf("voice %+v", v)
	}
	if o := got[3].Media; o == nil || !o.ViewOnce || o.FileID != 0 || o.Thumbnail != nil {
		t.Errorf("view once %+v", o)
	}
}

func TestAnAttachmentNotOnMetasServersIsNeverAFile(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.rec.reset()
	h.apply(&table.LSTable{
		LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$evil", aliceID, -1, "")},
		LSInsertBlobAttachment: []*table.LSInsertBlobAttachment{{
			MessageId: "mid.$evil", AttachmentFbid: "e1", AttachmentType: table.AttachmentTypeImage,
			PlayableUrl: "https://tracker.example.com/pixel.jpg", PreviewUrl: "http://scontent.xx.fbcdn.net/plain-http.jpg",
		}},
	})
	md := h.rec.messages()[0].Media
	if md == nil || md.FileID != 0 || md.Thumbnail != nil {
		t.Errorf("media %+v", md)
	}
	for _, url := range []string{"https://scontent.xx.fbcdn.net/a", "https://www.facebook.com/x", "https://cdn.fbsbx.com/y", "https://mmg.whatsapp.net/z"} {
		if !allowedMediaURL(url) {
			t.Errorf("%s refused", url)
		}
	}
	for _, url := range []string{"https://fbcdn.net.example.com/a", "http://scontent.xx.fbcdn.net/a", "https://user:pw@scontent.xx.fbcdn.net/a", "file:///etc/passwd", "https://example.com/fbcdn.net"} {
		if allowedMediaURL(url) {
			t.Errorf("%s allowed", url)
		}
	}
}

func TestWebLinksAreOnlyHTTPAndUnwrapped(t *testing.T) {
	cases := map[string]string{
		"https://l.facebook.com/l.php?u=https%3A%2F%2Fa.example%2F&h=x": "https://a.example/",
		"http://plain.example/x":                               "http://plain.example/x",
		"fb-messenger://community_subthread":                   "",
		"javascript:alert(1)":                                  "",
		"https://evil.example/l.php?u=https%3A%2F%2Fb.example": "https://evil.example/l.php?u=https%3A%2F%2Fb.example",
	}
	for in, want := range cases {
		if got := webLink(in); got != want {
			t.Errorf("webLink(%q) = %q, want %q", in, got, want)
		}
	}
	if !strings.HasPrefix(oneLine(" a \n b\tc "), "a b c") {
		t.Error("oneLine")
	}
}
