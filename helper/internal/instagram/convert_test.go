// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestTextKeepsInstagramsMentionsAsUTF16Entities(t *testing.T) {
	h := newHarness(t)
	// "👋 @maya.lens and @everyone, *see* you": the wave is two UTF-16 units,
	// so the mention starts at 3. @everyone keeps its words without an
	// entity, a range splitting the wave is left out, and the stars are
	// Instagram's bold.
	msg := `{"id":"mid.1","sender_fbid":"2003","timestamp_ms":"1759912345678",
		"content":{"__typename":"SlideMessageText","text_body":"👋 @maya.lens and @everyone, *see* you"},
		"mentions":[{"offset":3,"length":10,"profile_range_type":"PROFILE","user_fbid":"2002"},
		            {"offset":18,"length":9,"profile_range_type":"THREAD","user_fbid":"8123456789012345"},
		            {"offset":1,"length":1,"profile_range_type":"PROFILE","user_fbid":"2004"}]}`
	c, parts := h.convertOne(groupThread(), msg)
	if len(parts) != 1 {
		t.Fatalf("parts = %d", len(parts))
	}
	m := parts[0]
	if m.Text != "👋 @maya.lens and @everyone, see you" || m.ChatID != c.id || m.SenderID != h.userID(samFBID) || m.Outgoing {
		t.Errorf("message = %+v", m)
	}
	want := []proto.Entity{
		{Offset: 3, Length: 10, Type: proto.Mention, UserID: h.userID(mayaFBID)},
		{Offset: 29, Length: 3, Type: proto.Bold},
	}
	if len(m.Entities) != 2 || m.Entities[0] != want[0] || m.Entities[1] != want[1] {
		t.Errorf("entities = %+v, want %+v", m.Entities, want)
	}
	if got := ids.Millis(m.ID); got != 1759912345678 || m.Date != 1759912345 {
		t.Errorf("id from %d ms, date %d", got, m.Date)
	}
}

func TestAPhotoAlbumBecomesConsecutivePartsWithTheReplyFirstAndNothingDownloaded(t *testing.T) {
	h := newHarness(t)
	att := func(n int) string {
		return fmt.Sprintf(`{"__typename":"SlideMessagingImageAttachment","attachment_fbid":"77%d",
			"attachment_cdn_url":"%s/v/t1.15752-9/full%d_n.jpg?oh=sig&oe=AAA",
			"preview_cdn_url":"%s/v/t1.15752-9/prev%d_n.jpg?oh=sig&oe=AAA","preview_width":%d,"preview_height":480}`, n, cdn, n, cdn, n, 600+n)
	}
	msg := fmt.Sprintf(`{"id":"mid.album","sender_fbid":"2002","timestamp_ms":"%d",
		"content":{"__typename":"SlideMessageImageContent","attachments":[%s,%s,%s]},
		"replied_to_message_id":"mid.q","reactions":[{"reaction":"❤️","sender_fbid":"1000"}]}`, tsBase, att(1), att(2), att(3))
	_, parts := h.convertOne(dmThread(), msg)
	if len(parts) != 3 {
		t.Fatalf("parts = %d", len(parts))
	}
	for i, p := range parts {
		if p.ID != parts[0].ID+int64(i) || p.Album != parts[0].ID {
			t.Errorf("part %d: id %d album %d", i, p.ID, p.Album)
		}
		if p.Media == nil || p.Media.Kind != proto.Photo || p.Media.FileID == 0 || p.Media.Thumbnail == nil || p.Media.ViewOnce {
			t.Fatalf("part %d media = %+v", i, p.Media)
		}
		if (p.ReplyTo != nil) != (i == 0) || (len(p.Reactions) > 0) != (i == 0) {
			t.Errorf("part %d: reply %v reactions %v", i, p.ReplyTo, p.Reactions)
		}
		ref, ok := h.deps.Files.Get(p.Media.FileID)
		if !ok || ref.Key != fmt.Sprintf("att:77%d:file", i+1) || ref.Mime != "image/jpeg" {
			t.Errorf("part %d file = %+v", i, ref)
		}
		if p.Media.Thumbnail.FileID == p.Media.FileID || p.Media.Thumbnail.Width != 601+i {
			t.Errorf("part %d thumbnail = %+v", i, p.Media.Thumbnail)
		}
	}
	if !parts[0].Reactions[0].Mine {
		t.Error("your reaction isn't marked yours")
	}
	// The answered message isn't known: no id, but the reply is kept.
	if parts[0].ReplyTo.MessageID != 0 {
		t.Errorf("reply_to = %+v", parts[0].ReplyTo)
	}
}

