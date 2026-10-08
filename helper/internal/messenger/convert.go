// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"net/url"
	"strings"

	"go.mau.fi/mautrix-meta/pkg/messagix/methods"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// The thumbs-up "stickers" the Like button sends, in three sizes; Messenger
// draws them as the emoji.
const (
	thumbsUpSmall  = 369239263222822
	thumbsUpMedium = 369239343222814
	thumbsUpLarge  = 369239383222810
)

// stickerSize is how big Messenger draws every sticker.
const stickerSize = 96

// convertFB turns a message from a Messenger table into the backend's form;
// m.mu is held. It never fetches anything: attachments become files
// downloaded only on request.
func (m *Messenger) convertFB(c *chat, wm *table.WrappedMessage) *message {
	msg := &message{
		netID:     wm.MessageId,
		ms:        wm.TimestampMs,
		sender:    wm.SenderId,
		forwarded: wm.IsForwarded,
		editCount: wm.EditCount,
		edited:    wm.EditCount > 0,
		canUnsend: wm.CannotUnsendReason == table.CAN_UNSEND,
	}
	if msg.ms == 0 {
		msg.ms, _ = methods.ParseMessageID(wm.MessageId)
	}
	text := wm.Text
	stickers := wm.Stickers
	switch wm.StickerId {
	case thumbsUpSmall, thumbsUpMedium, thumbsUpLarge:
		if len(stickers) == 1 {
			text, stickers = "👍", nil
		}
	}
	if wm.IsAdminMessage {
		msg.service = oneLine(text)
		msg.canUnsend = false
		text = ""
	} else {
		msg.text = text
		msg.mentions = fbMentions(wm)
	}

	seen := map[string]bool{}
	for i, a := range wm.BlobAttachments {
		if a.AttachmentFbid != "" {
			// Messenger sometimes sends the same attachment twice.
			if seen[a.AttachmentFbid] {
				continue
			}
			seen[a.AttachmentFbid] = true
		}
		msg.media = append(msg.media, m.blobMedia(c, wm.MessageId, i, a))
	}
	for i, a := range wm.Attachments {
		msg.media = append(msg.media, m.legacyMedia(c, wm.MessageId, i, a))
	}
	for _, s := range stickers {
		msg.media = append(msg.media, m.stickerMedia(s))
	}
	for _, x := range wm.XMAAttachments {
		m.xma(msg, x)
	}
	if wm.ReplySourceId != "" {
		msg.reply = &replyRef{netID: wm.ReplySourceId, sender: wm.ReplyToUserId}
		switch {
		case wm.ReplyMessageText != "":
			msg.reply.text = snippet(wm.ReplyMessageText)
		case wm.ReplyAttachmentType != table.AttachmentTypeNone:
			msg.reply.text = label(attachmentKind(wm.ReplyAttachmentType, "", ""))
		}
		if replied := c.msgs[wm.ReplySourceId]; replied != nil {
			msg.reply.sender = replied.sender
			if msg.reply.text == "" {
				msg.reply.text = quoteOf(replied)
			}
		}
	}
	for _, r := range wm.Reactions {
		msg.setReaction(r.ActorId, r.Reaction)
	}
	linkOnly(msg)
	if msg.text == "" && msg.service == "" && len(msg.media) == 0 && msg.preview == nil && msg.unsupported == "" {
		msg.unsupported = "[Unsupported message]"
	}
	return msg
}

// fbMentions reads a message's mention lists (comma-separated, UTF-16).
func fbMentions(wm *table.WrappedMessage) []mention {
	parsed, err := (&socket.MentionData{
		MentionIDs: wm.MentionIds, MentionOffsets: wm.MentionOffsets,
		MentionLengths: wm.MentionLengths, MentionTypes: wm.MentionTypes,
	}).Parse()
	if err != nil {
		return nil
	}
	out := make([]mention, 0, len(parsed))
	for _, p := range parsed {
		mn := mention{offset: p.Offset, length: p.Length}
		if p.Type == socket.MentionTypePerson || p.Type == socket.MentionTypeSilent {
			mn.fbid = p.ID
		}
		out = append(out, mn)
	}
	return out
}

