// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
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
	reads, err := st.reads(ctx)
	if err != nil {
		hlog.Error("messenger: can't read how far encrypted chats were read", hlog.Kind(err))
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
	defer func() {
		// Set back however reading back ends, a panic included: what
		// arrives next is new and is reported.
		m.replaying = false
		m.dirty = nil
	}()
	rewritten := false
	for _, s := range kept {
		rewritten = m.replay(ctx, st, s) || rewritten
	}
	if rewritten {
		st.checkpoint(ctx)
	}
	// Messenger's thread rows say the chat was read only as far as it was
	// before any encrypted receipt: how far it was read here, or on another
	// device that told this one, was kept.
	for user, ms := range reads {
		key, err := strconv.ParseInt(user, 10, 64)
		if err != nil {
			continue
		}
		if c := m.lookupChat(key); c != nil && ms > c.readUpTo {
			c.readUpTo = ms
			m.recount(c)
			m.touch(c)
		}
	}
	for _, c := range m.chats {
		// Receipts went out for what was read before this run.
		c.receipted = max(c.receipted, c.readUpTo)
	}
	// Disappearing messages whose time ran out while tuimeta wasn't
	// running go before anything is shown, their downloads too.
	m.expire()
	m.flushDirty()
	m.watch(st)
	hlog.Info("messenger: encrypted messages read back", hlog.Int("count", int64(len(kept))))
}

// SweepEvery is how often disappearing messages whose time is up are looked
// for.
const SweepEvery = time.Minute

// watch runs the disappearing-message sweep for as long as st is the open
// store; m.mu is held.
func (m *Messenger) watch(st *e2eeStore) {
	life := m.life
	hlog.Go("messenger sweep", func() { m.sweepLoop(life, st) })
}

// sweepLoop deletes disappearing messages once their time is up, here as on
// the phone.
func (m *Messenger) sweepLoop(life context.Context, st *e2eeStore) {
	tick := time.NewTicker(SweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-life.Done():
			return
		}
		if !m.sweep(st) {
			return
		}
	}
}

// sweep deletes the messages whose time is up; false once st isn't the open
// store any more.
func (m *Messenger) sweep(st *e2eeStore) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store != st || m.closed {
		return false
	}
	m.expire()
	return true
}

// expire deletes the disappearing messages whose time is up: from their chat
// (reported, unless they're being read back), from the store, and what was
// downloaded of them; m.mu is held.
func (m *Messenger) expire() {
	now := m.now().UnixMilli()
	type gone struct {
		c     *chat
		netID string
	}
	var list []gone
	for _, c := range m.chats {
		for netID, msg := range c.msgs {
			if msg.expires > 0 && msg.expires <= now {
				list = append(list, gone{c, netID})
			}
		}
	}
	for _, g := range list {
		m.forgetFiles(g.c.msgs[g.netID])
		m.deleted(g.c, g.netID)
	}
	if m.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.store.deleteExpired(ctx, now); err != nil {
			hlog.Error("messenger: can't delete disappeared messages", hlog.Kind(err))
		}
	}
	if len(list) > 0 {
		hlog.Info("messenger: disappearing messages deleted", hlog.Int("count", int64(len(list))))
	}
}

// forgetFiles deletes what was downloaded of msg (its files, their stills,
// its link's picture), once msg is gone; m.mu is held.
func (m *Messenger) forgetFiles(msg *message) {
	if msg == nil || m.d.Downloads == nil {
		return
	}
	var files []int32
	for _, md := range msg.media {
		files = append(files, md.FileID)
		if md.Thumbnail != nil {
			files = append(files, md.Thumbnail.FileID)
		}
	}
	if msg.preview != nil && msg.preview.Image != nil {
		files = append(files, msg.preview.Image.FileID)
	}
	for _, id := range files {
		if ref, ok := m.d.Files.Get(id); ok {
			m.d.Downloads.Remove(ref)
		}
	}
}

// disappears is when a message with setting s goes: for one counted from
// when it was sent (at ms), a time in unix ms; for one counted from when
// it's seen, how many seconds it lasts then. View-once text ("seen once")
// lasts five minutes once seen, as mautrix-meta has it.
func disappears(s *waMsgApplication.MessageApplication_EphemeralSetting, ms int64) (expires, seenTimer int64) {
	secs := int64(s.GetEphemeralExpiration())
	switch s.GetEphemeralityType() {
	case waMsgApplication.MessageApplication_EphemeralSetting_SEEN_ONCE:
		return 0, 5 * 60
	case waMsgApplication.MessageApplication_EphemeralSetting_SEEN_BASED_WITH_TIMER:
		return 0, secs
	}
	if secs > 0 {
		return ms + secs*1000, 0
	}
	return 0, 0
}

