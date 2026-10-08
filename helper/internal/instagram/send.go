// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"cmp"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/mautrix-meta/pkg/instameow"
	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
	"go.mau.fi/mautrix-meta/pkg/messagix/methods"
	"go.mau.fi/util/jsontime"
	"go.mau.fi/util/variationselector"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Instagram's limits (the connector's capabilities).
const (
	maxTextLength = 1000 // UTF-16 code units
	maxImageSize  = 8 * 1000 * 1000
	maxFileSize   = 25 * 1000 * 1000
)

// sendAttribution is what the web client says a message was sent from.
const sendAttribution = "igd_web_chat_tab:in_thread"

// sendState is a message being sent. Each file is its own Instagram message
// (Instagram has no albums to send, nor captions: the text follows the
// files), and each may come back on the socket before its request answers:
// got keeps those, by offline threading id, for Send to report.
type sendState struct {
	chat *chat
	got  map[string]*netMsg
}

func newOTID() string { return strconv.FormatInt(methods.GenerateEpochID(), 10) }

// message is the chat and message a request names; b.mu is held.
func (b *Instagram) message(ref backend.MessageRef) (*chat, *netMsg, error) {
	c, err := b.chatOf(ref.Chat)
	if err != nil {
		return nil, nil, err
	}
	n := c.msgs[ref.NetID]
	if n == nil {
		return nil, nil, proto.ErrNoMessage
	}
	return c, n, nil
}

// checkUpload refuses what Instagram won't take, before anything is sent.
func checkUpload(u backend.Upload) error {
	size := len(u.Data)
	switch u.Kind {
	case proto.FileMedia:
		return proto.Err(proto.Unsupported, "Instagram only sends photos, videos and audio, not other files.")
	case proto.Photo, proto.GIF, proto.Sticker:
		if size > maxImageSize {
			return proto.Err(proto.Unsupported, "Instagram takes pictures up to 8 MB; this one is bigger.")
		}
	default:
		if size > maxFileSize {
			return proto.Err(proto.Unsupported, "Instagram takes videos and audio up to 25 MB; this file is bigger.")
		}
	}
	return nil
}

// uploadName is a file's name made safe for the upload form's header.
func uploadName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return cmp.Or(name, "file")
}

// Send sends the files, one Instagram message each, then the text. Replying
// in a message request accepts it first, as Instagram requires before you
// can answer.
func (b *Instagram) Send(ctx context.Context, out *backend.Outgoing) error {
	conn, err := b.current()
	if err != nil {
		return err
	}
	for _, f := range out.Files {
		if err := checkUpload(f); err != nil {
			return err
		}
	}
	if proto.UTF16Len(out.Text) > maxTextLength {
		return proto.Err(proto.Unsupported, "Instagram messages can be at most 1000 characters long.")
	}
	b.mu.Lock()
	c, err := b.chatOf(out.Chat)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	igid, key, request := c.igid, c.key, c.request
	reply := ""
	if out.ReplyTo != nil {
		reply = out.ReplyTo.NetID
	}
	st := &sendState{chat: c, got: map[string]*netMsg{}}
	b.mu.Unlock()
	if igid == "" && len(out.Files) > 0 {
		return proto.Err(proto.Unsupported, "Send a text message first: Instagram starts a new chat with text.")
	}
	if request && igid != "" {
		_, err := conn.cli.AcceptMessageRequest(withQuietLog(ctx), &slidetypes.AcceptMessageRequestRequest{ThreadID: igid, OfflineThreadingID: newOTID()})
		if err != nil {
			return requestError(err, "Instagram didn't let you answer this message request; accept it in the app first.")
		}
		b.mu.Lock()
		c.request = false
		b.mu.Unlock()
	}

	var sent []*netMsg
	var otids []string
	defer func() {
		b.mu.Lock()
		for _, otid := range otids {
			delete(b.sending, otid)
		}
		b.mu.Unlock()
	}()
	track := func() string {
		otid := newOTID()
		otids = append(otids, otid)
		b.mu.Lock()
		b.sending[otid] = st
		b.mu.Unlock()
		return otid
	}

	for i, f := range out.Files {
		otid := track()
		n, err := b.sendFile(ctx, conn, st, f, igid, key, otid, reply, i == 0)
		if err != nil {
			return b.sentSome(out, sent, err)
		}
		sent = append(sent, n)
		reply = "" // only the first answers
	}
	if out.Text != "" {
		otid := track()
		req := &slidetypes.SendTextRequest{
			OfflineThreadingID: otid,
			Text:               slidetypes.SensitiveString{Value: out.Text},
			SendAttribution:    new(sendAttribution),
		}
		if igid != "" {
			req.IGThreadIGID = &igid
		} else {
			// A dm Instagram has no thread for yet names its person, as
			// the connector does.
			req.RecipientIGIDs = []string{strconv.FormatInt(key, 10)}
		}
		if reply != "" {
			req.ReplyToMessageID = &reply
		}
		resp, err := conn.cli.SendMessage(withQuietLog(ctx), req)
		if err != nil {
			if n := b.arrived(st, otid); n != nil {
				// The answer was lost, but the socket brought the message.
				return b.done(out, append(sent, n))
			}
			return b.sentSome(out, sent, requestError(err, "Instagram didn't take the message; try again."))
		}
		n := b.confirmed(st, otid, resp.GetMessage(), func(n *netMsg) {
			n.text, n.plainText = out.Text, true
			if reply != "" {
				n.replyNet = reply
			}
		})
		if n == nil {
			return b.sentSome(out, sent, proto.Err(proto.NetworkError, "Instagram didn't confirm the message; it may not have been sent."))
		}
		sent = append(sent, n)
	}
	return b.done(out, sent)
}