// attachmentKind is what an attachment is shown as.
func attachmentKind(t table.AttachmentType, mime, name string) proto.MediaKind {
	switch t {
	case table.AttachmentTypeImage, table.AttachmentTypeEphemeralImage:
		if mime == "image/gif" {
			return proto.GIF
		}
		return proto.Photo
	case table.AttachmentTypeAnimatedImage:
		return proto.GIF
	case table.AttachmentTypeVideo, table.AttachmentTypeEphemeralVideo:
		return proto.Video
	case table.AttachmentTypeAudio, table.AttachmentTypeSoundBite:
		if strings.HasPrefix(strings.ToLower(name), "audioclip") {
			return proto.Voice
		}
		return proto.Audio
	case table.AttachmentTypeSticker, table.AttachmentTypeSelfieSticker, table.AttachmentTypeThirdPartySticker:
		return proto.Sticker
	}
	switch {
	case strings.HasPrefix(mime, "image/"):
		return proto.Photo
	case strings.HasPrefix(mime, "video/"):
		return proto.Video
	case strings.HasPrefix(mime, "audio/"):
		return proto.Audio
	}
	return proto.FileMedia
}

// fileKey names an attachment by its own id, so its download is found
// again next run whatever URL it comes with.
func fileKey(prefix, fbid, rawURL string) string {
	if fbid != "" {
		return prefix + fbid
	}
	return prefix + avatarKey(rawURL)
}

// blobMedia is a photo, video, recording or file; m.mu is held.
func (m *Messenger) blobMedia(c *chat, msgID string, i int, a *table.LSInsertBlobAttachment) proto.Media {
	full, mime, expires := a.PlayableUrl, a.PlayableUrlMimeType, a.PlayableUrlExpirationTimestampMs
	if mime == "" {
		mime = a.AttachmentMimeType
	}
	if full == "" {
		full, mime, expires = a.PreviewUrl, a.PreviewUrlMimeType, a.PreviewUrlExpirationTimestampMs
	}
	kind := attachmentKind(a.AttachmentType, mime, a.Filename)
	if a.WaveformData != "" && kind == proto.Audio {
		kind = proto.Voice
	}
	md := proto.Media{
		Kind: kind, Name: a.Filename, Mime: mime, Size: a.Filesize,
		Width: int(a.PreviewWidth), Height: int(a.PreviewHeight), Duration: int(a.PlayableDurationMs / 1000),
	}
	if a.AttachmentType == table.AttachmentTypeEphemeralImage || a.AttachmentType == table.AttachmentTypeEphemeralVideo {
		md.ViewOnce = true
		return md
	}
	src := &fbSource{URL: full, Mime: mime, Expires: expires, Thread: c.threadKey(), MessageID: msgID, AttachmentID: a.AttachmentFbid, Part: i}
	md.FileID = m.register(fileKey("fb:", a.AttachmentFbid, full), full, mime, a.Filename, a.Filesize, src)
	md.Thumbnail = m.thumbnail(a.AttachmentFbid, full, a.PreviewUrl, a.PreviewUrlMimeType, a.PreviewWidth, a.PreviewHeight, md, src)
	return md
}

// legacyMedia is an attachment in the older table form, which view-once
// media still comes in; m.mu is held.
func (m *Messenger) legacyMedia(c *chat, msgID string, i int, a *table.LSInsertAttachment) proto.Media {
	full, mime, expires := a.PlayableUrl, a.PlayableUrlMimeType, a.PlayableUrlExpirationTimestampMs
	if mime == "" {
		mime = a.AttachmentMimeType
	}
	if full == "" {
		full, mime, expires = a.PreviewUrl, a.PreviewUrlMimeType, a.PreviewUrlExpirationTimestampMs
	}
	md := proto.Media{
		Kind: attachmentKind(a.AttachmentType, mime, a.Filename), Name: a.Filename, Mime: mime, Size: a.Filesize,
		Width: int(a.PreviewWidth), Height: int(a.PreviewHeight), Duration: int(a.PlayableDurationMs / 1000),
	}
	// The view mode's zero value is "view once", so it only counts on
	// media whose state says it's ephemeral at all.
	ephemeral := a.EphemeralMediaState != table.EphemeralMediaStatePermanent && a.EphemeralMediaViewMode != table.EphemeralMediaPermanent
	if ephemeral || a.AttachmentType == table.AttachmentTypeEphemeralImage || a.AttachmentType == table.AttachmentTypeEphemeralVideo {
		md.ViewOnce = true
		return md
	}
	src := &fbSource{URL: full, Mime: mime, Expires: expires, Thread: c.threadKey(), MessageID: msgID, AttachmentID: a.AttachmentFbid, Part: i, Legacy: true}
	md.FileID = m.register(fileKey("fb:", a.AttachmentFbid, full), full, mime, a.Filename, a.Filesize, src)
	md.Thumbnail = m.thumbnail(a.AttachmentFbid, full, a.PreviewUrl, a.PreviewUrlMimeType, a.PreviewWidth, a.PreviewHeight, md, src)
	return md
}

