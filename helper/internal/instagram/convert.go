// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"cmp"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf16"

	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
	"go.mau.fi/util/jsontime"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/metatext"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Instagram's limits on editing your own text messages (the connector's
// capabilities): fifteen minutes, five edits.
const (
	editWindow = 15 * 60
	maxEdits   = 5
)

// maxParts is the most attachments one message is split into; the ids
// package won't give a message more.
const maxParts = 64

// netMsg is one Instagram message as the backend keeps it, whatever its
// parts.
type netMsg struct {
	netID, otid string
	ms          int64
	sender      int64 // the sender's messaging id (fbid)
	mine        bool

	text     string
	mentions slidetypes.MentionList
	media    []proto.Media

	replyNet     string // the message this one answers
	replySender  int64
	replySnippet string

	forwarded   bool
	editCount   int
	service     string
	unsupported string
	preview     *proto.LinkPreview
	reactions   []reaction

	// plainText is a text message, the only kind Instagram lets you edit.
	plainText bool
	// skip is a message that isn't shown at all (Instagram's own "reacted
	// to your message" notes, which repeat a reaction).
	skip bool
	// counted is a message of the chat proper, which can be unread; a copy
	// that only came quoted inside a reply isn't.
	counted bool
	inLog   bool
	ids     []int64
}

type reaction struct {
	sender int64
	emoji  string
}

// convert turns an Instagram message into the backend's; b.mu is held. It
// registers the message's files, but downloads nothing and fetches no URL.
func (b *Instagram) convert(c *chat, m *slidetypes.Message) *netMsg {
	return b.convertDepth(c, m, 0)
}

func (b *Instagram) convertDepth(c *chat, m *slidetypes.Message, depth int) *netMsg {
	if m == nil {
		return nil
	}
	n := &netMsg{
		netID:     cmp.Or(m.ID, m.MessageID),
		otid:      m.OfflineThreadingID,
		ms:        millis(m.TimestampMS),
		sender:    m.SenderFBID,
		forwarded: m.IGDIsForwarded,
		editCount: len(m.SlideEditHistory),
	}
	if n.netID == "" {
		return nil
	}
	if n.ms == 0 {
		n.ms = time.Now().UnixMilli() // every message has a time; just in case
	}
	if m.Sender != nil {
		if p := b.person(&m.Sender.UserDict); p != nil && n.sender == 0 {
			n.sender = p.fbid
		}
	}
	n.mine = n.sender != 0 && n.sender == b.selfFBID
	for _, r := range m.Reactions {
		if r != nil && r.Reaction != "" {
			n.setReaction(r.SenderFBID, r.Reaction)
		}
	}
	// Media in a chat in vanish mode, or that expires once seen, is shown
	// only while it's open on the phone: tuimeta never gets a file of it.
	vanishing := !m.ExpirationTimestampMS.IsZero() || !m.ViewExpirationTimestampMS.IsZero()

	switch content := m.Content.Content.(type) {
	case *slidetypes.MessageContentText:
		n.text = cmp.Or(content.TextBody, m.TextBody)
		n.mentions = m.Mentions
		n.plainText = true
	case *slidetypes.MessageContentAdminText:
		if content.IsReactionActionLog {
			n.skip = true
		}
		n.service = adminSentence(content.TextFragments, m.IGDSnippet)
	case *slidetypes.MessageContentImage:
		for i, att := range content.Attachments {
			n.addMedia(b.attachment(c, n, att, proto.Photo, i, vanishing))
		}
	case *slidetypes.MessageContentVideo:
		for i, att := range content.Videos {
			n.addMedia(b.attachment(c, n, att, proto.Video, i, vanishing))
		}
	case *slidetypes.MessageContentMultiMedia:
		for i, att := range content.Attachments {
			n.addMedia(b.attachment(c, n, att, attachmentKind(att), i, vanishing))
		}
	case *slidetypes.MessageContentAudio:
		for i, att := range content.AudioAttachments {
			n.addMedia(b.audio(c, n, att, i, vanishing))
		}
	case *slidetypes.MessageContentAnimatedMedia:
		for i, att := range content.AnimatedMedia {
			n.addMedia(b.animated(c, n, att, i, vanishing))
		}
	case *slidetypes.MessageContentRavenImage:
		n.addMedia(b.raven(c, n, content.Attachment, content.ViewMode, proto.Photo, vanishing))
	case *slidetypes.MessageContentRavenVideo:
		n.addMedia(b.raven(c, n, content.Attachment, content.ViewMode, proto.Video, vanishing))
	case *slidetypes.MessageContentSticker:
		n.addMedia(b.sticker(c, n, content, vanishing))
	case *slidetypes.MessageContentMusicSticker:
		n.unsupported = "[Music]"
	case *slidetypes.MessageContentXMA:
		b.xma(c, n, content, m.Mentions)
	case *slidetypes.MessageContentAIRichResponse:
		n.text = content.UnifiedResponse
	case *slidetypes.MessageContentAISearchResponse:
		n.text = content.MessageTextBody
	default:
		n.text = m.TextBody
		n.unsupported = "[Unsupported message]"
	}
	if n.service == "" && n.text == "" && len(n.media) == 0 && n.unsupported == "" && n.preview == nil {
		n.unsupported = "[Unsupported message]"
	}

	if m.RepliedToMessageID != "" && n.service == "" {
		n.replyNet = m.RepliedToMessageID
		if rm := m.RepliedToMessage; rm != nil {
			n.replySender = rm.SenderFBID
			n.replySnippet = proto.Snippet(snippetOf(rm), 100)
			// Instagram quotes the answered message whole: kept, and given
			// ids, it answers get_message without loading its history.
			if depth == 0 && cmp.Or(rm.ID, rm.MessageID) == n.replyNet && millis(rm.TimestampMS) > 0 && c.msgs[n.replyNet] == nil {
				if q := b.convertDepth(c, rm, depth+1); q != nil && !q.skip {
					q.replyNet = "" // its own reply isn't quoted here
					c.msgs[q.netID] = q
					b.assign(c, q)
				}
			}
		}
	}
	return n
}

