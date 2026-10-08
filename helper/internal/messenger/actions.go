// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/util/variationselector"
	"go.mau.fi/whatsmeow"
	armadillo "go.mau.fi/whatsmeow/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/methods"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// SendTries is how often a send is tried while the socket isn't ready.
const SendTries = 3

// guardTasks refuses tasks that would tell people something about you
// unless the request is exactly that: read receipts only from MarkRead,
// typing only from SetTyping, and never the app state ("active") report.
func guardTasks(purpose string, tasks []socket.Task) error {
	for _, t := range tasks {
		switch t.(type) {
		case *socket.ThreadMarkReadTask:
			if purpose != "mark_read" {
				return proto.Err(proto.Internal, "A read receipt was about to go out unasked; it was stopped.")
			}
		case *socket.UpdatePresenceTask:
			if purpose != "typing" {
				return proto.Err(proto.Internal, "A typing notice was about to go out unasked; it was stopped.")
			}
		case *socket.ReportAppStateTask:
			return proto.Err(proto.Internal, "An activity report was about to go out; it was stopped.")
		}
	}
	return nil
}

// run sends tasks to Messenger and applies its answer.
func (m *Messenger) run(ctx context.Context, purpose string, tasks ...socket.Task) (*table.LSTable, error) {
	if err := guardTasks(purpose, tasks); err != nil {
		return nil, err
	}
	meta, err := m.connected()
	if err != nil {
		return nil, err
	}
	tbl, err := meta.ExecuteTasks(ctx, tasks...)
	if err != nil {
		hlog.Info("messenger: request failed", hlog.Str("request", purpose), hlog.Kind(err))
		return nil, requestError(err)
	}
	m.mu.Lock()
	m.applyTable(tbl, fromResponse)
	m.mu.Unlock()
	return tbl, nil
}

// sendsEncrypted reports whether c's messages go through the encrypted
// chats' socket; m.mu is held.
func (m *Messenger) sendsEncrypted(c *chat) bool {
	return c.encrypted && (c.ttype.IsWhatsApp() || c.server != "" || c.kind == proto.DM)
}

func (m *Messenger) Send(ctx context.Context, out *backend.Outgoing) error {
	m.mu.Lock()
	c, err := m.chatOf(out.Chat.ID)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if c.encrypted && !m.sendsEncrypted(c) {
		m.mu.Unlock()
		return proto.Err(proto.NetworkError, "This encrypted group isn't ready yet; try again in a moment.")
	}
	encrypted := m.sendsEncrypted(c)
	var replied *message
	if out.ReplyTo != nil {
		replied = c.msgs[baseNetID(out.ReplyTo.NetID)]
	}
	m.mu.Unlock()
	if encrypted {
		return m.sendEncrypted(ctx, c, out, replied)
	}
	return m.sendFB(ctx, c, out, replied)
}

