// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/erictran308/tuimeta/helper/internal/metatext"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// changeKind is what a WhatsApp message does to its chat.
type changeKind int

const (
	ignored changeKind = iota
	added              // a new message (or an event): change.msg
	edited             // the content of change.target is now change.msg's
	revoked            // change.target was deleted for everyone
	reacted            // the sender's reaction to change.target is now change.emoji
)

// change is what reading a WhatsApp message gave.
type change struct {
	kind   changeKind
	msg    *message
	target string
	emoji  string
	// timer is set when the message turned disappearing messages on or off.
	timer *uint32
	// owner is whose message a deletion says it deletes (a person key), when
	// it names someone: a group's admin may delete anyone's.
	owner string
}

// reader reads WhatsApp's messages. canon gives a person's key (their
// WhatsApp id when it's known, else their number's JID).
type reader struct {
	canon func(waTypes.JID) string
}

// read reads one message whose sender's key is sender.
func (r reader) read(evt *events.Message, sender string) change {
	info, msg := evt.Info, evt.Message
	if msg == nil {
		return change{}
	}
	if pm := msg.GetProtocolMessage(); pm != nil {
		return r.protocol(pm, info, sender)
	}
	if rm := msg.GetReactionMessage(); rm != nil {
		if !reactionLike(rm.GetText()) {
			return change{}
		}
		return change{kind: reacted, target: rm.GetKey().GetID(), emoji: rm.GetText()}
	}
	switch {
	case msg.GetPollUpdateMessage() != nil, msg.GetEncEventResponseMessage() != nil, msg.GetEncCommentMessage() != nil,
		msg.GetKeepInChatMessage() != nil, msg.GetPinInChatMessage() != nil, msg.GetAlbumMessage() != nil,
		msg.GetStickerSyncRmrMessage() != nil, msg.GetMessageHistoryBundle() != nil, msg.GetMessageHistoryNotice() != nil,
		msg.GetPlaceholderMessage() != nil, msg.GetEncReactionMessage() != nil:
		// Votes, pins, album headers and bookkeeping: nothing to show. An
		// encrypted reaction is decrypted before it's read (receive); one
		// still encrypted (in the phone's history) can't be.
		return change{}
	}
	switch msg.GetMessageContextInfo().GetMessageAssociation().GetAssociationType() {
	case waE2E.MessageAssociation_HD_IMAGE_DUAL_UPLOAD, waE2E.MessageAssociation_HD_VIDEO_DUAL_UPLOAD, waE2E.MessageAssociation_MOTION_PHOTO:
		// A better copy of a photo or video already shown: the one shown stays.
		return change{}
	}
	out := &message{ID: info.ID, Sender: sender, MS: info.Timestamp.UnixMilli()}
	r.content(msg, out, evt.IsViewOnce || evt.IsViewOnceV2 || evt.IsViewOnceV2Extension)
	if out.Text == "" && out.Media == nil && out.Unsupported == "" && out.Preview == nil && out.Service == nil {
		if msg.GetSenderKeyDistributionMessage() != nil {
			return change{} // only a group's keys, which whatsmeow has taken
		}
		out.Unsupported = "[Unsupported message]"
	}
	return change{kind: added, msg: out}
}

// protocolIn is the protocol message (an edit, a deletion, a timer change)
// a history entry holds, unwrapped as whatsmeow unwraps messages; nil if
// it holds none.
func protocolIn(web *waWeb.WebMessageInfo) *waE2E.ProtocolMessage {
	e := &events.Message{RawMessage: web.GetMessage()}
	return e.UnwrapRaw().Message.GetProtocolMessage()
}