// done reports a send that went through.
func (b *Instagram) done(out *backend.Outgoing, sent []*netMsg) error {
	b.report(out, sent)
	return nil
}

// arrived is the socket's copy of a message being sent, if it came.
func (b *Instagram) arrived(st *sendState, otid string) *netMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return st.got[otid]
}

// sendFile uploads one file and sends it as a message.
func (b *Instagram) sendFile(ctx context.Context, conn *connection, st *sendState, f backend.Upload, igid string, key int64, otid, reply string, first bool) (*netMsg, error) {
	attachment, err := conn.cli.Upload(withQuietLog(ctx), key, uploadName(f.Name), f.Mime, f.Data, f.Kind == proto.Voice)
	if err != nil || attachment == 0 {
		hlog.Warn("instagram: upload failed", hlog.Kind(err))
		return nil, requestError(cmp.Or(err, errors.New("no attachment id")), "Instagram didn't take the file; try again.")
	}
	req := &slidetypes.SendMediaRequest{AttachmentFBID: strconv.FormatInt(attachment, 10), ThreadID: igid, OfflineThreadingID: otid}
	if first && reply != "" {
		req.ReplyToMessageID = &reply
	}
	resp, err := conn.cli.SendMedia(withQuietLog(ctx), req)
	if err != nil {
		if n := b.arrived(st, otid); n != nil {
			return n, nil // the answer was lost, but the socket brought it
		}
		return nil, requestError(err, "Instagram didn't take the file; try again.")
	}
	n := b.confirmed(st, otid, resp.GetMessage(), func(n *netMsg) {
		// The file as sent, until the socket brings Instagram's copy.
		n.media = []proto.Media{{Kind: f.Kind, Name: f.Name, Mime: f.Mime, Size: int64(len(f.Data)), Width: f.Width, Height: f.Height}}
		if first && reply != "" {
			n.replyNet = reply
		}
	})
	if n == nil {
		return nil, proto.Err(proto.NetworkError, "Instagram didn't confirm the file; it may not have been sent.")
	}
	return n, nil
}

// confirmed is the message Instagram accepted: the socket's copy if it came
// first, else one made from what was sent (fill) and the answer.
func (b *Instagram) confirmed(st *sendState, otid string, sm slidetypes.SentMessage, fill func(*netMsg)) *netMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := st.got[otid]; n != nil {
		return n
	}
	n := &netMsg{
		netID:  cmp.Or(sm.ID, sm.MessageID),
		otid:   otid,
		ms:     millis(sm.TimestampMS),
		sender: b.selfFBID,
		mine:   true,
	}
	if n.netID == "" {
		return nil
	}
	if n.ms <= 0 {
		n.ms = time.Now().UnixMilli()
	}
	fill(n)
	c := st.chat
	if q := c.msgs[n.replyNet]; q != nil && n.replyNet != "" {
		// Quoted as Instagram would, until its own copy comes.
		n.replySender = q.sender
		n.replySnippet = proto.Snippet(cmp.Or(q.text, q.service), 100)
	}
	n = b.keep(c, n)
	c.order = max(c.order, n.ms)
	b.putLog(c, n)
	return n
}