func TestViewOnceAndVanishingMediaHaveNoFileAndNoThumbnail(t *testing.T) {
	att := `{"attachment_fbid":"991","attachment_cdn_url":"` + cdn + `/v/t1/secret_n.jpg?oh=1","preview_cdn_url":"` + cdn + `/v/t1/secretprev_n.jpg?oh=1","preview_width":640,"preview_height":640}`
	cases := []struct {
		name, content, extra string
		kind                 proto.MediaKind
		hidden               bool
	}{
		{"seen once", `{"__typename":"SlideMessageRavenImageContent","view_mode":0,"attachment":` + att + `}`, "", proto.Photo, true},
		{"replayed once", `{"__typename":"SlideMessageRavenVideoContent","view_mode":1,"attachment":` + att + `}`, "", proto.Video, true},
		{"already seen", `{"__typename":"SlideMessageRavenImageContent","view_mode":0,"attachment":null}`, "", proto.Photo, true},
		{"vanish mode", `{"__typename":"SlideMessageImageContent","attachments":[` + att + `]}`, `,"view_expiration_timestamp_ms":"1759912999999"`, proto.Photo, true},
		{"expiring", `{"__typename":"SlideMessageVideosContent","videos":[` + att + `]}`, `,"expiration_timestamp_ms":"1759912999999"`, proto.Video, true},
		{"kept in chat", `{"__typename":"SlideMessageRavenImageContent","view_mode":2,"attachment":` + att + `}`, "", proto.Photo, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			msg := fmt.Sprintf(`{"id":"mid.raven","sender_fbid":"2002","timestamp_ms":"%d","content":%s%s}`, tsBase, tc.content, tc.extra)
			_, parts := h.convertOne(dmThread(), msg)
			m := parts[0].Media
			if m == nil || m.Kind != tc.kind {
				t.Fatalf("media = %+v", m)
			}
			if !tc.hidden {
				if m.ViewOnce || m.FileID == 0 {
					t.Errorf("a photo kept in the chat is ordinary media: %+v", m)
				}
				return
			}
			if !m.ViewOnce || m.FileID != 0 || m.Thumbnail != nil {
				t.Errorf("hidden media = %+v", m)
			}
			// Nothing of it can be downloaded: no file id was handed out.
			if _, ok := h.deps.Files.Get(1); ok {
				t.Error("a file was registered for media shown only once")
			}
		})
	}
}

func TestAReplyQuotesInstagramsSnippetAndTheQuotedMessageCanBeFetched(t *testing.T) {
	h := newHarness(t)
	quoted := strings.Replace(textMsg("mid.q", mayaFBID, tsBase-86_400_000, "Are you coming to the reunion?"), `"reactions":[]`,
		`"reactions":[],"igd_snippet":"Are you coming…"`, 1)
	msg := fmt.Sprintf(`{"id":"mid.r","sender_fbid":"1000","timestamp_ms":"%d",
		"content":{"__typename":"SlideMessageText","text_body":"Yes!"},"replied_to_message_id":"mid.q","replied_to_message":%s}`, tsBase, quoted)
	c, parts := h.convertOne(dmThread(), msg)
	r := parts[0].ReplyTo
	if r == nil || r.SenderID != h.userID(mayaFBID) || r.Text != "Are you coming…" {
		t.Fatalf("reply_to = %+v", r)
	}
	known, ok := h.deps.Messages.Known(c.id, "mid.q")
	if !ok || r.MessageID != known[0] || ids.Millis(r.MessageID) != tsBase-86_400_000 {
		t.Fatalf("reply_to.message_id = %d, known %v", r.MessageID, known)
	}
	// The quoted message answers get_message without a request.
	h.b.conn = nil
	got, err := h.b.GetMessage(t.Context(), h.msgRef(mayaFBID, "mid.q"))
	if err != nil || got.Text != "Are you coming to the reunion?" || got.ID != r.MessageID {
		t.Errorf("get_message = %+v, %v", got, err)
	}
	// A quoted copy doesn't count as unread.
	h.b.mu.Lock()
	unread := c.unread(selfFBID)
	h.b.mu.Unlock()
	if unread != 0 {
		t.Errorf("unread = %d", unread)
	}
}