// unixSeconds is a setting's time in unix seconds, as mautrix-meta reads
// it; one written in milliseconds is brought down to seconds, so it can't
// outrank every later setting.
func unixSeconds(t int64) int64 {
	if t > 1e11 {
		return t / 1000
	}
	return t
}

// setTimer records the chat's disappearing-messages setting s as of at
// (unix seconds): what you send carries it, as the official apps' messages
// do.
func (c *chat) setTimer(s *waMsgApplication.MessageApplication_EphemeralSetting, at int64) {
	c.timerAt = at
	c.timer = int64(s.GetEphemeralExpiration())
	if s.GetEphemeralityType() == waMsgApplication.MessageApplication_EphemeralSetting_SEEN_ONCE {
		c.timer = 5 * 60
	}
}

// startTimer starts the time of a message that disappears once seen, if it
// has been: yours at once, someone else's once the chat is read past it;
// m.mu is held.
func (m *Messenger) startTimer(c *chat, msg *message) {
	if msg.seenTimer == 0 || msg.expires != 0 {
		return
	}
	switch {
	case msg.sender == m.self:
		msg.expires = msg.ms + msg.seenTimer*1000
	case msg.ms <= c.readUpTo:
		msg.expires = m.now().UnixMilli() + msg.seenTimer*1000
	}
}

// startTimers starts the time of c's messages that disappear once seen and
// now have been, and keeps when they go; m.mu is held.
func (m *Messenger) startTimers(c *chat) {
	var ctx context.Context
	for _, msg := range c.msgs {
		if msg.seenTimer == 0 || msg.expires != 0 || msg.wa == nil {
			continue
		}
		m.startTimer(c, msg)
		if msg.expires == 0 || m.store == nil {
			continue
		}
		if ctx == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
		}
		if err := m.store.setExpires(ctx, msg.wa.chat.String(), msg.wa.sender.String(), msg.wa.id, msg.expires); err != nil {
			hlog.Error("messenger: can't keep when a message disappears", hlog.Kind(err))
		}
	}
}

// sendMeta is what a message sent in c says besides its content: the
// message it answers, if any, and the chat's disappearing-messages timer,
// which the official apps put on every message they send (leaving its kind
// unset), so what you send disappears as theirs does.
func (m *Messenger) sendMeta(c *chat, replied *message) *waMsgApplication.MessageApplication_Metadata {
	meta := &waMsgApplication.MessageApplication_Metadata{}
	if replied != nil && replied.wa != nil {
		meta.QuotedMessage = &waMsgApplication.MessageApplication_Metadata_QuotedMessage{
			StanzaID: gproto.String(replied.wa.id), Participant: gproto.String(replied.wa.sender.String()),
		}
	}
	m.mu.Lock()
	timer, at := c.timer, c.timerAt
	m.mu.Unlock()
	if timer > 0 && timer <= math.MaxUint32 {
		setting := &waMsgApplication.MessageApplication_EphemeralSetting{
			EphemeralExpiration: gproto.Uint32(uint32(timer)),
			EphemeralityType:    waMsgApplication.MessageApplication_EphemeralSetting_UNKNOWN.Enum(),
		}
		if at > 0 {
			setting.EphemeralSettingTimestamp = gproto.Int64(at)
		}
		meta.Ephemeral = &waMsgApplication.MessageApplication_Metadata_ChatEphemeralSetting{ChatEphemeralSetting: setting}
	}
	return meta
}

// replay applies a kept row again, and brings the row up to date with what
// applying it says now: an edit or a reaction an older build kept as a
// message is kept as one (its message's earlier ones go), a row that
// changes or shows nothing (an edit of nothing, a reaction that's no emoji,
// an address that names no chat) is dropped, and what an older build kept
// of a view-once message or a quote is taken out. It says whether the store
// changed; m.mu is held.
func (m *Messenger) replay(ctx context.Context, st *e2eeStore, s storedMessage) bool {
	evt := decodeStored(s)
	if evt == nil {
		return false
	}
	r := m.applyWA(evt)
	if !r.keep {
		if err := st.drop(ctx, s.Chat, s.Sender, s.ID); err != nil {
			hlog.Error("messenger: can't drop a kept message", hlog.Kind(err))
			return false
		}
		return true
	}
	want := s
	want.Kind = r.kind
	if r.target != nil {
		want.TargetSender, want.TargetID = r.target.sender.String(), r.target.id
	}
	if r.msg != nil {
		// When it goes was kept once its time started (read, for one that
		// goes once seen); one read since starts now.
		if s.Expires > 0 {
			r.msg.expires = s.Expires
		}
		want.Expires = r.msg.expires
	}
	kept, stripped := keptApplication(evt.FBApplication)
	if stripped {
		app, err := gproto.Marshal(kept)
		if err != nil {
			return false
		}
		want.App = app
	}
	if !stripped && want.Kind == s.Kind && want.TargetSender == s.TargetSender && want.TargetID == s.TargetID && want.Expires == s.Expires {
		return false
	}
	if err := st.put(ctx, want); err != nil {
		hlog.Error("messenger: can't rewrite a kept message", hlog.Kind(err))
		return false
	}
	return true
}