// thumbnail is the still to draw for an attachment: its preview image, or
// for a photo the photo itself; m.mu is held.
func (m *Messenger) thumbnail(fbid, full, preview, mime string, w, h int64, md proto.Media, src *fbSource) *proto.Image {
	if preview != "" && preview != full {
		s := *src
		s.URL, s.Mime, s.Preview = preview, mime, true
		id := m.register(fileKey("fb-preview:", fbid, preview), preview, mime, "preview", 0, &s)
		if id != 0 {
			return &proto.Image{FileID: id, Width: int(w), Height: int(h)}
		}
	}
	if (md.Kind == proto.Photo || md.Kind == proto.GIF || md.Kind == proto.Sticker) && md.FileID != 0 {
		return &proto.Image{FileID: md.FileID, Width: md.Width, Height: md.Height}
	}
	return nil
}

// stickerMedia is a sticker; m.mu is held.
func (m *Messenger) stickerMedia(s *table.LSInsertStickerAttachment) proto.Media {
	full, mime := s.PlayableUrl, s.PlayableUrlMimeType
	if full == "" {
		full, mime = s.PreviewUrl, s.PreviewUrlMimeType
	}
	if mime == "" {
		mime = s.ImageUrlMimeType
	}
	md := proto.Media{Kind: proto.Sticker, Mime: mime, Width: stickerSize, Height: stickerSize}
	md.FileID = m.register(fileKey("sticker:", s.AttachmentFbid, full), full, mime, "sticker", 0, &fbSource{URL: full, Mime: mime, Expires: s.PlayableUrlExpirationTimestampMs})
	if md.FileID != 0 {
		md.Thumbnail = &proto.Image{FileID: md.FileID, Width: stickerSize, Height: stickerSize}
	}
	return md
}

// xma reads a structured attachment: a link's preview, or a label for what
// tuimeta doesn't show (polls, locations, calls, shared posts); m.mu is held.
func (m *Messenger) xma(msg *message, x *table.WrappedXMA) {
	ctaType, action := "", x.ActionUrl
	if x.CTA != nil {
		ctaType = x.CTA.Type_
		if x.CTA.ActionUrl != "" {
			action = x.CTA.ActionUrl
		}
	}
	switch {
	case strings.HasPrefix(ctaType, "xma_poll"):
		setUnsupported(msg, "[Poll]")
		return
	case strings.Contains(ctaType, "location"):
		setUnsupported(msg, "[Location]")
		return
	case strings.HasPrefix(ctaType, "xma_rtc"), strings.Contains(ctaType, "call"):
		setUnsupported(msg, "[Call]")
		return
	case strings.Contains(ctaType, "story"):
		setUnsupported(msg, "[Story]")
		return
	}
	link := webLink(action)
	if link != "" && msg.preview == nil {
		p := &proto.LinkPreview{URL: link, Title: oneLine(x.TitleText), Description: oneLine(firstNonEmpty(x.SubtitleText, x.DescriptionText))}
		if x.PreviewUrl != "" {
			id := m.register(fileKey("xma:", x.AttachmentFbid, x.PreviewUrl), x.PreviewUrl, x.PreviewUrlMimeType, "preview", 0,
				&fbSource{URL: x.PreviewUrl, Mime: x.PreviewUrlMimeType, Expires: x.PreviewUrlExpirationTimestampMs})
			if id != 0 {
				p.Image = &proto.Image{FileID: id, Width: int(x.PreviewWidth), Height: int(x.PreviewHeight)}
			}
		}
		msg.preview = p
		return
	}
	if link == "" && msg.text == "" && len(msg.media) == 0 {
		setUnsupported(msg, "[Shared content]")
	}
}

// linkOnly gives a message that is only a shared link its link as text:
// tuimeta shows a preview only for a link the text has.
func linkOnly(msg *message) {
	if msg.text == "" && msg.service == "" && msg.preview != nil {
		msg.text = msg.preview.URL
	}
}

func setUnsupported(msg *message, label string) {
	if msg.unsupported == "" {
		msg.unsupported = label
	}
}

// webLink is the web address an attachment's action points to: Facebook's
// own redirect unwrapped, and only http(s) kept.
func webLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Path == "/l.php" && strings.HasSuffix(u.Hostname(), "facebook.com") {
		if inner := u.Query().Get("u"); inner != "" {
			return webLink(inner)
		}
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}
	return u.String()
}

// register gives a downloadable thing a file id; m.mu is held. A URL that
// isn't Meta's own media host gets none: nothing a sender wrote is fetched.
func (m *Messenger) register(key, rawURL, mime, name string, size int64, src *fbSource) int32 {
	if rawURL == "" || !allowedMediaURL(rawURL) {
		return 0
	}
	return m.d.Files.Register(ids.FileRef{Network: net, Key: key, Size: size, Mime: mime, Name: name, Source: src})
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// oneLine is text on one line, for service sentences and previews.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