func TestReactionsAreOnePerPersonAndYoursAreMarked(t *testing.T) {
	h := newHarness(t)
	msg := fmt.Sprintf(`{"id":"mid.re","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageText","text_body":"Saturday?"},
		"reactions":[{"reaction":"❤️","sender_fbid":"1000"},{"reaction":"❤️","sender_fbid":"2003"},{"reaction":"🔥","sender_fbid":"2004"},
		             {"reaction":"😂","sender_fbid":"2003"}]}`, tsBase)
	_, parts := h.convertOne(groupThread(), msg)
	got := parts[0].Reactions
	want := []proto.Reaction{{Emoji: "❤️", Count: 1, Mine: true}, {Emoji: "😂", Count: 1}, {Emoji: "🔥", Count: 1}}
	if len(got) != 3 {
		t.Fatalf("reactions = %+v", got)
	}
	counts := map[string]proto.Reaction{}
	for _, r := range got {
		counts[r.Emoji] = r
	}
	for _, w := range want {
		if counts[w.Emoji] != w {
			t.Errorf("%s = %+v, want %+v", w.Emoji, counts[w.Emoji], w)
		}
	}
}

func TestYourTextMessagesAreEditableWithinInstagramsLimits(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UnixMilli()
	_, mine := h.convertOne(dmThread(), textMsg("mid.mine", selfFBID, now, "on my way"))
	if !mine[0].Outgoing || mine[0].EditableUntil != now/1000+15*60 || !mine[0].Deletable || mine[0].Edited {
		t.Errorf("your text = %+v", mine[0])
	}
	_, theirs := h.convertOne(dmThread(), textMsg("mid.theirs", mayaFBID, now, "ok"))
	if theirs[0].EditableUntil != 0 || theirs[0].Deletable {
		t.Errorf("their text = %+v", theirs[0])
	}
	edited := strings.Replace(textMsg("mid.edited", selfFBID, now, "fixed"), `"reactions":[]`,
		`"reactions":[],"slide_edit_history":[{"body":"a"},{"body":"b"},{"body":"c"},{"body":"d"},{"body":"e"}]`, 1)
	_, worn := h.convertOne(dmThread(), edited)
	if !worn[0].Edited || worn[0].EditableUntil != 0 {
		t.Errorf("edited five times = %+v", worn[0])
	}
	photo := fmt.Sprintf(`{"id":"mid.p","sender_fbid":"1000","timestamp_ms":"%d","content":{"__typename":"SlideMessageImageContent",
		"attachments":[{"attachment_fbid":"5","attachment_cdn_url":"%s/v/x_n.jpg"}]}}`, now, cdn)
	_, pic := h.convertOne(dmThread(), photo)
	if pic[0].EditableUntil != 0 || !pic[0].Deletable {
		t.Errorf("your photo = %+v", pic[0])
	}
}

func TestSharedLinksBecomeCardsFromInstagramsOwnDataWithoutFetchingThem(t *testing.T) {
	h := newHarness(t)
	msg := fmt.Sprintf(`{"id":"mid.link","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageXMAContent",
		"xma_text_body":"This is the route",
		"xma":{"target_url":"https://l.facebook.com/l.php?u=https%%3A%%2F%%2Fexample.com%%2Froutes%%2Farete&h=AT0",
		       "title_text":"The Arête","subtitle_text":"A classic 5.8",
		       "preview_image":{"url":"%s/v/t15/arete_n.jpg?oh=sig","width":600,"height":315}}}}`, tsBase, cdn)
	_, parts := h.convertOne(dmThread(), msg)
	m := parts[0]
	lp := m.LinkPreview
	if lp == nil || lp.URL != "https://example.com/routes/arete" || lp.Title != "The Arête" || lp.Description != "A classic 5.8" {
		t.Fatalf("link_preview = %+v", lp)
	}
	if lp.Image == nil || lp.Image.Width != 600 {
		t.Errorf("preview image = %+v", lp.Image)
	}
	if m.Text != "This is the route\nhttps://example.com/routes/arete" || m.Unsupported != "" {
		t.Errorf("text = %q, unsupported %q", m.Text, m.Unsupported)
	}
	// noNetwork fails the test if anything was fetched.
}