// forgetKept deletes what the store kept of a chat that left the list
// (deleted, a message request deleted, a group left), as the WhatsApp
// backend does: else the next start would read it back as a chat, and a
// deleted request as an ordinary one. m.mu is held.
func (m *Messenger) forgetKept(c *chat) {
	if m.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.store.forgetChat(ctx, c.netID()); err != nil {
		hlog.Error("messenger: can't forget a removed chat's messages", hlog.Kind(err))
	}
}

// keptApplication is app as the store keeps it: a view-once photo or video
// without what it could be fetched with (it's shown only on the phone, so
// tuimeta keeps nothing that could fetch it), and a quote's copy of the
// message it answers only if that's text, all a reply shows of it.
// stripped says whether anything was taken out; app itself is left as it
// is.
func keptApplication(app *waMsgApplication.MessageApplication) (kept *waMsgApplication.MessageApplication, stripped bool) {
	kept = app
	own := func() *waMsgApplication.MessageApplication {
		if !stripped {
			kept = gproto.Clone(app).(*waMsgApplication.MessageApplication)
			stripped = true
		}
		return kept
	}
	switch sub := app.GetPayload().GetSubProtocol().GetSubProtocol().(type) {
	case *waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage:
		if cm, err := sub.Decode(); err == nil && bareViewOnce(cm) {
			bare := &waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage{}
			if bare.Set(cm) == nil {
				own().GetPayload().GetSubProtocol().SubProtocol = bare
			}
		}
	case *waMsgApplication.MessageApplication_SubProtocolPayload_Armadillo:
		if a, err := sub.Decode(); err == nil && bareRaven(a) {
			bare := &waMsgApplication.MessageApplication_SubProtocolPayload_Armadillo{}
			if bare.Set(a) == nil {
				own().GetPayload().GetSubProtocol().SubProtocol = bare
			}
		}
	}
	if q := app.GetMetadata().GetQuotedMessage(); q.GetPayload() != nil && !textMessage(q.GetPayload()) {
		own().GetMetadata().GetQuotedMessage().Payload = nil
	}
	return kept, stripped
}

// bareViewOnce takes out of a view-once photo or video in cm what it could
// be fetched with (and its caption, never shown); false if there was
// nothing to take. What kind it was stays.
func bareViewOnce(cm *waConsumerApplication.ConsumerApplication) bool {
	switch c := cm.GetPayload().GetContent().GetViewOnceMessage().GetViewOnceContent().(type) {
	case *waConsumerApplication.ConsumerApplication_ViewOnceMessage_ImageMessage:
		if c.ImageMessage.GetImage() == nil && c.ImageMessage.GetCaption() == nil {
			return false
		}
		c.ImageMessage = &waConsumerApplication.ConsumerApplication_ImageMessage{}
	case *waConsumerApplication.ConsumerApplication_ViewOnceMessage_VideoMessage:
		if c.VideoMessage.GetVideo() == nil && c.VideoMessage.GetCaption() == nil {
			return false
		}
		c.VideoMessage = &waConsumerApplication.ConsumerApplication_VideoMessage{}
	default:
		return false
	}
	return true
}