// protocol reads an edit, a deletion or a timer change.
func (r reader) protocol(pm *waE2E.ProtocolMessage, info waTypes.MessageInfo, sender string) change {
	switch pm.GetType() {
	case waE2E.ProtocolMessage_REVOKE:
		if pm.GetKey().GetID() == "" {
			return change{}
		}
		ch := change{kind: revoked, target: pm.GetKey().GetID()}
		if p, err := waTypes.ParseJID(pm.GetKey().GetParticipant()); err == nil && personJID(p) {
			ch.owner = r.canon(p)
		}
		return ch
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		out := &message{ID: pm.GetKey().GetID(), Sender: sender, MS: pm.GetTimestampMS()}
		if out.MS == 0 {
			out.MS = info.Timestamp.UnixMilli()
		}
		if pm.GetEditedMessage() == nil || out.ID == "" {
			return change{}
		}
		r.content(pm.GetEditedMessage(), out, false)
		return change{kind: edited, msg: out, target: out.ID}
	case waE2E.ProtocolMessage_EPHEMERAL_SETTING:
		// In a group the timer is the group's, and WhatsApp's own notice of
		// it (GroupInfo) says who changed it, after checking they may: a
		// member's message saying so is taken as nothing. In a chat with one
		// person either may set it, to one of WhatsApp's timers.
		secs := pm.GetEphemeralExpiration()
		if info.IsGroup || info.Chat.Server == waTypes.GroupServer || !timerAllowed(secs) {
			return change{}
		}
		out := &message{ID: info.ID, Sender: sender, MS: info.Timestamp.UnixMilli(), Service: timerService(sender, secs)}
		return change{kind: added, msg: out, timer: &secs}
	}
	return change{}
}

// timerAllowed reports whether secs is one of WhatsApp's disappearing-
// messages timers: off, 24 hours, 7 days or 90 days.
func timerAllowed(secs uint32) bool {
	switch secs {
	case 0, 86400, 7 * 86400, 90 * 86400:
		return true
	}
	return false
}

// Limits on what a sender can make the helper keep and show.
const (
	// MaxText is the longest text kept of a message, in bytes: WhatsApp's
	// own limit is 65,536 characters.
	MaxText = 64 << 10
	// MaxThumb is the biggest picture kept from inside a message (a
	// thumbnail or a link's picture); a bigger one isn't kept.
	MaxThumb = 64 << 10
	// MaxMentions is how many people one message may mention.
	MaxMentions = 256
	// maxReaction is the longest reaction, in bytes: one emoji, however
	// it's composed.
	maxReaction = 32
	// MaxField is the longest name, title or description kept of a message
	// (a file's name, a link card's title, a group's name in an event), in
	// bytes.
	MaxField = 1 << 10
	// maxMime is the longest file type kept, in bytes.
	maxMime = 255
	// maxLink is the longest link a card is kept for, in bytes.
	maxLink = 4 << 10
)

// reactionLike reports whether s can be a reaction: "" (taken back), or a
// short emoji, not words, numbers or spaces made to look like part of the
// message (a count, a time, ticks). Digits and punctuation pass only as
// emoji: with the keycap or the emoji-style selector ("1️⃣", "‼️").
func reactionLike(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > maxReaction || !utf8.ValidString(s) {
		return false
	}
	emojiStyle := strings.ContainsAny(s, "\u20e3\ufe0f")
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsSpace(r), unicode.IsControl(r):
			return false
		case (unicode.IsNumber(r) || unicode.IsPunct(r)) && !emojiStyle:
			return false
		}
	}
	return true
}

// cutText keeps at most n bytes of s, ending on a whole character.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// thumb is a picture from inside a message, if it isn't too big to keep.
func thumb(b []byte) []byte {
	if len(b) > MaxThumb {
		return nil
	}
	return b
}

// timerService is the event of a disappearing-messages timer set to secs.
func timerService(actor string, secs uint32) *service {
	return &service{Kind: "timer", Actor: actor, Text: strconv.FormatUint(uint64(secs), 10)}
}