func TestSharesWithoutAnAddressAreLabelled(t *testing.T) {
	cases := map[string]string{
		`"target_url":"","title_text":"Post"`:                     "[Shared post]",
		`"target_url":"instagram://stories/123","title_text":"s"`: "[Shared post]",
		`"target_url":"javascript:alert(1)"`:                      "[Shared post]",
		`"target_url":"ftp://instagram.com/stories/user/1"`:       "[Shared story]",
		`"target_url":"/reel/Cabc/"`:                              "[Shared reel]",
	}
	for xma, label := range cases {
		h := newHarness(t)
		msg := fmt.Sprintf(`{"id":"mid.x","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageXMAContent","xma":{%s}}}`, tsBase, xma)
		_, parts := h.convertOne(dmThread(), msg)
		if parts[0].Unsupported != label || parts[0].LinkPreview != nil {
			t.Errorf("%s: unsupported %q, preview %+v", xma, parts[0].Unsupported, parts[0].LinkPreview)
		}
	}
}

func TestPostsSharedWithAnAddressNameIt(t *testing.T) {
	h := newHarness(t)
	msg := fmt.Sprintf(`{"id":"mid.post","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageXMAContent",
		"xma":{"target_id":"3141592653","target_url":"https://www.instagram.com/p/C0ffee/?igsh=x","header_title_text":"maya.lens","caption_body_text":"Sunset"}}}`, tsBase)
	_, parts := h.convertOne(dmThread(), msg)
	m := parts[0]
	if m.LinkPreview == nil || m.LinkPreview.URL != "https://www.instagram.com/p/C0ffee/?igsh=x" || m.LinkPreview.Title != "maya.lens" || m.LinkPreview.Description != "Sunset" {
		t.Fatalf("preview = %+v", m.LinkPreview)
	}
	if m.Text != "https://www.instagram.com/p/C0ffee/?igsh=x" {
		t.Errorf("text = %q", m.Text)
	}
}

func TestEventsAreServiceSentencesAndReactionNotesAreLeftOut(t *testing.T) {
	h := newHarness(t)
	msg := fmt.Sprintf(`{"id":"mid.ev","sender_fbid":"2003","timestamp_ms":"%d","content":{"__typename":"SlideMessageAdminText",
		"text_fragments":[{"plaintext":"Sam named the group "},{"plaintext":"Climbing crew 🧗"}]}}`, tsBase)
	group, parts := h.convertOne(groupThread(), msg)
	if parts[0].Service != "Sam named the group Climbing crew 🧗" || parts[0].Text != "" || parts[0].Deletable || parts[0].EditableUntil != 0 {
		t.Errorf("event = %+v", parts[0])
	}
	h.b.mu.Lock()
	n := h.b.convert(group, message(t, fmt.Sprintf(`{"id":"mid.rl","sender_fbid":"2002","timestamp_ms":"%d",
		"content":{"__typename":"SlideMessageAdminText","is_reaction_action_log":true,"text_fragments":[{"plaintext":"Maya reacted ❤️ to your message"}]}}`, tsBase)))
	h.b.mu.Unlock()
	if n == nil || !n.skip {
		t.Error("a reaction note is shown as a message")
	}
}