// sendFB sends a message in an unencrypted chat. The confirmation comes in
// the answer or on the socket, whichever is first (the offline threading
// id finds the message either way).
func (m *Messenger) sendFB(ctx context.Context, c *chat, out *backend.Outgoing, replied *message) error {
	meta, err := m.connected()
	if err != nil {
		return err
	}
	otid := methods.GenerateEpochID()
	key := strconv.FormatInt(otid, 10)
	out.SetKey(key)
	task := &socket.SendMessageTask{
		ThreadId: c.threadKey(), Otid: otid, Source: table.MESSENGER_INBOX_IN_THREAD,
		InitiatingSource: table.FACEBOOK_INBOX, SendType: table.TEXT, SyncGroup: 1, Text: out.Text,
	}
	m.mu.Lock()
	if c.request {
		// Answering a message request from the requests folder accepts it,
		// which is what sending in it means.
		task.Source = table.MESSENGER_INBOX_PENDING_REQUESTS
	}
	m.mu.Unlock()
	if replied != nil && replied.wa == nil {
		task.ReplyMetaData = &socket.ReplyMetaData{ReplyMessageId: replied.netID, ReplySourceType: 1, ReplySender: replied.sender}
	}
	var sent []proto.Media
	for _, f := range out.Files {
		fbid, err := meta.Upload(ctx, c.threadKey(), &httpclient.MercuryUploadMedia{
			Filename: f.Name, MimeType: f.Mime, MediaData: f.Data, IsVoiceClip: f.Kind == proto.Voice,
		})
		if err != nil {
			hlog.Info("messenger: upload failed", hlog.Kind(err))
			return proto.Err(proto.NetworkError, "A file couldn't be uploaded to Messenger; try again.")
		}
		task.SendType = table.MEDIA
		task.AttachmentFBIds = append(task.AttachmentFBIds, fbid)
		sent = append(sent, m.localMedia(f))
	}
	if err := guardTasks("send", []socket.Task{task}); err != nil {
		return err
	}
	var tbl *table.LSTable
	for try := 0; try < SendTries; try++ {
		if err = meta.WaitUntilCanSend(ctx, 15*time.Second); err == nil {
			if tbl, err = meta.ExecuteTasks(ctx, task); err == nil {
				break
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err != nil {
		hlog.Info("messenger: send failed", hlog.Kind(err))
		return requestError(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyTable(tbl, fromResponse)
	for _, f := range tbl.LSMarkOptimisticMessageFailed {
		if f.OTID == key {
			return proto.Err(proto.NetworkError, "Messenger refused the message.")
		}
	}
	for _, f := range tbl.LSHandleFailedTask {
		if f.OTID == key {
			return proto.Err(proto.NetworkError, "Messenger refused the message.")
		}
	}
	var netID string
	for _, r := range tbl.LSReplaceOptimsiticMessage {
		if r.OfflineThreadingId == key {
			netID = r.MessageId
		}
	}
	if netID == "" || m.d.Outbox.Find(net, key) == nil {
		return nil // confirmed already, or the socket will
	}
	// Accepted, but the message itself hasn't come back: it's the one sent.
	ms, err := methods.ParseMessageID(netID)
	if err != nil || ms <= 0 {
		ms = m.now().UnixMilli()
	}
	msg := &message{netID: netID, ms: ms, sender: m.self, text: out.Text, media: sent, canUnsend: true}
	if replied != nil {
		msg.reply = &replyRef{netID: replied.netID, sender: replied.sender, text: quoteOf(replied)}
	}
	if cur := m.lookupChat(c.key); cur != nil {
		m.dirty = map[int64]bool{}
		m.arrived(cur, msg, key)
		m.flushDirty()
	}
	return nil
}

// localMedia is a file you sent, served from the copy read when the
// request came until the network's own copy is known; m.mu need not be held.
func (m *Messenger) localMedia(f backend.Upload) proto.Media {
	sum := sha256.Sum256(f.Data)
	id := m.d.Files.Register(ids.FileRef{
		Network: net, Key: "upload:" + hex.EncodeToString(sum[:]), Size: int64(len(f.Data)), Mime: f.Mime, Name: f.Name,
		Source: &inlineSource{Data: f.Data},
	})
	md := proto.Media{Kind: f.Kind, FileID: id, Name: f.Name, Mime: f.Mime, Size: int64(len(f.Data)), Width: f.Width, Height: f.Height}
	if (f.Kind == proto.Photo || f.Kind == proto.GIF) && f.Width > 0 {
		md.Thumbnail = &proto.Image{FileID: id, Width: f.Width, Height: f.Height}
	}
	return md
}

// sendEncrypted sends a message in an encrypted chat: the text, or each
// file as its own message with the text as the last one's caption.
func (m *Messenger) sendEncrypted(ctx context.Context, c *chat, out *backend.Outgoing, replied *message) error {
	e2ee, err := m.encrypted()
	if err != nil {
		return err
	}
	m.mu.Lock()
	jid := c.jid()
	own := e2ee.OwnJID().ToNonAD()
	m.mu.Unlock()
	var contents []*waConsumerApplication.ConsumerApplication_Content
	if len(out.Files) == 0 {
		contents = append(contents, &waConsumerApplication.ConsumerApplication_Content{
			Content: &waConsumerApplication.ConsumerApplication_Content_MessageText{MessageText: &waCommon.MessageText{Text: gproto.String(out.Text)}},
		})
	}
	for i, f := range out.Files {
		caption := ""
		if i == len(out.Files)-1 && f.Kind != proto.Audio && f.Kind != proto.Voice {
			caption = out.Text
		}
		content, err := m.encryptedFile(ctx, e2ee, f, caption)
		if err != nil {
			return err
		}
		contents = append(contents, content)
	}
	if n := len(out.Files); n > 0 && out.Text != "" && (out.Files[n-1].Kind == proto.Audio || out.Files[n-1].Kind == proto.Voice) {
		// A recording carries no caption: the text follows as its own message.
		contents = append(contents, &waConsumerApplication.ConsumerApplication_Content{
			Content: &waConsumerApplication.ConsumerApplication_Content_MessageText{MessageText: &waCommon.MessageText{Text: gproto.String(out.Text)}},
		})
	}
	var parts []proto.Message
	for i, content := range contents {
		app := wrapConsumer(content)
		meta := &waMsgApplication.MessageApplication_Metadata{}
		if i == 0 && replied != nil && replied.wa != nil {
			meta.QuotedMessage = &waMsgApplication.MessageApplication_Metadata_QuotedMessage{
				StanzaID: gproto.String(replied.wa.id), Participant: gproto.String(replied.wa.sender.String()),
			}
		}
		id := strconv.FormatInt(methods.GenerateEpochID(), 10)
		resp, err := e2ee.SendFBMessage(ctx, jid, app, meta, whatsmeow.SendRequestExtra{ID: id})
		if err != nil {
			hlog.Info("messenger: encrypted send failed", hlog.Kind(err))
			err = requestError(err)
			out.Partly(err, parts...) // what went out before the failure, and what didn't
			return err
		}
		ts := resp.Timestamp
		if ts.IsZero() {
			ts = m.now()
		}
		evt := ownEvent(jid, own, id, ts, app, meta)
		m.mu.Lock()
		msg := m.newWA(c, evt, own, m.self)
		m.consumerContent(msg, content)
		cur := m.lookupChat(c.key)
		if cur != nil {
			p, gone := m.keep(cur, msg)
			m.d.Events.MessageDeleted(cur.id, gone)
			cur.activity = max(cur.activity, msg.ms)
			m.recount(cur)
			m.tellPeople(msg)
			parts = append(parts, p...)
		}
		st := m.store
		m.mu.Unlock()
		m.keepOwn(st, evt)
	}
	out.Sent(parts...)
	m.mu.Lock()
	if cur := m.lookupChat(c.key); cur != nil {
		m.touch(cur)
	}
	m.mu.Unlock()
	return nil
}

// encryptedFile uploads a file to the encrypted chats' media servers
// (encrypted on the way) and wraps it as the right kind of message, as the
// connector does.
func (m *Messenger) encryptedFile(ctx context.Context, e2ee e2eeAPI, f backend.Upload, caption string) (*waConsumerApplication.ConsumerApplication_Content, error) {
	data, mime := f.Data, f.Mime
	kind := f.Kind
	if kind == proto.Photo && mime == "image/png" {
		data = nrgbaPNG(data) // Messenger's apps refuse some PNG color models
	}
	mediaType := whatsmeow.MediaDocument
	switch kind {
	case proto.Photo:
		mediaType = whatsmeow.MediaImage
	case proto.Video, proto.GIF:
		// Messenger wants GIFs sent as videos (with GIF playback).
		mediaType = whatsmeow.MediaVideo
	case proto.Audio, proto.Voice:
		mediaType = whatsmeow.MediaAudio
	}
	up, err := e2ee.Upload(ctx, data, mediaType)
	if err != nil {
		hlog.Info("messenger: encrypted upload failed", hlog.Kind(err))
		return nil, proto.Err(proto.NetworkError, "A file couldn't be uploaded to Messenger; try again.")
	}
	w, h := thumbSize(f.Width, f.Height, kind == proto.Photo)
	transport := &waMediaTransport.WAMediaTransport{
		Integral: &waMediaTransport.WAMediaTransport_Integral{
			FileSHA256: up.FileSHA256, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			DirectPath: gproto.String(up.DirectPath), MediaKeyTimestamp: gproto.Int64(m.now().Unix()),
		},
		Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{
			FileLength: gproto.Uint64(uint64(len(data))), Mimetype: gproto.String(mime),
			// The official apps won't show media without this.
			Thumbnail: &waMediaTransport.WAMediaTransport_Ancillary_Thumbnail{ThumbnailWidth: gproto.Uint32(uint32(w)), ThumbnailHeight: gproto.Uint32(uint32(h))},
			ObjectID:  gproto.String(up.ObjectID),
		},
	}
	var text *waCommon.MessageText
	if caption != "" {
		text = &waCommon.MessageText{Text: gproto.String(caption)}
	}
	content := &waConsumerApplication.ConsumerApplication_Content{}
	switch kind {
	case proto.Photo:
		msg := &waConsumerApplication.ConsumerApplication_ImageMessage{Caption: text}
		err = msg.Set(&waMediaTransport.ImageTransport{
			Integral:  &waMediaTransport.ImageTransport_Integral{Transport: transport},
			Ancillary: &waMediaTransport.ImageTransport_Ancillary{Width: gproto.Uint32(uint32(f.Width)), Height: gproto.Uint32(uint32(f.Height))},
		})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_ImageMessage{ImageMessage: msg}
	case proto.Video, proto.GIF:
		msg := &waConsumerApplication.ConsumerApplication_VideoMessage{Caption: text}
		anc := &waMediaTransport.VideoTransport_Ancillary{Width: gproto.Uint32(uint32(f.Width)), Height: gproto.Uint32(uint32(f.Height))}
		if kind == proto.GIF {
			anc.GifPlayback = gproto.Bool(true)
		}
		err = msg.Set(&waMediaTransport.VideoTransport{Integral: &waMediaTransport.VideoTransport_Integral{Transport: transport}, Ancillary: anc})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_VideoMessage{VideoMessage: msg}
	case proto.Audio, proto.Voice:
		msg := &waConsumerApplication.ConsumerApplication_AudioMessage{PTT: gproto.Bool(kind == proto.Voice)}
		err = msg.Set(&waMediaTransport.AudioTransport{
			Integral:  &waMediaTransport.AudioTransport_Integral{Transport: transport},
			Ancillary: &waMediaTransport.AudioTransport_Ancillary{},
		})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_AudioMessage{AudioMessage: msg}
	default:
		msg := &waConsumerApplication.ConsumerApplication_DocumentMessage{FileName: gproto.String(f.Name)}
		err = msg.Set(&waMediaTransport.DocumentTransport{
			Integral:  &waMediaTransport.DocumentTransport_Integral{Transport: transport},
			Ancillary: &waMediaTransport.DocumentTransport_Ancillary{},
		})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_DocumentMessage{DocumentMessage: msg}
	}
	if err != nil {
		return nil, proto.Err(proto.Internal, "The file couldn't be prepared for sending.")
	}
	return content, nil
}

// thumbSize is the thumbnail size Messenger's apps require on media: the
// picture's own, at most 400 pixels a side.
func thumbSize(w, h int, photo bool) (int, int) {
	if w > 400 {
		h, w = h*400/w, 400
	}
	if h > 400 {
		w, h = w*400/h, 400
	}
	if w == 0 && photo {
		w, h = 400, 400
	}
	return w, h
}

// nrgbaPNG re-encodes a PNG that isn't NRGBA, which Messenger can't show.
func nrgbaPNG(data []byte) []byte {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.ColorModel == color.NRGBAModel {
		return data
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	var buf bytes.Buffer
	if png.Encode(&buf, out) != nil {
		return data
	}
	return buf.Bytes()
}

func wrapConsumer(content *waConsumerApplication.ConsumerApplication_Content) *waConsumerApplication.ConsumerApplication {
	return &waConsumerApplication.ConsumerApplication{
		Payload: &waConsumerApplication.ConsumerApplication_Payload{
			Payload: &waConsumerApplication.ConsumerApplication_Payload_Content{Content: content},
		},
	}
}

// ownEvent is a message sent from here as whatsmeow would deliver it, so it
// is kept and read back like any other.
func ownEvent(chat, own waTypes.JID, id string, ts time.Time, sub armadillo.RealMessageApplicationSub, meta *waMsgApplication.MessageApplication_Metadata) *events.FBMessage {
	app := &waMsgApplication.MessageApplication{
		Payload:  &waMsgApplication.MessageApplication_Payload{},
		Metadata: meta,
	}
	sp := &waMsgApplication.MessageApplication_SubProtocolPayload{}
	if consumer, ok := sub.(*waConsumerApplication.ConsumerApplication); ok {
		cm := &waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage{}
		if cm.Set(consumer) == nil {
			sp.SubProtocol = cm
		}
	}
	app.Payload.Content = &waMsgApplication.MessageApplication_Payload_SubProtocol{SubProtocol: sp}
	evt := &events.FBMessage{Message: sub, FBApplication: app}
	evt.Info.Chat = chat
	evt.Info.Sender = own
	evt.Info.IsFromMe = true
	evt.Info.IsGroup = chat.Server == waTypes.GroupServer
	evt.Info.ID = id
	evt.Info.Timestamp = ts
	return evt
}

// keepOwn stores a message sent from here.
func (m *Messenger) keepOwn(st *e2eeStore, evt *events.FBMessage) {
	if st == nil {
		return
	}
	app, err := gproto.Marshal(evt.FBApplication)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = st.put(ctx, storedMessage{Chat: evt.Info.Chat.String(), Sender: evt.Info.Sender.ToNonAD().String(), ID: evt.Info.ID, TS: evt.Info.Timestamp, FromMe: true, App: app})
		cancel()
	}
	if err != nil {
		hlog.Error("messenger: can't keep a sent encrypted message", hlog.Kind(err))
	}
}

// waKey is the key whatsmeow names msg by in edits, reactions and unsends.
func (m *Messenger) waKey(msg *message) *waCommon.MessageKey {
	key := &waCommon.MessageKey{RemoteJID: gproto.String(msg.wa.chat.String()), ID: gproto.String(msg.wa.id)}
	if msg.sender == m.self {
		key.FromMe = gproto.Bool(true)
	}
	if msg.wa.chat.Server == waTypes.GroupServer {
		key.Participant = gproto.String(msg.wa.sender.String())
	}
	return key
}

// sendOwn sends a consumer message about msg (an edit, a reaction, an
// unsend) in an encrypted chat and keeps it like any sent from here.
func (m *Messenger) sendOwn(ctx context.Context, c *chat, payload *waConsumerApplication.ConsumerApplication) (*events.FBMessage, error) {
	e2ee, err := m.encrypted()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	jid := c.jid()
	m.mu.Unlock()
	id := strconv.FormatInt(methods.GenerateEpochID(), 10)
	resp, err := e2ee.SendFBMessage(ctx, jid, payload, nil, whatsmeow.SendRequestExtra{ID: id})
	if err != nil {
		hlog.Info("messenger: encrypted request failed", hlog.Kind(err))
		return nil, requestError(err)
	}
	ts := resp.Timestamp
	if ts.IsZero() {
		ts = m.now()
	}
	return ownEvent(jid, e2ee.OwnJID().ToNonAD(), id, ts, payload, &waMsgApplication.MessageApplication_Metadata{}), nil
}

func (m *Messenger) EditText(ctx context.Context, ref backend.MessageRef, text string) error {
	m.mu.Lock()
	c, msg, err := m.messageOf(ref)
	if err == nil {
		switch {
		case msg.sender != m.self:
			err = proto.Err(proto.BadRequest, "You can only edit your own messages.")
		case len(msg.media) > 0 || msg.service != "" || msg.unsupported != "":
			err = proto.Err(proto.Unsupported, "Only text messages can be edited.")
		case m.now().Unix() > msg.ms/1000+EditWindow:
			err = proto.Err(proto.Unsupported, "This message is too old to edit; Messenger allows 15 minutes.")
		}
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if msg.wa != nil {
		return m.editEncrypted(ctx, c, msg, text)
	}
	ch := make(chan string, 8)
	m.mu.Lock()
	m.edits[msg.netID] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.edits, msg.netID)
		m.mu.Unlock()
	}()
	tbl, err := m.run(ctx, "edit", &socket.EditMessageTask{MessageID: msg.netID, Text: text})
	if err != nil {
		return err
	}
	if len(tbl.LSEditMessage) == 0 || tbl.LSEditMessage[0].Text == text {
		return nil
	}
	// Messenger sometimes first answers with the old text and confirms the
	// edit a moment later (the connector waits the same way).
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == text {
				return nil
			}
		case <-timeout:
			return proto.Err(proto.NetworkError, "Messenger didn't accept the edit.")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Messenger) editEncrypted(ctx context.Context, c *chat, msg *message, text string) error {
	now := m.now().UnixMilli()
	payload := wrapConsumer(&waConsumerApplication.ConsumerApplication_Content{
		Content: &waConsumerApplication.ConsumerApplication_Content_EditMessage{EditMessage: &waConsumerApplication.ConsumerApplication_EditMessage{
			Key: m.waKey(msg), Message: &waCommon.MessageText{Text: gproto.String(text)}, TimestampMS: gproto.Int64(now),
		}},
	})
	evt, err := m.sendOwn(ctx, c, payload)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.applyWA(evt)
	st := m.store
	m.mu.Unlock()
	m.keepOwn(st, evt)
	return nil
}

func (m *Messenger) Delete(ctx context.Context, ref backend.MessageRef) error {
	m.mu.Lock()
	c, msg, err := m.messageOf(ref)
	if err == nil && (msg.sender != m.self || msg.service != "") {
		err = proto.Err(proto.BadRequest, "You can only unsend your own messages.")
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if msg.wa != nil {
		payload := &waConsumerApplication.ConsumerApplication{
			Payload: &waConsumerApplication.ConsumerApplication_Payload{
				Payload: &waConsumerApplication.ConsumerApplication_Payload_ApplicationData{
					ApplicationData: &waConsumerApplication.ConsumerApplication_ApplicationData{
						ApplicationContent: &waConsumerApplication.ConsumerApplication_ApplicationData_Revoke{
							Revoke: &waConsumerApplication.ConsumerApplication_RevokeMessage{Key: m.waKey(msg)},
						},
					},
				},
			},
		}
		if _, err := m.sendOwn(ctx, c, payload); err != nil {
			return err
		}
		m.mu.Lock()
		ref := msg.wa
		m.deleted(c, msg.netID)
		st := m.store
		m.mu.Unlock()
		if st != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = st.remove(ctx, ref.chat.String(), ref.sender.String(), ref.id)
			cancel()
		}
		return nil
	}
	if _, err := m.run(ctx, "delete", &socket.DeleteMessageTask{MessageId: msg.netID}); err != nil {
		return err
	}
	m.mu.Lock()
	m.deleted(c, msg.netID)
	m.mu.Unlock()
	return nil
}

func (m *Messenger) React(ctx context.Context, ref backend.MessageRef, emoji string) error {
	m.mu.Lock()
	c, msg, err := m.messageOf(ref)
	if err == nil && msg.service != "" {
		err = proto.Err(proto.BadRequest, "Events can't be reacted to.")
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	// Messenger keeps reactions without variation selectors.
	emoji = variationselector.Remove(emoji)
	if msg.wa != nil {
		payload := wrapConsumer(&waConsumerApplication.ConsumerApplication_Content{
			Content: &waConsumerApplication.ConsumerApplication_Content_ReactionMessage{ReactionMessage: &waConsumerApplication.ConsumerApplication_ReactionMessage{
				Key: m.waKey(msg), Text: gproto.String(emoji), SenderTimestampMS: gproto.Int64(m.now().UnixMilli()),
			}},
		})
		evt, err := m.sendOwn(ctx, c, payload)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.applyWA(evt)
		st := m.store
		m.mu.Unlock()
		m.keepOwn(st, evt)
		return nil
	}
	m.mu.Lock()
	self := m.self
	m.mu.Unlock()
	if _, err := m.run(ctx, "react", &socket.SendReactionTask{
		ThreadKey: c.threadKey(), MessageID: msg.netID, ActorID: self, Reaction: emoji,
		SyncGroup: 1, SendAttribution: table.MESSENGER_INBOX_IN_THREAD,
	}); err != nil {
		return err
	}
	m.mu.Lock()
	if cur := c.msgs[msg.netID]; cur != nil && cur.setReaction(self, emoji) {
		m.changed(c, cur)
	}
	m.mu.Unlock()
	return nil
}

// MarkRead is the only place read receipts go out: for the chat's
// Facebook messages up to msg, a receipt for the chat; for its encrypted
// ones, a receipt per sender for the unread messages up to msg.
func (m *Messenger) MarkRead(ctx context.Context, ref backend.MessageRef) error {
	m.mu.Lock()
	c, msg, err := m.messageOf(ref)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	upTo := msg.ms
	from := c.readUpTo
	waFrom := c.receipted
	thread := c.threadKey()
	jid := c.jid()
	group := c.kind == proto.Group
	var fb bool
	bySender := map[waTypes.JID][]waTypes.MessageID{}
	for _, other := range c.msgs {
		if other.ms > upTo || other.sender == m.self {
			continue
		}
		if other.wa == nil {
			fb = fb || other.ms > from
			continue
		}
		if other.ms <= waFrom {
			continue
		}
		if strings.HasPrefix(other.netID, "wa-event:") {
			continue
		}
		sender := waTypes.EmptyJID
		if group {
			sender = other.wa.sender
		}
		bySender[sender] = append(bySender[sender], other.wa.id)
	}
	if msg.wa == nil && upTo > from {
		fb = true
	}
	m.mu.Unlock()
	if !fb && len(bySender) == 0 {
		// Nothing new to read: nobody is told anything.
		m.mu.Lock()
		m.readElsewhere(c.key, upTo)
		m.mu.Unlock()
		return nil
	}

	if fb {
		if _, err := m.run(ctx, "mark_read", &socket.ThreadMarkReadTask{ThreadId: thread, LastReadWatermarkTs: upTo, SyncGroup: 1}); err != nil {
			return err
		}
	}
	if len(bySender) > 0 {
		e2ee, err := m.encrypted()
		if err != nil {
			return err
		}
		for sender, list := range bySender {
			if err := e2ee.MarkRead(ctx, list, m.now(), jid, sender); err != nil {
				hlog.Info("messenger: encrypted read receipt failed", hlog.Kind(err))
				return requestError(err)
			}
		}
	}
	m.mu.Lock()
	m.readElsewhere(c.key, upTo)
	m.mu.Unlock()
	return nil
}

// SetTyping is the only place typing goes out. Encrypted chats send it as
// a chat state; Messenger may not pass it on, since it does that only for
// devices marked online, and this one never is.
func (m *Messenger) SetTyping(ctx context.Context, ref backend.ChatRef, typing bool) error {
	m.mu.Lock()
	c, err := m.chatOf(ref.ID)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	encrypted := m.sendsEncrypted(c)
	jid, thread, group, ttype := c.jid(), c.threadKey(), c.kind == proto.Group, c.ttype
	m.mu.Unlock()
	if encrypted {
		e2ee, err := m.encrypted()
		if err != nil {
			return err
		}
		state := waTypes.ChatPresencePaused
		if typing {
			state = waTypes.ChatPresenceComposing
		}
		return requestError(e2ee.SendChatPresence(ctx, jid, state, waTypes.ChatPresenceMediaText))
	}
	task := &socket.UpdatePresenceTask{ThreadKey: thread, SyncGroup: 1, ThreadType: int64(ttype)}
	if group {
		task.IsGroupThread = 1
	}
	if typing {
		task.IsTyping = 1
	}
	if err := guardTasks("typing", []socket.Task{task}); err != nil {
		return err
	}
	meta, err := m.connected()
	if err != nil {
		return err
	}
	return requestError(meta.ExecuteStatelessTask(ctx, task))
}

func (m *Messenger) Mute(ctx context.Context, ref backend.ChatRef, muted bool) error {
	m.mu.Lock()
	c, err := m.chatOf(ref.ID)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	until := int64(0)
	if muted {
		until = -1 // for good
	}
	if _, err := m.run(ctx, "mute", &socket.MuteThreadTask{ThreadKey: c.threadKey(), MuteExpireTimeMS: until, SyncGroup: 1}); err != nil {
		return err
	}
	m.mu.Lock()
	if cur := m.lookupChat(c.key); cur != nil && cur.muteUntil != until {
		cur.muteUntil = until
		m.touch(cur)
	}
	m.mu.Unlock()
	return nil
}

// Search asks Messenger for people and groups matching query, for opening
// a chat with.
func (m *Messenger) Search(ctx context.Context, query string) ([]proto.SearchResult, error) {
	tbl, err := m.run(ctx, "search", &socket.SearchUserTask{
		Query: query,
		SupportedTypes: []table.SearchType{
			table.SearchTypeContact, table.SearchTypeGroup, table.SearchTypePage, table.SearchTypeNonContact,
			table.SearchTypeCommunityMessagingThread,
		},
		SurfaceType: 5,
	})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	results := []proto.SearchResult{}
	seen := map[int64]bool{}
	for _, r := range tbl.LSInsertSearchResult {
		if r.ThreadType.IsOneToOne() {
			fbid := r.GetFBID()
			if fbid == 0 || fbid == m.self || seen[fbid] || !r.CanViewerMessage {
				continue
			}
			seen[fbid] = true
			p := m.person(fbid)
			if !p.known {
				p.name = oneLine(r.DisplayName)
				if allowedMediaURL(r.ProfilePicUrl) {
					p.avatar = r.ProfilePicUrl
				}
			}
			m.tellUser(p, false)
			res := proto.SearchResult{UserID: p.id, Title: firstNonEmpty(p.name, oneLine(r.DisplayName)), Username: p.username, Kind: proto.DM}
			if c := m.chats[fbid]; c != nil && m.visible(c) {
				res.ChatID = c.id
			}
			results = append(results, res)
			continue
		}
		key, err := strconv.ParseInt(r.ResultId, 10, 64)
		if err != nil {
			continue
		}
		if c := m.lookupChat(key); c != nil && m.visible(c) && !seen[c.key] {
			seen[c.key] = true
			if !m.sent[c.id] {
				m.sent[c.id] = true
				m.sendChat(c)
			}
			results = append(results, proto.SearchResult{ChatID: c.id, Title: m.title(c), Kind: c.kind})
		}
	}
	return results, nil
}

// OpenDM is the chat with a person, made on Messenger if there's none. New
// one-to-one chats are end-to-end encrypted when the encrypted chats are
// connected, as Messenger's own apps make them.
func (m *Messenger) OpenDM(ctx context.Context, ref backend.UserRef) (int64, error) {
	fbid, err := strconv.ParseInt(ref.NetID, 10, 64)
	if err != nil || fbid <= 0 {
		return 0, proto.ErrNoUser
	}
	m.mu.Lock()
	if c := m.chats[fbid]; c != nil && c.kind == proto.DM && m.visible(c) {
		if !m.sent[c.id] {
			m.sent[c.id] = true
			m.sendChat(c)
		}
		m.mu.Unlock()
		return c.id, nil
	}
	e2ee := m.e2ee != nil
	m.mu.Unlock()
	if _, err := m.run(ctx, "open_dm", &socket.CreateThreadTask{ThreadFBID: fbid, SyncGroup: 1}); err != nil {
		return 0, err
	}
	if e2ee {
		tbl, err := m.run(ctx, "open_dm", &socket.CreateWhatsAppThreadTask{
			WAJID: fbid, OfflineThreadKey: methods.GenerateEpochID(), ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE,
			FolderType: table.INBOX, BumpTimestampMS: m.now().UnixMilli(),
		})
		if err != nil {
			e2ee = false
		} else if len(tbl.LSIssueNewTask) > 0 {
			follow := make([]socket.Task, len(tbl.LSIssueNewTask))
			for i, t := range tbl.LSIssueNewTask {
				follow[i] = t
			}
			if err := guardTasks("open_dm", follow); err == nil {
				if _, err := m.run(ctx, "open_dm", follow...); err != nil {
					hlog.Info("messenger: encrypted chat setup incomplete", hlog.Kind(err))
				}
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.chatByKey(fbid)
	if !c.known {
		c.known = true
		c.kind, c.other = proto.DM, fbid
		c.ttype = table.ONE_TO_ONE
		if e2ee {
			c.ttype = table.ENCRYPTED_OVER_WA_ONE_TO_ONE
			c.encrypted = true
			c.log.SetComplete(true)
		}
		c.activity = max(c.activity, m.now().UnixMilli())
	}
	m.sent[c.id] = true
	m.sendChat(c)
	return c.id, nil
}

func (m *Messenger) GetMessage(ctx context.Context, ref backend.MessageRef) (proto.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.chatOf(ref.Chat.ID)
	if err != nil {
		return proto.Message{}, err
	}
	msg, ok := c.log.Get(ref.ID)
	if !ok {
		return proto.Message{}, proto.ErrNoMessage
	}
	return msg, nil
}