// content fills in what a message says and carries.
func (r reader) content(msg *waE2E.Message, out *message, viewOnce bool) {
	var ci *waE2E.ContextInfo
	switch {
	case msg.GetConversation() != "":
		out.Text = msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		x := msg.GetExtendedTextMessage()
		out.Text, ci = x.GetText(), x.GetContextInfo()
		out.Preview = linkPreview(x)
	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		ci = m.GetContextInfo()
		kind := proto.Photo
		if m.GetMimetype() == "image/gif" {
			kind = proto.GIF
		}
		out.Media = &media{
			Kind: kind, Mime: m.GetMimetype(), Size: int64(m.GetFileLength()), Width: int(m.GetWidth()), Height: int(m.GetHeight()),
			DirectPath: m.GetDirectPath(), MediaKey: m.GetMediaKey(), FileSHA256: m.GetFileSHA256(), FileEncSHA256: m.GetFileEncSHA256(),
			MediaType: string(whatsmeow.MediaImage), Thumb: m.GetJPEGThumbnail(),
		}
		out.Text = m.GetCaption()
		viewOnce = viewOnce || m.GetViewOnce()
	case msg.GetVideoMessage() != nil:
		m := msg.GetVideoMessage()
		ci = m.GetContextInfo()
		kind := proto.Video
		if m.GetGifPlayback() {
			kind = proto.GIF
		}
		out.Media = &media{
			Kind: kind, Mime: m.GetMimetype(), Size: int64(m.GetFileLength()), Width: int(m.GetWidth()), Height: int(m.GetHeight()), Seconds: int(m.GetSeconds()),
			DirectPath: m.GetDirectPath(), MediaKey: m.GetMediaKey(), FileSHA256: m.GetFileSHA256(), FileEncSHA256: m.GetFileEncSHA256(),
			MediaType: string(whatsmeow.MediaVideo), Thumb: m.GetJPEGThumbnail(),
		}
		out.Text = m.GetCaption()
		viewOnce = viewOnce || m.GetViewOnce()
	case msg.GetPtvMessage() != nil:
		// A round video message.
		m := msg.GetPtvMessage()
		ci = m.GetContextInfo()
		out.Media = &media{
			Kind: proto.Video, Mime: m.GetMimetype(), Size: int64(m.GetFileLength()), Width: int(m.GetWidth()), Height: int(m.GetHeight()), Seconds: int(m.GetSeconds()),
			DirectPath: m.GetDirectPath(), MediaKey: m.GetMediaKey(), FileSHA256: m.GetFileSHA256(), FileEncSHA256: m.GetFileEncSHA256(),
			MediaType: string(whatsmeow.MediaVideo), Thumb: m.GetJPEGThumbnail(),
		}
		viewOnce = viewOnce || m.GetViewOnce()
	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		ci = m.GetContextInfo()
		kind := proto.Audio
		if m.GetPTT() {
			kind = proto.Voice
		}
		out.Media = &media{
			Kind: kind, Mime: m.GetMimetype(), Size: int64(m.GetFileLength()), Seconds: int(m.GetSeconds()),
			DirectPath: m.GetDirectPath(), MediaKey: m.GetMediaKey(), FileSHA256: m.GetFileSHA256(), FileEncSHA256: m.GetFileEncSHA256(),
			MediaType: string(whatsmeow.MediaAudio),
		}
		viewOnce = viewOnce || m.GetViewOnce()
	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		ci = m.GetContextInfo()
		name := m.GetFileName()
		if name == "" {
			name = m.GetTitle()
		}
		out.Media = &media{
			Kind: proto.FileMedia, Mime: m.GetMimetype(), Name: name, Size: int64(m.GetFileLength()),
			DirectPath: m.GetDirectPath(), MediaKey: m.GetMediaKey(), FileSHA256: m.GetFileSHA256(), FileEncSHA256: m.GetFileEncSHA256(),
			MediaType: string(whatsmeow.MediaDocument), Thumb: m.GetJPEGThumbnail(),
			ThumbWidth: int(m.GetThumbnailWidth()), ThumbHeight: int(m.GetThumbnailHeight()),
		}
		out.Text = m.GetCaption()
	case msg.GetStickerMessage() != nil:
		m := msg.GetStickerMessage()
		ci = m.GetContextInfo()
		out.Media = &media{
			Kind: proto.Sticker, Mime: m.GetMimetype(), Size: int64(m.GetFileLength()), Width: int(m.GetWidth()), Height: int(m.GetHeight()),
			DirectPath: m.GetDirectPath(), MediaKey: m.GetMediaKey(), FileSHA256: m.GetFileSHA256(), FileEncSHA256: m.GetFileEncSHA256(),
			MediaType: string(whatsmeow.MediaImage),
		}
	case msg.GetLocationMessage() != nil:
		ci = msg.GetLocationMessage().GetContextInfo()
		out.Unsupported = "[Location]"
	case msg.GetLiveLocationMessage() != nil:
		ci = msg.GetLiveLocationMessage().GetContextInfo()
		out.Unsupported = "[Live location]"
	case msg.GetContactMessage() != nil:
		ci = msg.GetContactMessage().GetContextInfo()
		out.Unsupported = "[Contact]"
	case msg.GetContactsArrayMessage() != nil:
		ci = msg.GetContactsArrayMessage().GetContextInfo()
		out.Unsupported = "[Contacts]"
	case msg.GetPollCreationMessage() != nil, msg.GetPollCreationMessageV2() != nil, msg.GetPollCreationMessageV3() != nil,
		msg.GetPollCreationMessageV4() != nil, msg.GetPollCreationMessageV5() != nil, msg.GetPollCreationMessageV6() != nil:
		out.Unsupported = "[Poll]"
	case msg.GetGroupInviteMessage() != nil:
		ci = msg.GetGroupInviteMessage().GetContextInfo()
		out.Unsupported = "[Group invite]"
	case msg.GetEventMessage() != nil:
		out.Unsupported = "[Event]"
	case msg.GetCall() != nil, msg.GetScheduledCallCreationMessage() != nil:
		out.Unsupported = "[Call]"
	case msg.GetStickerPackMessage() != nil:
		out.Unsupported = "[Sticker pack]"
	case msg.GetRequestPhoneNumberMessage() != nil:
		out.Unsupported = "[Asked for your phone number]"
	case msg.GetSendPaymentMessage() != nil, msg.GetRequestPaymentMessage() != nil, msg.GetPaymentInviteMessage() != nil:
		out.Unsupported = "[Payment]"
	case msg.GetButtonsMessage() != nil:
		out.Text = msg.GetButtonsMessage().GetContentText()
		ci = msg.GetButtonsMessage().GetContextInfo()
	case msg.GetTemplateMessage() != nil:
		out.Text = msg.GetTemplateMessage().GetHydratedTemplate().GetHydratedContentText()
		ci = msg.GetTemplateMessage().GetContextInfo()
	case msg.GetInteractiveMessage() != nil:
		out.Text = msg.GetInteractiveMessage().GetBody().GetText()
		ci = msg.GetInteractiveMessage().GetContextInfo()
	case msg.GetListMessage() != nil:
		out.Text = msg.GetListMessage().GetDescription()
		ci = msg.GetListMessage().GetContextInfo()
	case msg.GetButtonsResponseMessage() != nil:
		out.Text = msg.GetButtonsResponseMessage().GetSelectedDisplayText()
		ci = msg.GetButtonsResponseMessage().GetContextInfo()
	case msg.GetListResponseMessage() != nil:
		out.Text = msg.GetListResponseMessage().GetTitle()
		ci = msg.GetListResponseMessage().GetContextInfo()
	case msg.GetTemplateButtonReplyMessage() != nil:
		out.Text = msg.GetTemplateButtonReplyMessage().GetSelectedDisplayText()
		ci = msg.GetTemplateButtonReplyMessage().GetContextInfo()
	}
	out.Text = cutText(out.Text, MaxText)
	if out.Media != nil {
		out.Media.Thumb = thumb(out.Media.Thumb)
		out.Media.Name = cutText(out.Media.Name, MaxField)
		out.Media.Mime = cutText(out.Media.Mime, maxMime)
	}
	if out.Preview != nil {
		out.Preview.Thumb = thumb(out.Preview.Thumb)
	}
	if viewOnce && out.Media != nil {
		// Seen only once, on the phone: nothing of it is kept that could
		// fetch it, and no preview.
		out.Media = &media{Kind: viewOnceKind(out.Media.Kind), ViewOnce: true}
		out.Text = ""
	}
	r.context(ci, out)
}