// bareRaven does what bareViewOnce does for Messenger's own view-once
// messages.
func bareRaven(a *waArmadilloApplication.Armadillo) bool {
	var rm *waArmadilloApplication.Armadillo_Content_RavenMessage
	switch c := a.GetPayload().GetContent().GetContent().(type) {
	case *waArmadilloApplication.Armadillo_Content_RavenMessage_:
		rm = c.RavenMessage
	case *waArmadilloApplication.Armadillo_Content_RavenMessageMsgr:
		rm = c.RavenMessageMsgr
	default:
		return false
	}
	switch mc := rm.GetMediaContent().(type) {
	case *waArmadilloApplication.Armadillo_Content_RavenMessage_ImageMessage:
		if len(mc.ImageMessage.GetPayload()) == 0 {
			return false
		}
		mc.ImageMessage = &waCommon.SubProtocol{}
	case *waArmadilloApplication.Armadillo_Content_RavenMessage_VideoMessage:
		if len(mc.VideoMessage.GetPayload()) == 0 {
			return false
		}
		mc.VideoMessage = &waCommon.SubProtocol{}
	default:
		return false
	}
	return true
}

// textMessage reports whether a quote's copy of a message is a text message.
func textMessage(p *waMsgApplication.MessageApplication_Payload) bool {
	sub, ok := p.GetSubProtocol().GetSubProtocol().(*waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage)
	if !ok {
		return false
	}
	cm, err := sub.Decode()
	if err != nil {
		return false
	}
	_, text := cm.GetPayload().GetContent().GetContent().(*waConsumerApplication.ConsumerApplication_Content_MessageText)
	return text
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
		if app.GetMetadata().GetChatEphemeralSetting() == nil {
			return nil
		}
		sub = nil // a chat's disappearing-messages setting has no message
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
	cli, as, st, life, self := m.msgx, m.as, m.store, m.life, m.self
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
		registered := ""
		if dev.ID != nil {
			registered = dev.ID.String()
		}
		if !m.keepDevice(gen, registered, cookieValues(cli)) {
			return // logged out (or replaced) as it registered
		}
		hlog.Info("messenger: registered an encrypted-chat device")
	}
	wa, err := cli.PrepareE2EEClient()
	if err != nil {
		fail("prepare", err)
		return
	}
	configureE2EE(wa)
	presentE2EE(wa, as)
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