func TestVoiceMessagesStickersAndGIFs(t *testing.T) {
	h := newHarness(t)
	voice := fmt.Sprintf(`{"id":"mid.v","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageAudiosContent",
		"audio_attachments":[{"attachment_fbid":"881","playable_duration_ms":4200,"attachment_cdn_url":"%s/v/t59/voice_n.mp4?oh=s"}]}}`, tsBase, cdn)
	_, parts := h.convertOne(dmThread(), voice)
	if m := parts[0].Media; m.Kind != proto.Voice || m.Duration != 5 || m.Mime != "audio/mp4" || m.FileID == 0 {
		t.Errorf("voice = %+v", m)
	}
	anim := fmt.Sprintf(`{"id":"mid.a","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageAnimatedMediaContent",
		"animated_media":[{"is_sticker":true,"attachment_webp_url":"%s/v/st/hi.webp?x=1","attachment_mp4_url":"%s/v/st/hi.mp4","preview_width":200,"preview_height":200},
		                  {"is_sticker":false,"attachment_mp4_url":"%s/v/gif/dance.mp4","preview_cdn_url":"%s/v/gif/dance.jpg","preview_width":320,"preview_height":240}]}}`,
		tsBase+1, cdn, cdn, cdn, cdn)
	_, parts = h.convertOne(dmThread(), anim)
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	if m := parts[0].Media; m.Kind != proto.Sticker || m.Mime != "image/webp" || m.Thumbnail == nil || m.Thumbnail.FileID != m.FileID {
		t.Errorf("sticker = %+v", m)
	}
	if m := parts[1].Media; m.Kind != proto.GIF || m.Mime != "video/mp4" || m.Thumbnail == nil || m.Thumbnail.FileID == m.FileID {
		t.Errorf("gif = %+v", m)
	}
	sticker := fmt.Sprintf(`{"id":"mid.s","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageStoreStickerContent",
		"preview_url":"%s/v/t39/sticker.png?oh=1","preview_width":160,"preview_height":160,"alt_text":"cat"}}`, tsBase+2, cdn)
	_, parts = h.convertOne(dmThread(), sticker)
	if m := parts[0].Media; m.Kind != proto.Sticker || m.Mime != "image/png" || m.Width != 160 {
		t.Errorf("store sticker = %+v", m)
	}
}

func TestWhatCantBeShownIsLabelled(t *testing.T) {
	h := newHarness(t)
	for typename, label := range map[string]string{
		"SlideMessagePollContent":            "[Unsupported message]",
		"SlideMessageMusicStickerXMAContent": "[Music]",
	} {
		msg := fmt.Sprintf(`{"id":"mid.%s","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":%q}}`, typename, tsBase, typename)
		_, parts := h.convertOne(dmThread(), msg)
		if parts[0].Unsupported != label || parts[0].Media != nil {
			t.Errorf("%s = %+v", typename, parts[0])
		}
	}
}

func TestFilesKeepTheirIDAndKeyWhenTheirAddressChanges(t *testing.T) {
	h := newHarness(t)
	at := func(host, sig string) string {
		return fmt.Sprintf(`{"id":"mid.f","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageVideosContent",
			"videos":[{"attachment_fbid":"4242","attachment_cdn_url":"%s/v/t42/clip_n.mp4?oh=%s","preview_cdn_url":"%s/v/t42/clip_n.jpg?oh=%s"}]}}`,
			tsBase, host, sig, host, sig)
	}
	_, first := h.convertOne(dmThread(), at(cdn, "old"))
	_, again := h.convertOne(dmThread(), at(otherCDN, "new"))
	if first[0].Media.FileID != again[0].Media.FileID || first[0].Media.Thumbnail.FileID != again[0].Media.Thumbnail.FileID {
		t.Fatalf("file ids changed: %+v then %+v", first[0].Media, again[0].Media)
	}
	ref, _ := h.deps.Files.Get(again[0].Media.FileID)
	if ref.Key != "att:4242:file" || ref.Mime != "video/mp4" || ref.Source.(*mediaSource).url != otherCDN+"/v/t42/clip_n.mp4?oh=new" {
		t.Errorf("file = %+v %+v", ref, ref.Source)
	}
	// Keyed by path, not by which CDN server or signature.
	if k1, k2 := fileKey("", cdn+"/v/a.jpg?oh=1", "file"), fileKey("", otherCDN+"/v/a.jpg?oh=2", "file"); k1 != k2 {
		t.Errorf("keys %q and %q differ", k1, k2)
	}
}