// viewOnceKind is how a view-once attachment is named: a photo, a video or a
// voice message.
func viewOnceKind(k proto.MediaKind) proto.MediaKind {
	switch k {
	case proto.Video, proto.GIF:
		return proto.Video
	case proto.Audio, proto.Voice:
		return proto.Voice
	}
	return proto.Photo
}

// linkPreview is the card the sender's app attached to a link, if any. Only
// http(s) links get one.
func linkPreview(x *waE2E.ExtendedTextMessage) *preview {
	link := webLink(x.GetMatchedText())
	if link == "" || len(link) > maxLink || (x.GetTitle() == "" && x.GetDescription() == "") {
		return nil
	}
	return &preview{
		URL: link, Title: oneLine(cutText(x.GetTitle(), MaxField)), Description: oneLine(cutText(x.GetDescription(), MaxField)),
		Thumb: x.GetJPEGThumbnail(),
	}
}

// context reads what a message's context says: the reply, forwarding,
// mentions and when it disappears.
func (r reader) context(ci *waE2E.ContextInfo, out *message) {
	if ci == nil {
		return
	}
	if id := ci.GetStanzaID(); id != "" {
		rp := &reply{ID: id}
		if p, err := waTypes.ParseJID(ci.GetParticipant()); err == nil && personJID(p) {
			rp.Sender = r.canon(p)
		}
		rp.Text = quoteOf(ci.GetQuotedMessage())
		out.Reply = rp
	}
	out.Forwarded = ci.GetIsForwarded()
	out.Mentions = out.Mentions[:0]
	for _, m := range ci.GetMentionedJID() {
		if len(out.Mentions) == MaxMentions {
			break
		}
		if !slices.Contains(out.Mentions, m) {
			out.Mentions = append(out.Mentions, m)
		}
	}
	if secs := ci.GetExpiration(); secs > 0 {
		out.Expires = out.MS + int64(secs)*1000
	}
}