// keepDevice records the encrypted-chat device connection gen registered,
// with the cookies as they are now, and saves the session; it reports
// whether gen is still the connection. As in keepSession, it's saved under
// the lock and only then, so a logout can't wipe the folder in between and
// find the session back at the next start.
func (m *Messenger) keepDevice(gen int, device string, values map[string]string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != gen || m.closed {
		return false
	}
	if m.sess == nil {
		return true
	}
	if device != "" {
		m.sess.WADevice = device
		m.sess.Cookies = values
	}
	if err := m.d.Session.SaveJSON(sessionFile, *m.sess); err != nil {
		hlog.Error("messenger: can't save session", hlog.Kind(err))
	}
	return true
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
// store before whatsmeow acknowledges it (SynchronousAck). Both happen under
// m.mu, so a chat removed in between can't have the message kept after what
// was kept of it was forgotten, and a panic while applying (recovered in
// onE2EE) leaves the lock free.
func (m *Messenger) receiveWA(evt *events.FBMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.applyWA(evt)
	st := m.store
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	switch {
	case r.revoked != nil:
		err = st.remove(ctx, r.revoked.chat.String(), r.revoked.sender.String(), r.revoked.id)
	case r.keep:
		err = keepWA(ctx, st, evt, r)
	}
	if err != nil {
		hlog.Error("messenger: can't keep an encrypted message", hlog.Kind(err))
	}
}

// ownChange is how a message sent from here is kept: as a message (going
// when the chat's timer says), or as an edit or a reaction of the kept
// message it names. ok is false when there's nothing to keep: the chat or
// that message isn't here, or the reaction is no emoji; m.mu is held.
func (m *Messenger) ownChange(evt *events.FBMessage) (r waApplied, ok bool) {
	key, ok := waChatKey(evt.Info.Chat)
	if !ok {
		return waApplied{}, false
	}
	c := m.lookupChat(key)
	if c == nil {
		return waApplied{}, false
	}
	sender := evt.Info.Sender.ToNonAD()
	consumer, _ := evt.Message.(*waConsumerApplication.ConsumerApplication)
	var mk *waCommon.MessageKey
	switch ct := consumer.GetPayload().GetContent().GetContent().(type) {
	case nil:
		return waApplied{}, false
	case *waConsumerApplication.ConsumerApplication_Content_EditMessage:
		r.kind, mk = rowEdit, ct.EditMessage.GetKey()
	case *waConsumerApplication.ConsumerApplication_Content_ReactionMessage:
		if !reactionLike(ct.ReactionMessage.GetText()) {
			return waApplied{}, false
		}
		r.kind, mk = rowReaction, ct.ReactionMessage.GetKey()
	default:
		return waApplied{keep: true, kind: rowMessage, msg: c.msgs[waNetID(sender, evt.Info.ID)]}, true
	}
	msg := c.msgs[m.waTarget(c, evt.Info.Chat, sender, mk)]
	if msg == nil || msg.wa == nil {
		return waApplied{}, false
	}
	r.keep, r.target = true, msg.wa
	return r, true
}

// keepWA stores an encrypted message (or an edit or reaction of one) as
// applying it said: as its kind, with the message it changed.
func keepWA(ctx context.Context, st *e2eeStore, evt *events.FBMessage, r waApplied) error {
	if evt.FBApplication == nil {
		return nil
	}
	kept, _ := keptApplication(evt.FBApplication)
	app, err := gproto.Marshal(kept)
	if err != nil {
		return err
	}
	row := storedMessage{
		Chat: evt.Info.Chat.String(), Sender: evt.Info.Sender.ToNonAD().String(), ID: evt.Info.ID,
		TS: evt.Info.Timestamp, FromMe: evt.Info.IsFromMe, App: app, Kind: r.kind,
	}
	if r.target != nil {
		row.TargetSender, row.TargetID = r.target.sender.String(), r.target.id
	}
	if r.msg != nil {
		row.Expires = r.msg.expires
	}
	if err := st.put(ctx, row); err != nil {
		return err
	}
	if r.kind != rowMessage {
		return nil // only messages count toward the cap
	}
	return st.pruneChat(ctx, row.Chat)
}

// waChatKey is the key of the chat an encrypted chat's JID names: a
// person's (on Messenger's server or WhatsApp's) or a group's. Any other
// address (a broadcast list, a channel, a bot) names no chat here, and so
// is never one sent to.
func waChatKey(jid waTypes.JID) (int64, bool) {
	switch jid.Server {
	case waTypes.MessengerServer, waTypes.DefaultUserServer, waTypes.GroupServer:
	default:
		return 0, false
	}
	key, err := strconv.ParseInt(jid.User, 10, 64)
	if err != nil || key <= 0 {
		return 0, false
	}
	return key, true
}

// waChat is the chat an encrypted message from sender is in, or nil when its
// address names none. Someone else's message in a one-to-one chat comes from
// the other person, so it can't be filed under another chat (or change
// where that chat's messages go); m.mu is held.
func (m *Messenger) waChat(jid, sender waTypes.JID, fromMe bool) *chat {
	key, ok := waChatKey(jid)
	if !ok {
		return nil
	}
	if !fromMe && int64(sender.UserInt()) != m.self {
		if sender.UserInt() == 0 || (jid.Server != waTypes.GroupServer && sender.User != jid.User) {
			return nil
		}
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

// waApplied is what applying an encrypted message did.
type waApplied struct {
	// keep says the message (or the edit or reaction) is to be kept in the
	// store, as kind; msg is the message, when it's one.
	keep bool
	kind string
	msg  *message
	// target is the kept message an edit or a reaction changed. One that
	// changed nothing isn't kept: it would only take room.
	target *waRef
	// revoked is the kept message an unsend removed.
	revoked *waRef
}

// applyWA applies an encrypted message (or an edit, reaction or unsend of
// one); m.mu is held. It says whether the message should be kept, or which
// kept message was unsent.
func (m *Messenger) applyWA(evt *events.FBMessage) waApplied {
	info := evt.Info
	sender := info.Sender.ToNonAD()
	c := m.waChat(info.Chat, sender, info.IsFromMe)
	if c == nil {
		return waApplied{}
	}
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
				msg := c.msgs[m.waTarget(c, info.Chat, sender, content.EditMessage.GetKey())]
				if !m.waEdit(c, msg, fbid, content.EditMessage) {
					return waApplied{kind: rowEdit}
				}
				return waApplied{keep: true, kind: rowEdit, target: msg.wa}
			case *waConsumerApplication.ConsumerApplication_Content_ReactionMessage:
				msg := c.msgs[m.waTarget(c, info.Chat, sender, content.ReactionMessage.GetKey())]
				emoji := content.ReactionMessage.GetText()
				// Only an emoji is a reaction: words, a count or a time put
				// there would read as part of the message.
				if msg == nil || msg.wa == nil || !reactionLike(emoji) || !msg.setReaction(fbid, emoji) {
					return waApplied{kind: rowReaction}
				}
				m.changed(c, msg)
				return waApplied{keep: true, kind: rowReaction, target: msg.wa}
			case *waConsumerApplication.ConsumerApplication_Content_PollUpdateMessage:
				return waApplied{} // a vote; polls aren't shown
			}
			msg := m.newWA(c, evt, sender, fbid)
			m.consumerContent(msg, p.Content)
			return m.waArrived(c, msg)
		case *waConsumerApplication.ConsumerApplication_Payload_ApplicationData:
			if rv := p.ApplicationData.GetRevoke(); rv != nil {
				target := m.waTarget(c, info.Chat, sender, rv.GetKey())
				msg := c.msgs[target]
				if msg == nil || msg.sender != fbid {
					return waApplied{} // only a message's sender unsends it
				}
				ref := msg.wa
				m.forgetFiles(msg)
				m.deleted(c, target)
				return waApplied{revoked: ref}
			}
			return waApplied{}
		}
		return waApplied{}
	case *waArmadilloApplication.Armadillo:
		msg := m.newWA(c, evt, sender, fbid)
		m.armadilloContent(c, msg, typed.GetPayload().GetContent())
		return m.waArrived(c, msg)
	}
	if s := evt.FBApplication.GetMetadata().GetChatEphemeralSetting(); evt.Message == nil && s != nil {
		// A disappearing-messages setting, not a message: kept while it's
		// the chat's newest, so what you send carries it after a restart.
		at := info.Timestamp.Unix()
		if at < c.timerAt {
			return waApplied{}
		}
		c.setTimer(s, at)
		return waApplied{keep: true, kind: rowSetting}
	}
	msg := m.newWA(c, evt, sender, fbid)
	msg.unsupported = "[Unsupported message]"
	m.waArrived(c, msg)
	return waApplied{}
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
	if s := meta.GetChatEphemeralSetting(); s != nil {
		// The sender's timer: the message disappears when it says, here as
		// on their phone. A newer setting than the chat's is the chat's now.
		msg.expires, msg.seenTimer = disappears(s, msg.ms)
		if at := unixSeconds(s.GetEphemeralSettingTimestamp()); at > c.timerAt {
			c.setTimer(s, at)
		}
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
		return ref
	}
	// Not a message kept here: who said it is only the sender's word, so it
	// isn't put in anyone's mouth (nor is the person named looked up), as
	// the WhatsApp backend does. What it says shows, unattributed.
	ref.sender = 0
	if t := quotedText(q.GetPayload()); t != "" {
		ref.text = snippet(t)
	}
	return ref
}