// millis is a timestamp in ms, 0 when Instagram gave none.
func millis(t jsontime.UnixMilliString) int64 {
	if t.IsZero() || t.UnixMilli() < 0 {
		return 0
	}
	return t.UnixMilli()
}

// addMedia adds an attachment, up to maxParts.
func (n *netMsg) addMedia(m proto.Media) {
	if len(n.media) < maxParts {
		n.media = append(n.media, m)
	}
}

// setReaction makes emoji sender's one reaction ("" takes it away).
func (n *netMsg) setReaction(sender int64, emoji string) {
	for i, r := range n.reactions {
		if r.sender == sender {
			if emoji == "" {
				n.reactions = append(n.reactions[:i], n.reactions[i+1:]...)
			} else {
				n.reactions[i].emoji = emoji
			}
			return
		}
	}
	if emoji != "" {
		n.reactions = append(n.reactions, reaction{sender, emoji})
	}
}

// reactionOf is sender's reaction, or "".
func (n *netMsg) reactionOf(sender int64) string {
	for _, r := range n.reactions {
		if r.sender == sender {
			return r.emoji
		}
	}
	return ""
}

// adminSentence is the words of an event in the chat ("Alice named the
// group Trip"), as Instagram writes them.
func adminSentence(fragments []slidetypes.TextFragment, snippet string) string {
	var b strings.Builder
	for _, f := range fragments {
		b.WriteString(f.Plaintext)
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	if s == "" {
		s = strings.Join(strings.Fields(snippet), " ")
	}
	if s == "" {
		s = "Something changed in this chat."
	}
	return s
}

// snippetOf is what a reply quotes of the message it answers: Instagram's
// own snippet when it has one, else the text.
func snippetOf(m *slidetypes.Message) string {
	if m.IGDSnippet != "" {
		return m.IGDSnippet
	}
	switch content := m.Content.Content.(type) {
	case *slidetypes.MessageContentText:
		return cmp.Or(content.TextBody, m.TextBody)
	case *slidetypes.MessageContentXMA:
		return content.XMATextBody
	}
	return m.TextBody
}

func attachmentKind(att *slidetypes.Attachment) proto.MediaKind {
	if att == nil {
		return proto.Photo
	}
	if att.DashManifest != "" || strings.Contains(strings.ToLower(att.Typename), "video") ||
		att.AttachmentType == 4 { // table.AttachmentTypeVideo
		return proto.Video
	}
	return proto.Photo
}

// fileKey names an attachment's content for the download cache: its
// attachment id when it has one, else the path of its URL. Never the query,
// which holds an expiring signature, nor the host, which is whichever CDN
// server answered.
func fileKey(attachmentID, rawURL, which string) string {
	if attachmentID != "" {
		return "att:" + attachmentID + ":" + which
	}
	return "url:" + urlPath(rawURL) + ":" + which
}

func urlPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

// mimeOf guesses a file's type from its URL's extension, else from what it is.
func mimeOf(raw string, kind proto.MediaKind) string {
	ext := strings.ToLower(path.Ext(urlPath(raw)))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4":
		if kind == proto.Voice || kind == proto.Audio {
			return "audio/mp4"
		}
		return "video/mp4"
	case ".m4a":
		return "audio/mp4"
	case ".aac":
		return "audio/aac"
	case ".ogg", ".opus":
		return "audio/ogg"
	}
	switch kind {
	case proto.Photo:
		return "image/jpeg"
	case proto.Video, proto.GIF:
		return "video/mp4"
	case proto.Voice, proto.Audio:
		return "audio/mp4"
	case proto.Sticker:
		return "image/webp"
	}
	return ""
}