// quoteOf is a one-line snippet of a quoted message. A photo or video seen
// only once is quoted without its caption, as its own message keeps none.
func quoteOf(q *waE2E.Message) string {
	viewOnce := false
	for _, wrapped := range []*waE2E.FutureProofMessage{q.GetViewOnceMessage(), q.GetViewOnceMessageV2(), q.GetViewOnceMessageV2Extension()} {
		if wrapped.GetMessage() != nil {
			q, viewOnce = wrapped.GetMessage(), true
			break
		}
	}
	caption := func(text string, once bool) string {
		if viewOnce || once {
			return ""
		}
		return snippet(text)
	}
	switch {
	case q == nil:
		return ""
	case q.GetConversation() != "":
		return snippet(q.GetConversation())
	case q.GetExtendedTextMessage().GetText() != "":
		return snippet(q.GetExtendedTextMessage().GetText())
	case q.GetImageMessage() != nil:
		return firstNonEmpty(caption(q.GetImageMessage().GetCaption(), q.GetImageMessage().GetViewOnce()), "Photo")
	case q.GetVideoMessage() != nil:
		return firstNonEmpty(caption(q.GetVideoMessage().GetCaption(), q.GetVideoMessage().GetViewOnce()), "Video")
	case q.GetAudioMessage() != nil:
		if q.GetAudioMessage().GetPTT() {
			return "Voice message"
		}
		return "Audio"
	case q.GetDocumentMessage() != nil:
		return firstNonEmpty(proto.Snippet(cutText(q.GetDocumentMessage().GetFileName(), MaxField), 100), "File")
	case q.GetStickerMessage() != nil:
		return "Sticker"
	}
	return ""
}

