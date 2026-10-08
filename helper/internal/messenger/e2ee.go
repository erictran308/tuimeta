// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	armadillo "go.mau.fi/whatsmeow/proto"
	"go.mau.fi/whatsmeow/proto/waArmadilloApplication"
	"go.mau.fi/whatsmeow/proto/waArmadilloXMA"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// openStore opens the encrypted chats' database once the Messenger socket is
// up, and reads back the encrypted messages it kept: their chats only have
// what arrived since this device was linked, so that's all there is.
func (m *Messenger) openStore(gen int) {
	m.mu.Lock()
	if m.gen != gen || m.store != nil {
		m.mu.Unlock()
		return
	}
	device := ""
	if m.sess != nil {
		device = m.sess.WADevice
	}
	self := m.self
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path, err := m.d.Session.Path(storeFile)
	if err != nil {
		hlog.Error("messenger: no place for the encrypted chats' store", hlog.Kind(err))
		return
	}
	st, err := openStore(ctx, path)
	if err != nil {
		hlog.Error("messenger: can't open the encrypted chats' store", hlog.Kind(err))
		return
	}
	owner, _ := waTypes.ParseJID(device)
	if device == "" || int64(owner.UserInt()) != self {
		// Another account's (or no) device: nothing of it is shown here.
		if err := st.clear(ctx); err != nil {
			hlog.Error("messenger: can't clear the encrypted chats' store", hlog.Kind(err))
		}
	}
	// Trim anything an older build kept past the per-chat cap before it's
	// read back, so the reload stays bounded too.
	if err := st.prune(ctx); err != nil {
		hlog.Error("messenger: can't prune the encrypted chats' store", hlog.Kind(err))
	}
	kept, err := st.all(ctx)
	if err != nil {
		hlog.Error("messenger: can't read the encrypted chats' store", hlog.Kind(err))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != gen || m.store != nil {
		st.Close()
		return
	}
	m.store = st
	m.replaying = true
	m.dirty = map[int64]bool{}
	for _, s := range kept {
		if evt := decodeStored(s); evt != nil {
			m.applyWA(evt)
		}
	}
	for _, c := range m.chats {
		// Receipts went out for what was read before this run.
		c.receipted = max(c.receipted, c.readUpTo)
	}
	m.flushDirty()
	m.replaying = false
	hlog.Info("messenger: encrypted messages read back", hlog.Int("count", int64(len(kept))))
}

// decodeStored rebuilds a kept message as whatsmeow delivered it.
func decodeStored(s storedMessage) *events.FBMessage {
	chat, err1 := waTypes.ParseJID(s.Chat)
	sender, err2 := waTypes.ParseJID(s.Sender)
	if err1 != nil || err2 != nil {
		return nil
	}
	var app waMsgApplication.MessageApplication
	if err := gproto.Unmarshal(s.App, &app); err != nil {
		return nil
	}
	sub, err := decodeApplication(&app)
	if err != nil {
		return nil
	}
	evt := &events.FBMessage{Message: sub, FBApplication: &app}
	evt.Info.Chat = chat
	evt.Info.Sender = sender
	evt.Info.IsFromMe = s.FromMe
	evt.Info.IsGroup = chat.Server == waTypes.GroupServer
	evt.Info.ID = s.ID
	evt.Info.Timestamp = s.TS
	return evt
}

// decodeApplication is the message inside an application payload, as
// whatsmeow decodes it.
func decodeApplication(app *waMsgApplication.MessageApplication) (armadillo.MessageApplicationSub, error) {
	switch sub := app.GetPayload().GetSubProtocol().GetSubProtocol().(type) {
	case *waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage:
		return sub.Decode()
	case *waMsgApplication.MessageApplication_SubProtocolPayload_Armadillo:
		return sub.Decode()
	}
	return nil, errors.New("unsupported subprotocol")
}

// connectE2EE registers this login as an encrypted-chat device of the
// account if it isn't one yet, as Messenger's web client does, and connects
// the encrypted chats' socket.
func (m *Messenger) connectE2EE(gen int) {
	m.mu.Lock()
	cli, st, life, self := m.msgx, m.store, m.life, m.self
	device := ""
	if m.sess != nil {
		device = m.sess.WADevice
	}
	current := m.gen == gen && m.wa == nil
	m.mu.Unlock()
	if !current || cli == nil || st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(life, 2*time.Minute)
	defer cancel()
	fail := func(step string, err error) {
		hlog.Warn("messenger: encrypted chats didn't connect", hlog.Str("step", step), hlog.Kind(err))
		m.d.Events.Error(net, "Encrypted chats couldn't connect; they'll be tried again when Messenger reconnects.")
	}
	dev, isNew, err := st.device(ctx, device)
	if err != nil {
		fail("device", err)
		return
	}
	cli.SetDevice(dev)
	if isNew {
		if err := st.dropDevices(ctx); err != nil {
			hlog.Warn("messenger: can't drop old devices", hlog.Kind(err))
		}
		if err := cli.RegisterE2EE(ctx, self); err != nil {
			fail("register", err)
			return
		}
		if err := dev.Save(ctx); err != nil {
			fail("save", err)
			return
		}
		m.mu.Lock()
		if m.sess != nil && dev.ID != nil {
			m.sess.WADevice = dev.ID.String()
			m.sess.Cookies = cookieValues(cli)
		}
		var sess savedSession
		if m.sess != nil {
			sess = *m.sess
		}
		m.mu.Unlock()
		if err := m.d.Session.SaveJSON(sessionFile, sess); err != nil {
			hlog.Error("messenger: can't save session", hlog.Kind(err))
		}
		hlog.Info("messenger: registered an encrypted-chat device")
	}
	wa, err := cli.PrepareE2EEClient()
	if err != nil {
		fail("prepare", err)
		return
	}
	configureE2EE(wa)
	wa.AddEventHandlerWithSuccessStatus(func(evt any) bool { return m.onE2EE(gen, evt) })
	m.mu.Lock()
	if m.gen != gen || m.closed {
		m.mu.Unlock()
		return
	}
	m.wa = wa
	m.e2ee = &e2eeConn{cli: wa}
	m.mu.Unlock()
	if err := wa.Connect(); err != nil {
		fail("connect", err)
	}
}

// onE2EE handles whatsmeow's events. It returns false only when a message
// couldn't be handled, so whatsmeow doesn't acknowledge it and the server
// delivers it again.
func (m *Messenger) onE2EE(gen int, evt any) (ok bool) {
	defer func() {
		if v := recover(); v != nil {
			hlog.Recovered("messenger e2ee event", v)
			ok = true
		}
	}()
	m.mu.Lock()
	current := m.gen == gen && !m.closed
	m.mu.Unlock()
	if !current {
		return true
	}
	switch e := evt.(type) {
	case *events.FBMessage:
		m.receiveWA(e)
	case *events.UndecryptableMessage:
		m.undecryptable(e)
	case *events.Receipt:
		m.waReceipt(e)
	case *events.ChatPresence:
		m.waTyping(e)
	case *events.GroupInfo:
		m.waGroupInfo(e)
	case *events.Connected:
		hlog.Info("messenger: encrypted chats connected")
		m.mu.Lock()
		m.e2eeOK = true
		m.mu.Unlock()
	case *events.Disconnected:
		hlog.Info("messenger: encrypted chats disconnected; reconnecting")
		m.mu.Lock()
		m.e2eeOK = false
		m.mu.Unlock()
	case *events.LoggedOut:
		hlog.Warn("messenger: encrypted-chat device logged out")
		m.forgetDevice(gen)
		m.fullReconnect(gen)
	case *events.ConnectFailure:
		hlog.Warn("messenger: encrypted chats refused", hlog.Int("reason", int64(e.Reason)))
		switch e.Reason {
		case events.ConnectFailureNotFound, events.ConnectFailureClientUnknown:
			m.forgetDevice(gen)
			m.fullReconnect(gen)
		case events.ConnectFailureGeneric:
			m.fullReconnect(gen)
		}
	case *events.CATRefreshError:
		hlog.Warn("messenger: encrypted chats' token couldn't be refreshed", hlog.Kind(e.Error))
		m.fullReconnect(gen)
	case *events.StreamReplaced:
		m.d.Events.Error(net, "Encrypted chats were opened by another copy of tuimeta with the same login; close it, then restart this one.")
	case *events.ClientOutdated:
		m.d.Events.Error(net, "Messenger says this version can't open encrypted chats any more; update tuimeta.")
	case *events.TemporaryBan:
		m.d.Events.Error(net, "Messenger has temporarily blocked encrypted chats for this account; try again later.")
	}
	return true
}

// forgetDevice drops a device the server no longer knows, so the next
// connection registers a new one.
func (m *Messenger) forgetDevice(gen int) {
	m.mu.Lock()
	wa := m.wa
	if m.gen != gen {
		m.mu.Unlock()
		return
	}
	if m.sess != nil {
		m.sess.WADevice = ""
	}
	m.wa, m.e2ee, m.e2eeOK = nil, nil, false
	m.mu.Unlock()
	if wa != nil {
		wa.Disconnect()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := wa.Store.Delete(ctx); err != nil {
			hlog.Warn("messenger: can't delete the old device", hlog.Kind(err))
		}
		cancel()
	}
}

// receiveWA handles an encrypted message: it's applied, then kept in the
// store before whatsmeow acknowledges it (SynchronousAck).
func (m *Messenger) receiveWA(evt *events.FBMessage) {
	m.mu.Lock()
	keep, revoked := m.applyWA(evt)
	st := m.store
	m.mu.Unlock()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	switch {
	case revoked != nil:
		err = st.remove(ctx, revoked.chat.String(), revoked.sender.String(), revoked.id)
	case keep && evt.FBApplication != nil:
		var app []byte
		app, err = gproto.Marshal(evt.FBApplication)
		if err == nil {
			chat := evt.Info.Chat.String()
			err = st.put(ctx, storedMessage{
				Chat: chat, Sender: evt.Info.Sender.ToNonAD().String(), ID: evt.Info.ID,
				TS: evt.Info.Timestamp, FromMe: evt.Info.IsFromMe, App: app,
			})
			if err == nil {
				err = st.pruneChat(ctx, chat)
			}
		}
	}
	if err != nil {
		hlog.Error("messenger: can't keep an encrypted message", hlog.Kind(err))
	}
}

// waChat is the chat an encrypted message is in; m.mu is held.
func (m *Messenger) waChat(jid waTypes.JID) *chat {
	key, err := strconv.ParseInt(jid.User, 10, 64)
	if err != nil || key == 0 {
		return nil
	}
	c := m.chatByKey(key)
	c.encrypted = true
	c.server = jid.Server
	if jid.Server == waTypes.GroupServer {
		if c.kind != proto.Group {
			c.kind = proto.Group
			c.ttype = table.ENCRYPTED_OVER_WA_GROUP
		}
		if !c.known {
			m.askGroup(c, jid)
		}
	} else if c.ttype == table.UNKNOWN_THREAD_TYPE {
		c.ttype = table.ENCRYPTED_OVER_WA_ONE_TO_ONE
		c.kind, c.other = proto.DM, key
	}
	if len(c.msgs) == 0 {
		c.refreshComplete()
	}
	return c
}

// askGroup asks the encrypted chats' server for a group's name and members,
// once; m.mu is held.
func (m *Messenger) askGroup(c *chat, jid waTypes.JID) {
	if m.asked[c.key] || m.e2ee == nil || m.replaying {
		return
	}
	m.asked[c.key] = true
	e2ee, life, gen := m.e2ee, m.life, m.gen
	hlog.Go("messenger group", func() {
		info, err := e2ee.GetGroupInfo(life, jid)
		if err != nil {
			hlog.Info("messenger: group lookup failed", hlog.Kind(err))
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.gen != gen {
			return
		}
		if c := m.lookupChat(c.key); c != nil {
			c.known = true
			if info.Name != "" {
				c.name = info.Name
			}
			c.members = c.members[:0]
			for _, p := range info.Participants {
				c.members = append(c.members, int64(p.JID.UserInt()))
			}
			m.touch(c)
		}
	})
}

// applyWA applies an encrypted message (or an edit, reaction or unsend of
// one); m.mu is held. It says whether the message should be kept, or which
// kept message was unsent.
func (m *Messenger) applyWA(evt *events.FBMessage) (keep bool, revoked *waRef) {
	info := evt.Info
	c := m.waChat(info.Chat)
	if c == nil {
		return false, nil
	}
	sender := info.Sender.ToNonAD()
	fbid := int64(sender.UserInt())
	if info.IsFromMe {
		fbid = m.self
	}
	switch typed := evt.Message.(type) {
	case *waConsumerApplication.ConsumerApplication:
		switch p := typed.GetPayload().GetPayload().(type) {
		case *waConsumerApplication.ConsumerApplication_Payload_Content:
			switch content := p.Content.GetContent().(type) {
			case *waConsumerApplication.ConsumerApplication_Content_EditMessage:
				target := m.waTarget(c, info.Chat, sender, content.EditMessage.GetKey())
				m.waEdit(c, target, fbid, content.EditMessage)
				return true, nil
			case *waConsumerApplication.ConsumerApplication_Content_ReactionMessage:
				target := m.waTarget(c, info.Chat, sender, content.ReactionMessage.GetKey())
				if msg := c.msgs[target]; msg != nil && msg.setReaction(fbid, content.ReactionMessage.GetText()) {
					m.changed(c, msg)
				}
				return true, nil
			case *waConsumerApplication.ConsumerApplication_Content_PollUpdateMessage:
				return false, nil // a vote; polls aren't shown
			}
			msg := m.newWA(c, evt, sender, fbid)
			m.consumerContent(msg, p.Content)
			m.waArrived(c, msg)
			return true, nil
		case *waConsumerApplication.ConsumerApplication_Payload_ApplicationData:
			if rv := p.ApplicationData.GetRevoke(); rv != nil {
				target := m.waTarget(c, info.Chat, sender, rv.GetKey())
				msg := c.msgs[target]
				if msg == nil || msg.sender != fbid {
					return false, nil // only a message's sender unsends it
				}
				ref := msg.wa
				m.deleted(c, target)
				return false, ref
			}
			return false, nil
		}
		return false, nil
	case *waArmadilloApplication.Armadillo:
		msg := m.newWA(c, evt, sender, fbid)
		m.armadilloContent(c, msg, typed.GetPayload().GetContent())
		m.waArrived(c, msg)
		return true, nil
	}
	if evt.Message == nil && evt.FBApplication.GetMetadata().GetChatEphemeralSetting() != nil {
		return false, nil // a disappearing-messages setting, not a message
	}
	msg := m.newWA(c, evt, sender, fbid)
	msg.unsupported = "[Unsupported message]"
	m.waArrived(c, msg)
	return false, nil
}

// newWA is the common part of an encrypted message; m.mu is held.
func (m *Messenger) newWA(c *chat, evt *events.FBMessage, sender waTypes.JID, fbid int64) *message {
	info := evt.Info
	msg := &message{
		netID: waNetID(sender, info.ID), ms: info.Timestamp.UnixMilli(), sender: fbid, canUnsend: true,
		wa: &waRef{chat: info.Chat, sender: sender, id: info.ID},
	}
	meta := evt.FBApplication.GetMetadata()
	msg.forwarded = meta.GetIsForwarded()
	if q := meta.GetQuotedMessage(); q != nil && q.GetStanzaID() != "" {
		msg.reply = m.waQuote(c, q)
	}
	return msg
}

// waQuote is the message an encrypted message answers; m.mu is held.
func (m *Messenger) waQuote(c *chat, q *waMsgApplication.MessageApplication_Metadata_QuotedMessage) *replyRef {
	pcp, _ := waTypes.ParseJID(q.GetParticipant())
	var ref *replyRef
	if !pcp.IsEmpty() {
		ref = &replyRef{netID: waNetID(pcp.ToNonAD(), q.GetStanzaID()), sender: int64(pcp.UserInt())}
	} else {
		// A one-to-one chat's quote may name no participant: it's one of
		// the two people's.
		for _, who := range []int64{c.other, m.self} {
			id := waNetID(waTypes.NewJID(strconv.FormatInt(who, 10), waTypes.MessengerServer), q.GetStanzaID())
			if c.msgs[id] != nil {
				ref = &replyRef{netID: id, sender: who}
			}
		}
		if ref == nil {
			ref = &replyRef{netID: "wa:?:" + q.GetStanzaID()}
		}
	}
	if replied := c.msgs[ref.netID]; replied != nil {
		ref.sender = replied.sender
		ref.text = quoteOf(replied)
	} else if p := q.GetPayload(); p != nil {
		if sub, ok := p.GetSubProtocol().GetSubProtocol().(*waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage); ok {
			if cm, err := sub.Decode(); err == nil {
				if t := cm.GetPayload().GetContent().GetMessageText().GetText(); t != "" {
					ref.text = snippet(t)
				}
			}
		}
	}
	return ref
}

// waTarget is the id among c's messages of the message key names, as the
// connector works it out: a key not from the sender of the edit or reaction
// in a one-to-one chat is the other person's; m.mu is held.
func (m *Messenger) waTarget(c *chat, chatJID, sender waTypes.JID, key *waCommon.MessageKey) string {
	target := sender
	if !key.GetFromMe() {
		if p := key.GetParticipant(); p != "" {
			parsed, err := waTypes.ParseJID(p)
			if err != nil {
				return ""
			}
			target = parsed.ToNonAD()
		} else if chatJID.Server != waTypes.GroupServer {
			own := strconv.FormatInt(m.self, 10)
			if sender.User == own {
				target = waTypes.NewJID(chatJID.User, waTypes.MessengerServer)
			} else {
				target = waTypes.NewJID(own, waTypes.MessengerServer)
			}
		} else {
			return ""
		}
	}
	return waNetID(target, key.GetID())
}

// waArrived records an encrypted message; m.mu is held.
func (m *Messenger) waArrived(c *chat, msg *message) {
	if m.replaying {
		m.keep(c, msg)
		c.activity = max(c.activity, msg.ms)
		m.recount(c)
		m.touch(c)
		return
	}
	m.arrived(c, msg, "")
}

// waEdit replaces the text of an encrypted message; only its sender may,
// and an older edit never undoes a newer one; m.mu is held.
func (m *Messenger) waEdit(c *chat, target string, fbid int64, e *waConsumerApplication.ConsumerApplication_EditMessage) {
	msg := c.msgs[target]
	if msg == nil || msg.sender != fbid || e.GetTimestampMS() <= msg.editTS {
		return
	}
	msg.editTS = e.GetTimestampMS()
	msg.text, msg.mentions = m.waText(e.GetMessage())
	msg.edited = true
	m.changed(c, msg)
}

// consumerContent fills in a consumer message's content; m.mu is held.
func (m *Messenger) consumerContent(msg *message, content *waConsumerApplication.ConsumerApplication_Content) {
	switch ct := content.GetContent().(type) {
	case *waConsumerApplication.ConsumerApplication_Content_MessageText:
		msg.text, msg.mentions = m.waText(ct.MessageText)
	case *waConsumerApplication.ConsumerApplication_Content_ExtendedTextMessage:
		x := ct.ExtendedTextMessage
		msg.text, msg.mentions = m.waText(x.GetText())
		link := webLink(firstNonEmpty(x.GetCanonicalURL(), x.GetMatchedText()))
		if link != "" && (x.GetTitle() != "" || x.GetDescription() != "") {
			msg.preview = &proto.LinkPreview{URL: link, Title: oneLine(x.GetTitle()), Description: oneLine(x.GetDescription())}
			if thumb, err := x.DecodeThumbnail(); err == nil && thumb != nil {
				anc := thumb.GetAncillary()
				if md := m.waMedia(thumb.GetIntegral().GetTransport(), proto.Photo, whatsmeow.MediaImage, "preview", int(anc.GetWidth()), int(anc.GetHeight()), 0); md.FileID != 0 {
					msg.preview.Image = &proto.Image{FileID: md.FileID, Width: md.Width, Height: md.Height}
				}
			}
		}
	case *waConsumerApplication.ConsumerApplication_Content_ImageMessage:
		if tr, err := ct.ImageMessage.Decode(); err == nil {
			anc := tr.GetAncillary()
			kind := proto.Photo
			if tr.GetIntegral().GetTransport().GetAncillary().GetMimetype() == "image/gif" {
				kind = proto.GIF
			}
			msg.media = append(msg.media, m.waMedia(tr.GetIntegral().GetTransport(), kind, whatsmeow.MediaImage, "", int(anc.GetWidth()), int(anc.GetHeight()), 0))
		}
		msg.text, msg.mentions = m.waText(ct.ImageMessage.GetCaption())
	case *waConsumerApplication.ConsumerApplication_Content_StickerMessage:
		if tr, err := ct.StickerMessage.Decode(); err == nil {
			anc := tr.GetAncillary()
			if anc.GetReceiverFetchID() != "" {
				msg.unsupported = "[Sticker]" // a sticker the app fetches by itself
			} else {
				msg.media = append(msg.media, m.waMedia(tr.GetIntegral().GetTransport(), proto.Sticker, whatsmeow.MediaImage, "", int(anc.GetWidth()), int(anc.GetHeight()), 0))
			}
		}
	case *waConsumerApplication.ConsumerApplication_Content_VideoMessage:
		if tr, err := ct.VideoMessage.Decode(); err == nil {
			anc := tr.GetAncillary()
			kind := proto.Video
			mime := tr.GetIntegral().GetTransport().GetAncillary().GetMimetype()
			switch {
			case strings.HasPrefix(mime, "image/"):
				kind = proto.Photo // Messenger sometimes sends pictures as videos
			case anc.GetGifPlayback():
				kind = proto.GIF
			}
			msg.media = append(msg.media, m.waMedia(tr.GetIntegral().GetTransport(), kind, whatsmeow.MediaVideo, "", int(anc.GetWidth()), int(anc.GetHeight()), int(anc.GetSeconds())))
		}
		msg.text, msg.mentions = m.waText(ct.VideoMessage.GetCaption())
	case *waConsumerApplication.ConsumerApplication_Content_AudioMessage:
		if tr, err := ct.AudioMessage.Decode(); err == nil {
			// Messenger's own apps don't mark voice messages as such, so
			// every recording is one, as in the connector.
			msg.media = append(msg.media, m.waMedia(tr.GetIntegral().GetTransport(), proto.Voice, whatsmeow.MediaAudio, "", 0, 0, int(tr.GetAncillary().GetSeconds())))
		}
	case *waConsumerApplication.ConsumerApplication_Content_DocumentMessage:
		if tr, err := ct.DocumentMessage.Decode(); err == nil {
			msg.media = append(msg.media, m.waMedia(tr.GetIntegral().GetTransport(), proto.FileMedia, whatsmeow.MediaDocument, ct.DocumentMessage.GetFileName(), 0, 0, 0))
		}
	case *waConsumerApplication.ConsumerApplication_Content_ViewOnceMessage:
		kind := proto.Photo
		if ct.ViewOnceMessage.GetVideoMessage() != nil {
			kind = proto.Video
		}
		msg.media = append(msg.media, proto.Media{Kind: kind, ViewOnce: true})
	case *waConsumerApplication.ConsumerApplication_Content_LocationMessage:
		msg.unsupported = "[Location]"
	case *waConsumerApplication.ConsumerApplication_Content_LiveLocationMessage:
		msg.unsupported = "[Live location]"
	case *waConsumerApplication.ConsumerApplication_Content_ContactMessage:
		msg.unsupported = "[Contact]"
	case *waConsumerApplication.ConsumerApplication_Content_ContactsArrayMessage:
		msg.unsupported = "[Contacts]"
	case *waConsumerApplication.ConsumerApplication_Content_PollCreationMessage:
		msg.unsupported = "[Poll]"
	case *waConsumerApplication.ConsumerApplication_Content_GroupInviteMessage:
		msg.unsupported = "[Group invite]"
	case *waConsumerApplication.ConsumerApplication_Content_StatusTextMessage:
		msg.text, msg.mentions = m.waText(ct.StatusTextMessage.GetText().GetText())
	default:
		msg.unsupported = "[Unsupported message]"
	}
	if msg.text == "" && len(msg.media) == 0 && msg.unsupported == "" && msg.preview == nil {
		msg.unsupported = "[Unsupported message]"
	}
}

// armadilloContent fills in a Messenger-specific encrypted message; m.mu is
// held.
func (m *Messenger) armadilloContent(c *chat, msg *message, content *waArmadilloApplication.Armadillo_Content) {
	switch ct := content.GetContent().(type) {
	case *waArmadilloApplication.Armadillo_Content_ExtendedContentMessage:
		m.waXMA(msg, ct.ExtendedContentMessage)
	case *waArmadilloApplication.Armadillo_Content_RavenMessage_:
		msg.media = append(msg.media, ravenMedia(ct.RavenMessage))
	case *waArmadilloApplication.Armadillo_Content_RavenMessageMsgr:
		msg.media = append(msg.media, ravenMedia(ct.RavenMessageMsgr))
	case *waArmadilloApplication.Armadillo_Content_ImageGalleryMessage_:
		images, err := ct.ImageGalleryMessage.Decode()
		if err == nil {
			for _, img := range images {
				anc := img.GetAncillary()
				msg.media = append(msg.media, m.waMedia(img.GetIntegral().GetTransport(), proto.Photo, whatsmeow.MediaImage, "", int(anc.GetWidth()), int(anc.GetHeight()), 0))
			}
		}
	case *waArmadilloApplication.Armadillo_Content_BumpExistingMessage_:
		if key := ct.BumpExistingMessage.GetKey(); key != nil {
			target := m.waTarget(c, msg.wa.chat, msg.wa.sender, key)
			ref := &replyRef{netID: target}
			if replied := c.msgs[target]; replied != nil {
				ref.sender, ref.text = replied.sender, quoteOf(replied)
			}
			msg.reply = ref
		}
		msg.unsupported = "[Bumped a message]"
	case *waArmadilloApplication.Armadillo_Content_CommonSticker_:
		msg.text = "👍"
	case *waArmadilloApplication.Armadillo_Content_PaymentsTransactionMessage_:
		msg.unsupported = "[Payment]"
	case *waArmadilloApplication.Armadillo_Content_NoteReplyMessage_:
		msg.text, msg.mentions = m.waText(ct.NoteReplyMessage.GetTextContent())
		if msg.text == "" {
			msg.unsupported = "[Reply to a note]"
		}
	case *waArmadilloApplication.Armadillo_Content_ScreenshotAction_:
		msg.service = m.displayName(msg.sender) + " took a screenshot"
	default:
		msg.unsupported = "[Unsupported message]"
	}
	if msg.text == "" && len(msg.media) == 0 && msg.unsupported == "" && msg.preview == nil && msg.service == "" {
		msg.unsupported = "[Unsupported message]"
	}
}

// ravenMedia is a view-once photo or video: shown only on the phone, so it
// has no file.
func ravenMedia(r *waArmadilloApplication.Armadillo_Content_RavenMessage) proto.Media {
	kind := proto.Photo
	if _, ok := r.GetMediaContent().(*waArmadilloApplication.Armadillo_Content_RavenMessage_VideoMessage); ok {
		kind = proto.Video
	}
	return proto.Media{Kind: kind, ViewOnce: true}
}

// waXMA reads a shared link or location; m.mu is held.
func (m *Messenger) waXMA(msg *message, x *waArmadilloXMA.ExtendedContentMessage) {
	msg.text = x.GetMessageText()
	for _, mn := range x.GetMentions() {
		if jid, err := waTypes.ParseJID(mn.GetMentionedJID()); err == nil {
			msg.mentions = append(msg.mentions, mention{offset: int(mn.GetOffset()), length: int(mn.GetLength()), fbid: int64(jid.UserInt())})
		}
	}
	if x.GetTargetType() == waArmadilloXMA.ExtendedContentMessage_FB_STORY_REPLY {
		setUnsupported(msg, "[Story reply]")
		return
	}
	for _, cta := range x.GetCtas() {
		native := cta.GetNativeURL()
		if strings.HasPrefix(native, "messenger://location_share") {
			setUnsupported(msg, "[Location]")
			return
		}
		link := webLink(firstNonEmpty(cta.GetActionURL(), native))
		if link != "" && msg.preview == nil {
			msg.preview = &proto.LinkPreview{URL: link, Title: oneLine(x.GetTitleText()), Description: oneLine(x.GetSubtitleText())}
		}
	}
	linkOnly(msg)
	if msg.text == "" && msg.preview == nil {
		setUnsupported(msg, "[Shared content]")
	}
}

// waText is an encrypted message's text and the mentions in it. Mentions
// written as "@<id>@msgr" in the text are replaced by "@" and the person's
// name, as the connector shows them; m.mu is held.
func (m *Messenger) waText(t *waCommon.MessageText) (string, []mention) {
	text := t.GetText()
	if text == "" {
		return "", nil
	}
	var out []mention
	if mentions := t.GetMentions(); len(mentions) > 0 {
		for _, mn := range mentions {
			jid, err := waTypes.ParseJID(mn.GetMentionedJID())
			if err != nil {
				continue
			}
			out = append(out, mention{offset: int(mn.GetOffset()), length: int(mn.GetLength()), fbid: int64(jid.UserInt())})
		}
		return text, out
	}
	type spot struct {
		at, end int
		fbid    int64
	}
	var spots []spot
	for _, raw := range t.GetMentionedJID() {
		jid, err := waTypes.ParseJID(raw)
		if err != nil {
			continue
		}
		for _, needle := range []string{"@" + raw, "@" + jid.User} {
			if i := strings.Index(text, needle); i >= 0 {
				spots = append(spots, spot{i, i + len(needle), int64(jid.UserInt())})
				break
			}
		}
	}
	slices.SortFunc(spots, func(a, b spot) int { return a.at - b.at })
	var b strings.Builder
	prev := 0
	for _, s := range spots {
		if s.at < prev {
			continue
		}
		b.WriteString(text[prev:s.at])
		start := b.Len()
		b.WriteString("@" + m.displayName(s.fbid))
		built := b.String()
		off, n := proto.UTF16Range(built, start, len(built))
		out = append(out, mention{offset: off, length: n, fbid: s.fbid})
		prev = s.end
	}
	b.WriteString(text[prev:])
	return b.String(), out
}

// displayName is someone's name as written in text; m.mu is held.
func (m *Messenger) displayName(fbid int64) string {
	if p := m.people[fbid]; p != nil && p.name != "" {
		return p.name
	}
	if !m.replaying {
		m.tellUser(m.person(fbid), false) // asks who they are
	}
	return "someone"
}

// waMedia registers an encrypted attachment as a file, downloaded and
// decrypted only on request; m.mu is held.
func (m *Messenger) waMedia(t *waMediaTransport.WAMediaTransport, kind proto.MediaKind, mediaType whatsmeow.MediaType, name string, w, h, seconds int) proto.Media {
	integral, anc := t.GetIntegral(), t.GetAncillary()
	md := proto.Media{Kind: kind, Mime: anc.GetMimetype(), Size: int64(anc.GetFileLength()), Width: w, Height: h, Duration: seconds, Name: name}
	hash := integral.GetFileSHA256()
	if len(hash) == 0 {
		hash = integral.GetFileEncSHA256()
	}
	if len(hash) == 0 || integral.GetDirectPath() == "" {
		return md
	}
	key := "wa:" + hex.EncodeToString(hash)
	md.FileID = m.d.Files.Register(ids.FileRef{
		Network: net, Key: key, Size: md.Size, Mime: md.Mime, Name: name,
		Source: &waSource{Integral: integral, MediaType: mediaType},
	})
	switch {
	case (kind == proto.Photo || kind == proto.Sticker || kind == proto.GIF) && md.FileID != 0 && strings.HasPrefix(md.Mime, "image/"):
		md.Thumbnail = &proto.Image{FileID: md.FileID, Width: w, Height: h}
	case len(anc.GetThumbnail().GetJPEGThumbnail()) > 0:
		th := anc.GetThumbnail()
		id := m.d.Files.Register(ids.FileRef{
			Network: net, Key: key + ":thumb", Size: int64(len(th.GetJPEGThumbnail())), Mime: "image/jpeg", Name: "thumbnail.jpg",
			Source: &inlineSource{Data: th.GetJPEGThumbnail()},
		})
		if id != 0 {
			md.Thumbnail = &proto.Image{FileID: id, Width: int(th.GetThumbnailWidth()), Height: int(th.GetThumbnailHeight())}
		}
	}
	return md
}

// undecryptable shows a message that couldn't be decrypted yet; whatsmeow
// asks the sender for it again, and when it comes it takes this one's place.
func (m *Messenger) undecryptable(e *events.UndecryptableMessage) {
	if e.DecryptFailMode == events.DecryptFailHide {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.waChat(e.Info.Chat)
	if c == nil {
		return
	}
	sender := e.Info.Sender.ToNonAD()
	netID := waNetID(sender, e.Info.ID)
	if c.msgs[netID] != nil {
		return
	}
	fbid := int64(sender.UserInt())
	if e.Info.IsFromMe {
		fbid = m.self
	}
	msg := &message{
		netID: netID, ms: e.Info.Timestamp.UnixMilli(), sender: fbid,
		unsupported: "[Waiting for this message; it couldn't be decrypted yet]",
		wa:          &waRef{chat: e.Info.Chat, sender: sender, id: e.Info.ID},
	}
	m.arrived(c, msg, "")
}

// waReceipt handles read receipts in encrypted chats: someone read your
// messages, or you read a chat on another device.
func (m *Messenger) waReceipt(e *events.Receipt) {
	if e.Type != waTypes.ReceiptTypeRead && e.Type != waTypes.ReceiptTypeReadSelf {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key, err := strconv.ParseInt(e.Chat.User, 10, 64)
	if err != nil {
		return
	}
	c := m.lookupChat(key)
	if c == nil {
		return
	}
	var newest int64
	for _, id := range e.MessageIDs {
		for _, msg := range c.msgs {
			if msg.wa != nil && msg.wa.id == id {
				newest = max(newest, msg.ms)
			}
		}
	}
	if newest == 0 {
		return
	}
	if e.Type == waTypes.ReceiptTypeReadSelf || e.IsFromMe {
		m.readElsewhere(c.key, newest)
		return
	}
	m.theyRead(c.key, int64(e.Sender.UserInt()), newest)
}

// waTyping reports someone typing in an encrypted chat.
func (m *Messenger) waTyping(e *events.ChatPresence) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, err := strconv.ParseInt(e.Chat.User, 10, 64)
	if err != nil {
		return
	}
	c := m.lookupChat(key)
	who := int64(e.Sender.UserInt())
	if c == nil || who == m.self || who == 0 {
		return
	}
	p := m.person(who)
	m.tellUser(p, false)
	m.d.Events.Typing(c.id, p.id, e.State == waTypes.ChatPresenceComposing)
}

// waGroupInfo applies a change to an encrypted group: its name, who joined
// or left. These come as notifications, not messages, so they're shown as
// events of their own.
func (m *Messenger) waGroupInfo(e *events.GroupInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, err := strconv.ParseInt(e.JID.User, 10, 64)
	if err != nil {
		return
	}
	c := m.lookupChat(key)
	if c == nil {
		return
	}
	actor := ""
	if e.Sender != nil {
		actor = m.displayName(int64(e.Sender.UserInt()))
		if int64(e.Sender.UserInt()) == m.self {
			actor = "You"
		}
	}
	var sentences []string
	if e.Name != nil && e.Name.Name != c.name {
		c.name = e.Name.Name
		sentences = append(sentences, firstNonEmpty(actor, "Someone")+" named the group "+oneLine(e.Name.Name))
	}
	for _, j := range e.Join {
		fbid := int64(j.UserInt())
		if !slices.Contains(c.members, fbid) {
			c.members = append(c.members, fbid)
		}
		if actor != "" && (e.Sender == nil || e.Sender.User != j.User) {
			sentences = append(sentences, actor+" added "+m.displayName(fbid))
		} else {
			sentences = append(sentences, m.displayName(fbid)+" joined the group")
		}
	}
	for _, j := range e.Leave {
		fbid := int64(j.UserInt())
		c.members = slices.DeleteFunc(c.members, func(id int64) bool { return id == fbid })
		if fbid == m.self && (e.Sender == nil || int64(e.Sender.UserInt()) == m.self) {
			m.removeChat(c)
			return
		}
		if actor != "" && (e.Sender == nil || e.Sender.User != j.User) {
			sentences = append(sentences, actor+" removed "+m.displayName(fbid))
		} else {
			sentences = append(sentences, m.displayName(fbid)+" left the group")
		}
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = m.now()
	}
	for i, s := range sentences {
		sender := m.self
		if e.Sender != nil {
			sender = int64(e.Sender.UserInt())
		}
		msg := &message{
			netID:   "wa-event:" + strconv.FormatInt(ts.UnixMilli(), 10) + ":" + strconv.Itoa(i) + ":" + e.JID.User,
			ms:      ts.UnixMilli(),
			sender:  sender,
			service: s,
		}
		m.arrived(c, msg, "")
	}
	m.touch(c)
}