// attachment is a photo or video. hidden ones (view once, vanish mode) get
// neither file nor thumbnail.
func (b *Instagram) attachment(c *chat, n *netMsg, att *slidetypes.Attachment, kind proto.MediaKind, index int, hidden bool) proto.Media {
	if hidden || att == nil {
		return proto.Media{Kind: kind, ViewOnce: true}
	}
	full := cmp.Or(att.AttachmentCDNURL, att.PreviewCDNURL)
	m := proto.Media{Kind: kind, Width: att.PreviewWidth, Height: att.PreviewHeight, Mime: mimeOf(full, kind)}
	if full != "" {
		m.FileID = b.register(fileKey(att.AttachmentFBID, full, "file"), full, m.Mime, c, n)
	}
	switch {
	case att.PreviewCDNURL != "" && att.PreviewCDNURL != full:
		thumb := b.register(fileKey(att.AttachmentFBID, att.PreviewCDNURL, "thumb"), att.PreviewCDNURL, "image/jpeg", c, n)
		m.Thumbnail = &proto.Image{FileID: thumb, Width: att.PreviewWidth, Height: att.PreviewHeight}
	case kind == proto.Photo && m.FileID != 0:
		m.Thumbnail = &proto.Image{FileID: m.FileID, Width: att.PreviewWidth, Height: att.PreviewHeight}
	}
	return m
}

func (b *Instagram) audio(c *chat, n *netMsg, att *slidetypes.AudioAttachment, index int, hidden bool) proto.Media {
	m := proto.Media{Kind: proto.Voice, Mime: "audio/mp4"}
	if att == nil {
		return m
	}
	m.Duration = (att.PlayableDurationMS + 999) / 1000
	if hidden {
		m.ViewOnce = true
		return m
	}
	if att.AttachmentCDNURL != "" {
		m.Mime = mimeOf(att.AttachmentCDNURL, proto.Voice)
		m.FileID = b.register(fileKey(att.AttachmentFBID, att.AttachmentCDNURL, "file"), att.AttachmentCDNURL, m.Mime, c, n)
	}
	return m
}

func (b *Instagram) animated(c *chat, n *netMsg, att *slidetypes.AnimatedAttachment, index int, hidden bool) proto.Media {
	if att == nil {
		return proto.Media{Kind: proto.GIF}
	}
	kind, link, mime := proto.Photo, att.PreviewCDNURL, "image/jpeg"
	switch {
	case att.AttachmentWebpURL != "" && (att.IsSticker || att.AttachmentMP4URL == ""):
		kind, link, mime = proto.Sticker, att.AttachmentWebpURL, "image/webp"
	case att.AttachmentMP4URL != "":
		kind, link, mime = proto.GIF, att.AttachmentMP4URL, "video/mp4"
	}
	m := proto.Media{Kind: kind, Width: att.PreviewWidth, Height: att.PreviewHeight, Mime: mime}
	if hidden {
		m.ViewOnce = true
		return m
	}
	if link != "" {
		m.FileID = b.register(fileKey("", link, "file"), link, mime, c, n)
	}
	switch {
	case att.PreviewCDNURL != "" && att.PreviewCDNURL != link:
		thumb := b.register(fileKey("", att.PreviewCDNURL, "thumb"), att.PreviewCDNURL, "image/jpeg", c, n)
		m.Thumbnail = &proto.Image{FileID: thumb, Width: att.PreviewWidth, Height: att.PreviewHeight}
	case kind != proto.GIF && m.FileID != 0:
		m.Thumbnail = &proto.Image{FileID: m.FileID, Width: att.PreviewWidth, Height: att.PreviewHeight}
	}
	return m
}