// stub reads one of the events WhatsApp keeps in a chat's history (a group
// renamed, people added, a missed call) as a service message; nil for ones
// that aren't shown.
func (r reader) stub(web *waWeb.WebMessageInfo, id, actor string, ms int64) *message {
	params := web.GetMessageStubParameters()
	people := func() []string {
		var out []string
		for _, p := range params {
			if j, err := waTypes.ParseJID(p); err == nil && personJID(j) {
				out = append(out, r.canon(j))
			}
		}
		return out
	}
	first := ""
	if len(params) > 0 {
		first = cutText(params[0], MaxField)
	}
	var s *service
	switch web.GetMessageStubType() {
	case waWeb.WebMessageInfo_GROUP_CREATE:
		s = &service{Kind: "create", Actor: actor, Text: oneLine(first)}
	case waWeb.WebMessageInfo_GROUP_CHANGE_SUBJECT:
		s = &service{Kind: "rename", Actor: actor, Text: oneLine(first)}
	case waWeb.WebMessageInfo_GROUP_CHANGE_ICON:
		s = &service{Kind: "photo", Actor: actor}
	case waWeb.WebMessageInfo_GROUP_CHANGE_DESCRIPTION:
		s = &service{Kind: "description", Actor: actor}
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD, waWeb.WebMessageInfo_GROUP_PARTICIPANT_INVITE:
		s = &service{Kind: "add", Actor: actor, Targets: people()}
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD_REQUEST_JOIN:
		s = &service{Kind: "join", Targets: people()}
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_REMOVE:
		s = &service{Kind: "remove", Actor: actor, Targets: people()}
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_LEAVE:
		s = &service{Kind: "leave", Targets: people()}
	case waWeb.WebMessageInfo_CALL_MISSED_VOICE, waWeb.WebMessageInfo_CALL_MISSED_GROUP_VOICE:
		s = &service{Kind: "missed_voice", Actor: actor}
	case waWeb.WebMessageInfo_CALL_MISSED_VIDEO, waWeb.WebMessageInfo_CALL_MISSED_GROUP_VIDEO:
		s = &service{Kind: "missed_video", Actor: actor}
	case waWeb.WebMessageInfo_CHANGE_EPHEMERAL_SETTING:
		// Only one of WhatsApp's timers: what isn't would read as a timer
		// it isn't (or, unreadable, as turned off).
		secs, err := strconv.ParseUint(first, 10, 32)
		if err != nil || !timerAllowed(uint32(secs)) {
			return nil
		}
		s = timerService(actor, uint32(secs))
	default:
		return nil
	}
	if (s.Kind == "add" || s.Kind == "remove" || s.Kind == "leave" || s.Kind == "join") && len(s.Targets) == 0 {
		return nil
	}
	return &message{ID: id, Sender: actor, MS: ms, Service: s}
}

// snippet is a one-line quote of text, its markers read. Only its start is
// read: a quote shows a hundred characters.
func snippet(text string) string {
	plain, _ := metatext.Parse(cutText(text, 1024))
	return proto.Snippet(plain, 100)
}

// webLink is raw if it's an http(s) address, else "".
func webLink(raw string) string {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") {
		return raw
	}
	if raw != "" && !strings.Contains(raw, "://") && !strings.ContainsAny(raw, " \t\n") && strings.Contains(raw, ".") {
		// WhatsApp's apps write addresses without a scheme in matchedText.
		return "https://" + raw
	}
	return ""
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