// quotedText is the text of a quote's copy of the message it answers, if
// that's a text message.
func quotedText(p *waMsgApplication.MessageApplication_Payload) string {
	sub, ok := p.GetSubProtocol().GetSubProtocol().(*waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage)
	if !ok {
		return ""
	}
	cm, err := sub.Decode()
	if err != nil {
		return ""
	}
	return cm.GetPayload().GetContent().GetMessageText().GetText()
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

// waArrived records an encrypted message, unless one with its id is here
// already; m.mu is held.
func (m *Messenger) waArrived(c *chat, msg *message) waApplied {
	if old := c.msgs[msg.netID]; old != nil {
		if !old.placeholder {
			// A message is replaced only by its decryption, once that
			// comes: its sender's message again under the same id (which
			// would rewrite it unmarked, here and in the store) is dropped,
			// as in the WhatsApp backend. Changes go through edits, which
			// say so.
			return waApplied{}
		}
		if len(msg.reactions) == 0 {
			msg.reactions = old.reactions
		}
	}
	m.startTimer(c, msg)
	if !m.replaying && msg.expires > 0 && msg.expires <= m.now().UnixMilli() {
		// It has disappeared already. (One read back goes once all are,
		// with what was downloaded of it.)
		return waApplied{}
	}
	if m.replaying {
		m.keep(c, msg)
		c.activity = max(c.activity, msg.ms)
		m.recount(c)
		m.touch(c)
		return waApplied{keep: true, msg: msg}
	}
	m.arrived(c, msg, "")
	return waApplied{keep: true, msg: msg}
}

// waEdit replaces the text of an encrypted message, and says whether it
// did: only its sender may, and an older edit never undoes a newer one;
// m.mu is held.
func (m *Messenger) waEdit(c *chat, msg *message, fbid int64, e *waConsumerApplication.ConsumerApplication_EditMessage) bool {
	if msg == nil || msg.wa == nil || msg.sender != fbid || e.GetTimestampMS() <= msg.editTS {
		return false
	}
	msg.editTS = e.GetTimestampMS()
	msg.text, msg.mentions = m.waText(e.GetMessage())
	msg.edited = true
	m.changed(c, msg)
	return true
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
				if len(msg.media) == maxParts {
					break // the rest wouldn't be shown, so they get no files
				}
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
	msg.mentions = waMentions(x.GetMentions())
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

// waMentions are the mentions a sender listed: each once, at most
// MaxMentions.
func waMentions(list []*waCommon.Mention) []mention {
	var set mentionSet
	for _, mn := range list {
		jid, err := waTypes.ParseJID(mn.GetMentionedJID())
		if err != nil {
			continue
		}
		if !set.add(mention{offset: int(mn.GetOffset()), length: int(mn.GetLength()), fbid: int64(jid.UserInt())}) {
			break
		}
	}
	return set.list
}

// maxNamedJID is the longest mention written out in full ("@<id>@msgr")
// looked for in a text; longer ones are looked for as "@<id>" only, so
// finding them stays linear in the text.
const maxNamedJID = 64

// waText is an encrypted message's text and the mentions in it, each once
// and at most MaxMentions. Mentions written as "@<id>@msgr" (or "@<id>") in
// the text are replaced by "@" and the person's name, as the connector shows
// them, found in one pass over the text; m.mu is held.
func (m *Messenger) waText(t *waCommon.MessageText) (string, []mention) {
	text := t.GetText()
	if text == "" {
		return "", nil
	}
	if mentions := t.GetMentions(); len(mentions) > 0 {
		return text, waMentions(mentions)
	}
	type person struct {
		raw  string // the JID as the sender wrote it
		fbid int64
	}
	named := map[string]person{} // by the JID's user part, all digits
	for _, raw := range t.GetMentionedJID() {
		if len(named) == MaxMentions {
			break
		}
		jid, err := waTypes.ParseJID(raw)
		if err != nil || jid.UserInt() == 0 {
			continue
		}
		if _, ok := named[jid.User]; !ok {
			if len(raw) > maxNamedJID {
				raw = ""
			}
			named[jid.User] = person{raw, int64(jid.UserInt())}
		}
	}
	if len(named) == 0 {
		return text, nil
	}
	// Where each is first written, in full or as "@<id>": an "@" is
	// followed by at most one id, its digits.
	type spot struct {
		at, end int
		fbid    int64
	}
	full, short := map[string]spot{}, map[string]spot{}
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		j := i + 1
		for j < len(text) && text[j] >= '0' && text[j] <= '9' {
			j++
		}
		user := text[i+1 : j]
		if p, ok := named[user]; ok {
			if _, seen := full[user]; !seen && p.raw != "" && strings.HasPrefix(text[i+1:], p.raw) {
				full[user] = spot{i, i + 1 + len(p.raw), p.fbid}
			}
			if _, seen := short[user]; !seen {
				short[user] = spot{i, j, p.fbid}
			}
		}
		i = j - 1
	}
	spots := make([]spot, 0, len(named))
	for user := range named {
		if s, ok := full[user]; ok {
			spots = append(spots, s)
		} else if s, ok := short[user]; ok {
			spots = append(spots, s)
		}
	}
	slices.SortFunc(spots, func(a, b spot) int { return a.at - b.at })
	var b strings.Builder
	var out []mention
	prev, units := 0, 0 // units: the UTF-16 length of what's built
	for _, s := range spots {
		if s.at < prev {
			continue
		}
		before := text[prev:s.at]
		b.WriteString(before)
		units += proto.UTF16Len(before)
		name := "@" + m.displayName(s.fbid)
		b.WriteString(name)
		n := proto.UTF16Len(name)
		out = append(out, mention{offset: units, length: n, fbid: s.fbid})
		units += n
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
	// The file is named by its hash and its media key together, as the
	// WhatsApp backend's mediaRefs does: the hash alone is the sender's to
	// state, so another sender stating someone else's file's hash could
	// otherwise take over its entry (its name, where it's fetched from, its
	// still), while the media key is held only by the file's own messages.
	// Without the hash whatsmeow can't check what it downloads, and refuses.
	hash, mediaKey := integral.GetFileSHA256(), integral.GetMediaKey()
	if len(hash) == 0 || len(mediaKey) == 0 || integral.GetDirectPath() == "" {
		return md
	}
	named := sha256.New()
	named.Write(hash)
	named.Write(mediaKey)
	key := "wa:" + hex.EncodeToString(named.Sum(nil))
	md.FileID = m.d.Files.Register(ids.FileRef{
		Network: net, Key: key, Size: md.Size, Mime: md.Mime, Name: name,
		Source: &waSource{Integral: integral, MediaType: mediaType, Kind: kind},
	})
	switch {
	case (kind == proto.Photo || kind == proto.Sticker || kind == proto.GIF) && md.FileID != 0 && strings.HasPrefix(md.Mime, "image/"):
		md.Thumbnail = &proto.Image{FileID: md.FileID, Width: w, Height: h}
	case len(anc.GetThumbnail().GetJPEGThumbnail()) > 0:
		// A still the message carries is named by its own bytes: they're
		// what's shown, whatever the file's entry says.
		th := anc.GetThumbnail()
		id := m.d.Files.Register(ids.FileRef{
			Network: net, Key: "wa:thumb:" + contentKey(th.GetJPEGThumbnail()), Size: int64(len(th.GetJPEGThumbnail())), Mime: "image/jpeg", Name: "thumbnail.jpg",
			Source: &inlineSource{Data: th.GetJPEGThumbnail()},
		})
		if id != 0 {
			md.Thumbnail = &proto.Image{FileID: id, Width: int(th.GetThumbnailWidth()), Height: int(th.GetThumbnailHeight())}
		}
	}
	return md
}

// contentKey names bytes a message carries by their hash.
func contentKey(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// undecryptable shows a message that couldn't be decrypted yet; whatsmeow
// asks the sender for it again, and when it comes it takes this one's place.
func (m *Messenger) undecryptable(e *events.UndecryptableMessage) {
	if e.DecryptFailMode == events.DecryptFailHide {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sender := e.Info.Sender.ToNonAD()
	c := m.waChat(e.Info.Chat, sender, e.Info.IsFromMe)
	if c == nil {
		return
	}
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
		placeholder: true,
	}
	m.arrived(c, msg, "")
}

// markReadWA sends the receipt for what you read of sender's encrypted
// messages, for the request purpose: one they see, or, where your read
// receipts are off, one only your own devices act on (whatsmeow's
// "read-self").
func markReadWA(ctx context.Context, e2ee e2eeAPI, purpose string, off bool, ids []waTypes.MessageID, at time.Time, chat, sender waTypes.JID) error {
	if off {
		return e2ee.MarkReadSelf(ctx, purpose, ids, at, chat, sender)
	}
	return e2ee.MarkRead(ctx, purpose, ids, at, chat, sender)
}

// waReceipt handles read receipts in encrypted chats: someone read your
// messages, or you read a chat on another device.
func (m *Messenger) waReceipt(e *events.Receipt) {
	if e.Type != waTypes.ReceiptTypeRead && e.Type != waTypes.ReceiptTypeReadSelf {
		return
	}
	key, ok := waChatKey(e.Chat)
	if !ok {
		return
	}
	// The ids once each, before the lock: a receipt may name thousands.
	named := make(map[waTypes.MessageID]bool, len(e.MessageIDs))
	for _, id := range e.MessageIDs {
		named[id] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.lookupChat(key)
	if c == nil {
		return
	}
	// whatsmeow leaves IsFromMe unset on the users a grouped receipt lists,
	// so your own account's id counts too.
	fromMe := e.IsFromMe || int64(e.Sender.UserInt()) == m.self
	if !fromMe && e.Type == waTypes.ReceiptTypeReadSelf {
		return // someone else's "read-self" says nothing about what you read
	}
	// Whose messages the ids can name: someone else reads yours; you read
	// another person's (the one the receipt says, else the chat's other
	// person or, in a group, anyone's).
	from := func(msg *message) bool { return msg.sender == m.self }
	if fromMe {
		switch {
		case !e.MessageSender.IsEmpty():
			who := int64(e.MessageSender.UserInt())
			from = func(msg *message) bool { return msg.sender == who && who != m.self }
		case c.kind == proto.DM:
			from = func(msg *message) bool { return msg.sender == c.other && c.other != m.self }
		default:
			from = func(msg *message) bool { return msg.sender != m.self }
		}
	}
	var newest int64
	for id := range named {
		for _, netID := range c.waIDs[id] {
			if msg := c.msgs[netID]; msg != nil && from(msg) {
				newest = max(newest, msg.ms)
			}
		}
	}
	if newest == 0 {
		return
	}
	if fromMe {
		m.readElsewhere(c.key, newest)
		return
	}
	m.theyRead(c.key, int64(e.Sender.UserInt()), newest)
}

// waTyping reports someone typing in an encrypted chat.
func (m *Messenger) waTyping(e *events.ChatPresence) {
	key, ok := waChatKey(e.Chat)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
	key, ok := waChatKey(e.JID)
	if !ok || e.JID.Server != waTypes.GroupServer {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
			m.forgetKept(c)
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