func (b *Instagram) sticker(c *chat, n *netMsg, s *slidetypes.MessageContentSticker, hidden bool) proto.Media {
	m := proto.Media{Kind: proto.Sticker, Width: s.PreviewWidth, Height: s.PreviewHeight, Mime: mimeOf(s.PreviewURL, proto.Sticker)}
	if hidden {
		m.ViewOnce = true
		return m
	}
	if s.PreviewURL != "" {
		m.FileID = b.register(fileKey("", s.PreviewURL, "file"), s.PreviewURL, m.Mime, c, n)
		m.Thumbnail = &proto.Image{FileID: m.FileID, Width: s.PreviewWidth, Height: s.PreviewHeight}
	}
	return m
}

// raven is a disappearing photo or video. Seen once, or replayed once, it's
// never offered as a file; only those the sender let you keep in the chat
// are ordinary media.
func (b *Instagram) raven(c *chat, n *netMsg, att *slidetypes.Attachment, mode slidetypes.RavenViewMode, kind proto.MediaKind, vanishing bool) proto.Media {
	hidden := vanishing || att == nil || mode != slidetypes.RavenViewModeKeepInChat
	return b.attachment(c, n, att, kind, 0, hidden)
}

// xma is a shared post, reel, story or link: a card from Instagram's own
// data when it names a web address (which the text then shows, as the card
// is of it), else a label saying what was shared.
func (b *Instagram) xma(c *chat, n *netMsg, content *slidetypes.MessageContentXMA, mentions slidetypes.MentionList) {
	n.text = content.XMATextBody
	n.mentions = mentions
	x := content.XMA
	if x == nil {
		if n.text == "" {
			n.unsupported = "[Unsupported message]"
		}
		return
	}
	link := webURL(unwrapRedirect(x.TargetURL))
	if link == "" {
		n.unsupported = xmaLabel(x.TargetURL)
		return
	}
	title := cmp.Or(x.TitleText, x.HeaderTitleText, x.EyebrowText)
	n.preview = &proto.LinkPreview{URL: link, Title: title, Description: cmp.Or(x.SubtitleText, x.CaptionBodyText)}
	if img := cmp.Or(x.PreviewImage, x.XMAPreviewImage); img != nil && webURL(img.URL) != "" {
		id := b.register(fileKey("", img.URL, "preview"), img.URL, mimeOf(img.URL, proto.Photo), c, n)
		n.preview.Image = &proto.Image{FileID: id, Width: img.Width, Height: img.Height}
	}
	// tuimeta shows a card only for a link the text holds, so a share
	// without words names its address.
	if !strings.Contains(n.text, link) {
		if n.text != "" {
			n.text += "\n"
		}
		n.text += link
	}
}

// xmaLabel names what was shared, from the kind of page it would open.
func xmaLabel(target string) string {
	p := ""
	if u, err := url.Parse(target); err == nil {
		p = u.Path
	}
	switch {
	case strings.HasPrefix(p, "/p/"):
		return "[Shared post]"
	case strings.HasPrefix(p, "/reel/"), strings.HasPrefix(p, "/reels/"):
		return "[Shared reel]"
	case strings.HasPrefix(p, "/stories/"):
		return "[Shared story]"
	case strings.HasPrefix(p, "/tv/"):
		return "[Shared video]"
	}
	return "[Shared post]"
}

// webURL is raw if it's an http(s) address with a host, else "".
func webURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return u.String()
}

// unwrapRedirect takes the real address out of Meta's link shims
// (l.facebook.com/l.php?u=…), as the connector does; nothing is fetched.
func unwrapRedirect(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	switch strings.ToLower(u.Hostname()) {
	case "l.facebook.com", "l.instagram.com", "lm.facebook.com":
		if inner := u.Query().Get("u"); inner != "" {
			return inner
		}
	}
	return raw
}

// assign gives a message its ids for the run (the same ones every time it's
// seen); b.mu is held.
func (b *Instagram) assign(c *chat, n *netMsg) {
	if n.ids == nil {
		n.ids = b.d.Messages.Assign(c.id, n.netID, max(n.ms, 1), max(len(n.media), 1))
	}
}