// report tells tuimeta the messages were sent; the temporary ids pair with
// the parts in order. From then on, the socket's copies of them are ordinary
// updates.
func (b *Instagram) report(out *backend.Outgoing, sent []*netMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range sent {
		delete(b.sending, n.otid)
	}
	var parts []proto.Message
	var c *chat
	for _, n := range sent {
		c = b.chats[mustKey(out.Chat.NetID)]
		if c == nil {
			return
		}
		parts = append(parts, b.parts(c, n)...)
	}
	out.Sent(parts...)
	if c != nil {
		b.tellChat(c)
	}
}

// sentSome reports what went out before err stopped the rest, or err if
// nothing did.
func (b *Instagram) sentSome(out *backend.Outgoing, sent []*netMsg, err error) error {
	if len(sent) == 0 {
		return err
	}
	b.report(out, sent)
	b.d.Events.Error(proto.Instagram, "Some of the message couldn't be sent to Instagram; send the rest again.")
	return nil
}

func mustKey(netID string) int64 {
	key, _ := strconv.ParseInt(netID, 10, 64)
	return key
}

// EditText edits your text message, within Instagram's limits.
func (b *Instagram) EditText(ctx context.Context, ref backend.MessageRef, text string) error {
	conn, err := b.current()
	if err != nil {
		return err
	}
	if proto.UTF16Len(text) > maxTextLength {
		return proto.Err(proto.Unsupported, "Instagram messages can be at most 1000 characters long.")
	}
	b.mu.Lock()
	c, n, err := b.message(ref)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	igid, netID := c.igid, n.netID
	switch {
	case !n.mine:
		err = proto.Err(proto.BadRequest, "You can only edit your own messages.")
	case !n.plainText || n.service != "":
		err = proto.Err(proto.Unsupported, "Only text messages can be edited on Instagram.")
	case n.editCount >= maxEdits:
		err = proto.Err(proto.Unsupported, "Instagram lets a message be edited only five times.")
	case time.Now().Unix() > n.ms/1000+editWindow:
		err = proto.Err(proto.Unsupported, "This message is too old to edit; Instagram allows edits for 15 minutes.")
	case igid == "":
		err = errNoThread
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = conn.cli.EditMessage(withQuietLog(ctx), &slidetypes.EditMessageRequest{
		ThreadID: igid, Body: slidetypes.SensitiveString{Value: text},
		OfflineThreadingID: newOTID(), TargetMessageID: netID,
	})
	if err != nil {
		return requestError(err, "Instagram didn't take the edit; try again.")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := c.msgs[netID]; n != nil && n.text != text {
		n.text, n.mentions = text, nil
		n.editCount++
		b.update(c, n)
	}
	return nil
}

// longID is the thread's long id, fetched if it isn't known yet.
func (b *Instagram) longID(ctx context.Context, conn *connection, c *chat) (string, error) {
	b.mu.Lock()
	id := c.longID
	b.mu.Unlock()
	if id != "" {
		return id, nil
	}
	ids, err := conn.cli.FetchThreadID(withQuietLog(ctx), c.key)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c.longID = ids.LongID
	b.byLongID[ids.LongID] = c
	if c.igid == "" && ids.ShortID != "" {
		c.igid = ids.ShortID
		b.byIGID[ids.ShortID] = c
	}
	return c.longID, nil
}

// Delete unsends your message for everyone.
func (b *Instagram) Delete(ctx context.Context, ref backend.MessageRef) error {
	conn, err := b.current()
	if err != nil {
		return err
	}
	b.mu.Lock()
	c, n, err := b.message(ref)
	if err == nil && (!n.mine || n.service != "") {
		err = proto.Err(proto.BadRequest, "You can only unsend your own messages.")
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	long, err := b.longID(ctx, conn, c)
	if err != nil {
		return requestError(err, "Instagram didn't unsend the message; try again.")
	}
	resp, err := conn.cli.UnsendMessage(withQuietLog(ctx), &slidetypes.UnsendMessageRequest{
		MessageID: n.netID, SendData: slidetypes.SendData{ThreadID: long},
	})
	if err != nil {
		return requestError(err, "Instagram didn't unsend the message; try again.")
	}
	if resp == nil || !resp.DirectUnsendMessage {
		return proto.Err(proto.NetworkError, "Instagram didn't unsend the message; try again.")
	}
	b.mu.Lock()
	b.removeMessage(c, n.netID)
	b.mu.Unlock()
	return nil
}

// React sets your one reaction ("" takes it back).
func (b *Instagram) React(ctx context.Context, ref backend.MessageRef, emoji string) error {
	conn, err := b.current()
	if err != nil {
		return err
	}
	emoji = variationselector.Remove(emoji) // as Instagram keeps them
	b.mu.Lock()
	c, n, err := b.message(ref)
	if err == nil && n.service != "" {
		err = proto.Err(proto.BadRequest, "Events can't be reacted to.")
	}
	var igid, netID, current string
	if err == nil {
		igid, netID, current = c.igid, n.netID, n.reactionOf(b.selfFBID)
		if igid == "" {
			err = errNoThread
		}
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	input := slidetypes.ReactionInput{Emoji: emoji, MessageID: netID, ReactionStatus: slidetypes.ReactionStatusCreated, ThreadID: igid}
	if emoji == "" {
		if current == "" {
			return nil
		}
		input.Emoji, input.ReactionStatus = current, slidetypes.ReactionStatusDeleted
	}
	resp, err := conn.cli.SendReaction(withQuietLog(ctx), &slidetypes.CreateReactionRequest{Input: input})
	if err != nil {
		return requestError(err, "Instagram didn't take the reaction; try again.")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n = c.msgs[netID]
	if n == nil {
		return nil
	}
	if resp != nil && resp.Message.ID == netID {
		// Instagram answers with the message's reactions as they now are.
		n.reactions = nil
		for _, r := range resp.Message.Reactions {
			if r.Reaction != "" && r.SenderFBID != 0 {
				n.setReaction(r.SenderFBID, r.Reaction)
			}
		}
	} else {
		n.setReaction(b.selfFBID, emoji)
	}
	b.update(c, n)
	return nil
}

// MarkRead sends a read receipt up to the message: the only place one goes
// out. Like the web client, it marks the thread read, then confirms it.
func (b *Instagram) MarkRead(ctx context.Context, ref backend.MessageRef) error {
	conn, err := b.current()
	if err != nil {
		return err
	}
	b.mu.Lock()
	c, n, err := b.message(ref)
	var netID string
	var ms int64
	already := false
	if err == nil {
		netID, ms = n.netID, n.ms
		already = ms <= c.readInbox && !c.markedUnread
	}
	b.mu.Unlock()
	if err != nil || already {
		return err
	}
	long, err := b.longID(ctx, conn, c)
	if err != nil {
		return requestError(err, "Instagram didn't take the read receipt; try again.")
	}
	meta := slidetypes.MarkReadMetadata{IGThreadIGID: long}
	if _, err := conn.cli.MarkRead(withQuietLog(ctx), &slidetypes.MarkReadRequest{
		Metadata: meta, Data: slidetypes.MarkReadData{MessageID: netID, ItemID: new("")},
	}); err != nil {
		return requestError(err, "Instagram didn't take the read receipt; try again.")
	}
	if _, err := conn.cli.MarkReadValidation(withQuietLog(ctx), &slidetypes.MarkReadRequest{
		Metadata: meta, Data: slidetypes.MarkReadData{MessageID: netID, MessageTimestampMS: jsontime.UnixMilliString{Time: time.UnixMilli(ms)}},
	}); err != nil {
		hlog.Warn("instagram: read receipt not confirmed", hlog.Kind(err))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.applyReceipt(c, b.selfFBID, ms)
	c.markedUnread = false
	b.tellRead(c, true, false)
	return nil
}

// SetTyping tells the chat you're typing, or stopped: the only place typing
// goes out.
func (b *Instagram) SetTyping(ctx context.Context, ref backend.ChatRef, typing bool) error {
	conn, err := b.current()
	if err != nil {
		return err
	}
	b.mu.Lock()
	c, err := b.chatOf(ref)
	igid := ""
	if err == nil {
		igid = c.igid
	}
	b.mu.Unlock()
	if err != nil || igid == "" {
		return err
	}
	if err := conn.cli.SetTyping(withQuietLog(ctx), igid, typing); err != nil {
		hlog.Warn("instagram: typing not sent", hlog.Kind(err))
		return proto.Err(proto.NetworkError, "Couldn't tell Instagram you're typing.")
	}
	return nil
}

// Mute mutes the chat for good, or unmutes it.
func (b *Instagram) Mute(ctx context.Context, ref backend.ChatRef, muted bool) error {
	conn, err := b.current()
	if err != nil {
		return err
	}
	b.mu.Lock()
	c, err := b.chatOf(ref)
	igid := ""
	if err == nil {
		igid = c.igid
		if igid == "" {
			err = errNoThread
		}
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	seconds := 0
	if muted {
		seconds = -1 // for good
	}
	if _, err := conn.cli.MuteThread(withQuietLog(ctx), &slidetypes.MuteThreadRequest{
		ThreadID: igid, MuteSeconds: seconds, OfflineThreadingID: newOTID(),
	}); err != nil {
		return requestError(err, "Instagram didn't change the chat's notifications; try again.")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c.muted = muted
	b.changed(c)
	return nil
}

// Search finds people on Instagram, and your groups whose names match.
func (b *Instagram) Search(ctx context.Context, query string) ([]proto.SearchResult, error) {
	conn, err := b.current()
	if err != nil {
		return nil, err
	}
	resp, err := conn.cli.SearchUsers(withQuietLog(ctx), query)
	if err != nil {
		return nil, requestError(err, "Instagram's search didn't answer; try again.")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	results := []proto.SearchResult{}
	if resp != nil {
		for _, r := range resp.Data.Results {
			if r == nil || r.InteropMessagingUserFBID == 0 || r.InteropMessagingUserFBID == b.selfFBID {
				continue
			}
			p := b.person(&slidetypes.User{
				InteropMessagingUserFBID: r.InteropMessagingUserFBID, ID: r.PK,
				Username: r.Username, FullName: r.FullName, ProfilePicURL: r.ProfilePicURL,
			})
			b.tellUser(p)
			res := proto.SearchResult{UserID: p.id, Title: p.displayName(), Username: p.username, Kind: proto.DM}
			if c := b.chats[p.fbid]; c != nil && !c.group {
				res.ChatID = c.id // a dm's key is its person's id
			}
			results = append(results, res)
		}
	}
	q := strings.ToLower(query)
	for _, c := range b.chats {
		if c.group && strings.Contains(strings.ToLower(b.title(c)), q) {
			results = append(results, proto.SearchResult{ChatID: c.id, Title: b.title(c), Kind: proto.Group})
		}
	}
	return results, nil
}

// OpenDM is the chat with a person: the known one, the one Instagram has,
// or a new one that starts on Instagram with the first text sent.
func (b *Instagram) OpenDM(ctx context.Context, ref backend.UserRef) (int64, error) {
	conn, err := b.current()
	if err != nil {
		return 0, err
	}
	fbid, err := strconv.ParseInt(ref.NetID, 10, 64)
	if err != nil || fbid == 0 {
		return 0, proto.ErrNoUser
	}
	b.mu.Lock()
	if c := b.chats[fbid]; c != nil && !c.group {
		if !b.sent[c.id] {
			b.tellChat(c)
		}
		b.mu.Unlock()
		return c.id, nil
	}
	b.mu.Unlock()

	ids, err := conn.cli.FetchThreadID(withQuietLog(ctx), fbid)
	switch {
	case errors.Is(err, instameow.ErrThreadNotFound):
		var info *slidetypes.User
		if resp, err := conn.cli.GetUserForNewDM(withQuietLog(ctx), fbid); err == nil && resp != nil {
			info = resp.Data
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if info != nil {
			info.InteropMessagingUserFBID = fbid
			b.person(info)
		}
		c := b.upsertThread(&slidetypes.ThreadInfo{ThreadKey: fbid}, false)
		c.members = []int64{fbid}
		c.fetched = true
		c.log.SetComplete(true)
		b.tellChat(c)
		return c.id, nil
	case err != nil:
		return 0, requestError(err, "Instagram didn't open the chat; try again.")
	}
	resp, err := conn.cli.GetThread(withQuietLog(ctx), slidetypes.MakeGetThreadInfoRequest(ids.ShortID))
	if err != nil || resp == nil || resp.ThreadInfo.AsIGDirectThread == nil {
		return 0, requestError(cmp.Or(err, instameow.ErrThreadNotFound), "Instagram didn't open the chat; try again.")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.threadPage(resp.ThreadInfo.AsIGDirectThread)
	if c == nil {
		return 0, errNoThread
	}
	if !b.sent[c.id] {
		b.tellChat(c)
	}
	return c.id, nil
}