// parts is the protocol's messages for n; b.mu is held.
func (b *Instagram) parts(c *chat, n *netMsg) []proto.Message {
	b.assign(c, n)
	base := proto.Message{
		ChatID:      c.id,
		SenderID:    b.userID(n.sender),
		Outgoing:    n.mine,
		Date:        n.ms / 1000,
		Forwarded:   n.forwarded,
		Edited:      n.editCount > 0,
		Deletable:   n.mine && n.service == "",
		State:       proto.Sent,
		Service:     n.service,
		Unsupported: n.unsupported,
	}
	if n.service == "" {
		base.Text, base.Entities = b.format(n.text, n.mentions)
		if n.preview != nil {
			p := *n.preview
			base.LinkPreview = &p
		}
	}
	if n.replyNet != "" {
		r := &proto.ReplyTo{SenderID: b.userID(n.replySender), Text: n.replySnippet}
		if ids, ok := b.d.Messages.Known(c.id, n.replyNet); ok && len(ids) > 0 {
			r.MessageID = ids[0]
		}
		base.ReplyTo = r
	}
	base.Reactions = b.reactionsOf(n)
	if n.mine && n.plainText && n.editCount < maxEdits && n.service == "" {
		base.EditableUntil = n.ms/1000 + editWindow
	}
	media := fitMedia(n.media, len(n.ids))
	return proto.SplitAlbum(base, media, n.ids)
}

// fitMedia makes the media match the number of ids a message was given
// (one per part, or one without media). They only differ if Instagram
// changed a message's attachments after it was first seen, which shouldn't
// happen; the album then keeps its shape rather than its ids changing.
func fitMedia(media []proto.Media, ids int) []proto.Media {
	switch {
	case len(media) == ids || (len(media) == 0 && ids == 1):
		return media
	case len(media) > ids:
		hlog.Warn("instagram: a message has more attachments than when first seen")
		return media[:ids]
	}
	hlog.Warn("instagram: a message has fewer attachments than when first seen")
	out := append([]proto.Media(nil), media...)
	for len(out) < ids {
		if len(out) == 0 {
			out = append(out, proto.Media{Kind: proto.Photo, ViewOnce: true})
		} else {
			out = append(out, out[len(out)-1])
		}
	}
	return out
}

// reactionsOf counts n's reactions by emoji, in the order they first came.
func (b *Instagram) reactionsOf(n *netMsg) []proto.Reaction {
	var out []proto.Reaction
	for _, r := range n.reactions {
		i := -1
		for j := range out {
			if out[j].Emoji == r.emoji {
				i = j
				break
			}
		}
		if i < 0 {
			out = append(out, proto.Reaction{Emoji: r.emoji})
			i = len(out) - 1
		}
		out[i].Count++
		if r.sender == b.selfFBID {
			out[i].Mine = true
		}
	}
	return out
}

// format is a message's text without Instagram's formatting markers
// (*bold*, _italic_, ~strike~, `code`, ```blocks```, quotes), and its
// entities: that formatting, and mentions from Instagram's own ranges, which
// are of the text with its markers (UTF-16 units). A mention of the whole
// chat, or of no one, keeps its words without an entity.
func (b *Instagram) format(text string, mentions slidetypes.MentionList) (string, []proto.Entity) {
	units := utf16.Encode([]rune(text))
	// A range must fall on whole characters: one splitting a character in
	// two (a sender's crafting, or a stale range) is left out.
	whole := func(at int) bool {
		if at < 0 || at > len(units) {
			return false
		}
		if at == 0 || at == len(units) {
			return true
		}
		high, low := units[at-1], units[at]
		return !(high >= 0xD800 && high < 0xDC00 && low >= 0xDC00 && low < 0xE000)
	}
	var ms []metatext.Mention
	for _, m := range mentions {
		if m == nil || m.Length <= 0 || !whole(m.Offset) || !whole(m.Offset+m.Length) {
			continue
		}
		var user int64
		if m.UserFBID != 0 && (m.ProfileRangeType == slidetypes.ProfileRangeTypeProfile || m.ProfileRangeType == "") {
			user = b.userID(m.UserFBID)
		}
		ms = append(ms, metatext.Mention{Offset: m.Offset, Length: m.Length, UserID: user})
	}
	return metatext.ParseWithMentions(text, ms)
}
