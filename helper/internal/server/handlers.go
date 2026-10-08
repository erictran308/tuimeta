// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

type handler func(s *Server, c *call) (any, error)

var handlers map[string]handler

func init() {
	handlers = map[string]handler{
		"login_cookies": (*Server).loginCookies,
		"logout":        (*Server).logout,
		"load_chats":    (*Server).loadChats,
		"history":       (*Server).history,
		"get_message":   (*Server).getMessage,
		"send_text":     (*Server).sendText,
		"send_files":    (*Server).sendFiles,
		"edit_text":     (*Server).editText,
		"delete":        (*Server).delete,
		"react":         (*Server).react,
		"mark_read":     (*Server).markRead,
		"typing":        (*Server).typing,
		"download":      (*Server).download,
		"search":        (*Server).search,
		"open_dm":       (*Server).openDM,
		"mute":          (*Server).mute,
	}
}

// Limits on what a request may ask for.
const (
	DefaultLimit = 50
	MaxLimit     = 500
	MaxText      = 64 << 10
	MaxQuery     = 256
	// SendTimeout is how long a send may go unconfirmed before it's
	// reported failed.
	SendTimeout = 2 * time.Minute
)

var errParams = proto.Err(proto.BadRequest, "The request's params are malformed.")

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 || string(raw) == "null" {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, errParams
	}
	return v, nil
}

func limit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	return min(n, MaxLimit)
}

// network is the named network's backend, whatever its account's state.
func (s *Server) network(name string) (proto.Network, backend.Backend, error) {
	n := proto.Network(name)
	if name == "" {
		return "", nil, proto.Err(proto.BadRequest, "Say which network: messenger or instagram.")
	}
	b := s.backend(n)
	if !n.Valid() || b == nil {
		return "", nil, proto.Err(proto.BadRequest, "Unknown network; use messenger or instagram.")
	}
	return n, b, nil
}

// ready refuses requests to a network nobody is logged in to.
func (s *Server) ready(n proto.Network) error {
	if s.Events.State(n) == proto.LoggedOut {
		return proto.ErrLoggedOut(n)
	}
	return nil
}

// chat resolves a chat id to its network's backend, which must be logged in.
func (s *Server) chat(id int64) (backend.ChatRef, backend.Backend, error) {
	if id <= 0 {
		return backend.ChatRef{}, nil, proto.Err(proto.BadRequest, "chat_id is missing.")
	}
	n, netID, ok := s.IDs.LookupChat(id)
	if !ok {
		return backend.ChatRef{}, nil, proto.ErrNoChat
	}
	b := s.backend(n)
	if b == nil {
		return backend.ChatRef{}, nil, proto.ErrNoChat
	}
	if err := s.ready(n); err != nil {
		return backend.ChatRef{}, nil, err
	}
	return backend.ChatRef{ID: id, Network: n, NetID: netID}, b, nil
}

// message resolves a message id in chat. Temporary ids name nothing yet.
func (s *Server) message(chat backend.ChatRef, id int64) (backend.MessageRef, error) {
	if id <= 0 {
		return backend.MessageRef{}, proto.Err(proto.BadRequest, "message_id is missing.")
	}
	p, ok := s.Messages.Lookup(chat.ID, id)
	if !ok {
		return backend.MessageRef{}, proto.ErrNoMessage
	}
	if p.Temp {
		return backend.MessageRef{}, proto.ErrStillGoing
	}
	return backend.MessageRef{Chat: chat, ID: id, NetID: p.NetID, Index: p.Index, Count: p.Count, IDs: p.IDs}, nil
}

func (s *Server) chatAndMessage(chatID, msgID int64) (backend.MessageRef, backend.Backend, error) {
	chat, b, err := s.chat(chatID)
	if err != nil {
		return backend.MessageRef{}, nil, err
	}
	ref, err := s.message(chat, msgID)
	return ref, b, err
}

func (s *Server) loginCookies(c *call) (any, error) {
	p, err := decode[struct {
		Network string `json:"network"`
		Cookies string `json:"cookies"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	n, b, err := s.network(p.Network)
	if err != nil {
		return nil, err
	}
	if s.Events.State(n) == proto.Ready {
		return nil, proto.Err(proto.BadRequest, "Already logged in to "+n.Title()+"; log out first.")
	}
	set, err := cookies.Parse(n, p.Cookies)
	if err != nil {
		hlog.Info("login refused", hlog.Str("network", string(n)), hlog.Kind(err))
		return nil, err
	}
	hlog.Info("logging in", hlog.Str("network", string(n)))
	if err := b.LoginCookies(c.ctx, set); err != nil {
		hlog.Info("login failed", hlog.Str("network", string(n)), hlog.Kind(err))
		return nil, err
	}
	hlog.Info("logged in", hlog.Str("network", string(n)))
	return nil, nil
}

func (s *Server) logout(c *call) (any, error) {
	p, err := decode[struct {
		Network string `json:"network"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	n, b, err := s.network(p.Network)
	if err != nil {
		return nil, err
	}
	err = b.Logout(c.ctx)
	// Whatever the network said, nothing of the account stays here.
	for _, id := range s.IDs.ChatsOf(n) {
		s.Messages.ForgetChat(id)
	}
	s.IDs.Forget(n)
	if ferr := s.Downloads.Forget(n); ferr != nil {
		hlog.Error("can't delete downloads", hlog.Str("network", string(n)), hlog.Kind(ferr))
	}
	hlog.Info("logged out", hlog.Str("network", string(n)), hlog.Kind(err))
	return nil, err
}

func (s *Server) loadChats(c *call) (any, error) {
	p, err := decode[struct {
		Network *string `json:"network"`
		Limit   int     `json:"limit"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	lim := limit(p.Limit)
	if p.Network != nil {
		n, b, err := s.network(*p.Network)
		if err != nil {
			return nil, err
		}
		if err := s.ready(n); err != nil {
			return nil, err
		}
		more, err := b.LoadChats(c.ctx, lim)
		if err != nil {
			return nil, err
		}
		return map[string]any{"has_more": more}, nil
	}
	// No network: each logged-in network sends up to limit more.
	more := false
	var firstErr error
	tried, failed := 0, 0
	for _, n := range proto.Networks {
		b := s.backend(n)
		if b == nil || s.ready(n) != nil {
			continue
		}
		tried++
		m, err := b.LoadChats(c.ctx, lim)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			hlog.Warn("load_chats failed", hlog.Str("network", string(n)), hlog.Kind(err))
			continue
		}
		more = more || m
	}
	if tried > 0 && failed == tried {
		return nil, firstErr
	}
	return map[string]any{"has_more": more}, nil
}

func (s *Server) history(c *call) (any, error) {
	p, err := decode[struct {
		ChatID int64  `json:"chat_id"`
		Before *int64 `json:"before"`
		After  *int64 `json:"after"`
		Around *int64 `json:"around"`
		Limit  int    `json:"limit"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	set := 0
	for _, v := range []*int64{p.Before, p.After, p.Around} {
		if v != nil {
			set++
		}
	}
	if set > 1 {
		return nil, proto.Err(proto.BadRequest, "Give at most one of before, after and around.")
	}
	chat, b, err := s.chat(p.ChatID)
	if err != nil {
		return nil, err
	}
	page, err := b.History(c.ctx, chat, history.Query{Before: p.Before, After: p.After, Around: p.Around, Limit: limit(p.Limit)})
	if err != nil {
		return nil, err
	}
	if page.Messages == nil {
		page.Messages = []proto.Message{}
	}
	return map[string]any{"messages": page.Messages, "has_more": page.HasMore}, nil
}

type messageParams struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

func (s *Server) getMessage(c *call) (any, error) {
	p, err := decode[messageParams](c.params)
	if err != nil {
		return nil, err
	}
	ref, b, err := s.chatAndMessage(p.ChatID, p.MessageID)
	if err != nil {
		return nil, err
	}
	m, err := b.GetMessage(c.ctx, ref)
	if err != nil {
		return nil, err
	}
	return map[string]any{"message": m}, nil
}

// checkText refuses text that's empty or absurdly long.
func checkText(text string) error {
	if strings.TrimSpace(text) == "" {
		return proto.Err(proto.BadRequest, "There's nothing to send.")
	}
	if len(text) > MaxText {
		return proto.Err(proto.BadRequest, "That message is too long.")
	}
	if !utf8.ValidString(text) {
		return proto.Err(proto.BadRequest, "That text isn't valid UTF-8.")
	}
	return nil
}

// replyTo resolves the message a send answers, and quotes it for the
// pending message.
func (s *Server) replyTo(c *call, chat backend.ChatRef, b backend.Backend, id *int64) (*backend.MessageRef, *proto.ReplyTo, error) {
	if id == nil {
		return nil, nil, nil
	}
	ref, err := s.message(chat, *id)
	if err != nil {
		return nil, nil, err
	}
	quote := &proto.ReplyTo{MessageID: ref.ID}
	if m, err := b.GetMessage(c.ctx, ref); err == nil {
		quote.SenderID = m.SenderID
		quote.Text = proto.Snippet(m.Text, 100)
	}
	return &ref, quote, nil
}

// startSend reports the pending messages, then (once the request is
// answered) hands the message to the backend and waits for the outcome.
func (s *Server) startSend(c *call, b backend.Backend, out *backend.Outgoing, pending []proto.Message) {
	for _, m := range pending {
		s.Events.Message(m)
	}
	c.then = func() {
		if err := b.Send(c.ctx, out); err != nil {
			out.Failed(err)
			return
		}
		select {
		case <-out.Done():
		case <-c.ctx.Done():
		case <-time.After(SendTimeout):
			out.Failed(proto.Err(proto.NetworkError, "The network didn't confirm the message; it may not have been sent."))
		}
	}
}

func (s *Server) pendingBase(chat backend.ChatRef, text string, quote *proto.ReplyTo) proto.Message {
	return proto.Message{
		ChatID:   chat.ID,
		SenderID: s.Events.Self(chat.Network),
		Outgoing: true,
		Date:     time.Now().Unix(),
		Text:     text,
		ReplyTo:  quote,
		State:    proto.Pending,
	}
}

func (s *Server) sendText(c *call) (any, error) {
	p, err := decode[struct {
		ChatID  int64  `json:"chat_id"`
		Text    string `json:"text"`
		ReplyTo *int64 `json:"reply_to"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	if err := checkText(p.Text); err != nil {
		return nil, err
	}
	chat, b, err := s.chat(p.ChatID)
	if err != nil {
		return nil, err
	}
	reply, quote, err := s.replyTo(c, chat, b, p.ReplyTo)
	if err != nil {
		return nil, err
	}
	temps := s.Messages.Temp(chat.ID, 1)
	pending := s.pendingBase(chat, p.Text, quote)
	pending.ID = temps[0]
	out := s.Outbox.New(chat, temps, p.Text, reply, nil)
	s.startSend(c, b, out, []proto.Message{pending})
	return map[string]any{"message_id": temps[0]}, nil
}

func (s *Server) sendFiles(c *call) (any, error) {
	p, err := decode[struct {
		ChatID  int64    `json:"chat_id"`
		Paths   []string `json:"paths"`
		Caption *string  `json:"caption"`
		ReplyTo *int64   `json:"reply_to"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	caption := ""
	if p.Caption != nil && strings.TrimSpace(*p.Caption) != "" {
		caption = *p.Caption
		if err := checkText(caption); err != nil {
			return nil, err
		}
	}
	chat, b, err := s.chat(p.ChatID)
	if err != nil {
		return nil, err
	}
	reply, quote, err := s.replyTo(c, chat, b, p.ReplyTo)
	if err != nil {
		return nil, err
	}
	uploads, err := readUploads(p.Paths)
	if err != nil {
		return nil, err
	}
	temps := s.Messages.Temp(chat.ID, len(uploads))
	media := make([]proto.Media, len(uploads))
	for i, u := range uploads {
		media[i] = proto.Media{Kind: u.Kind, Name: u.Name, Mime: u.Mime, Size: int64(len(u.Data)), Width: u.Width, Height: u.Height}
	}
	pending := proto.SplitAlbum(s.pendingBase(chat, caption, quote), media, temps)
	out := s.Outbox.New(chat, temps, caption, reply, uploads)
	s.startSend(c, b, out, pending)
	return map[string]any{"message_ids": temps}, nil
}

func (s *Server) editText(c *call) (any, error) {
	p, err := decode[struct {
		messageParams
		Text string `json:"text"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	if err := checkText(p.Text); err != nil {
		return nil, err
	}
	ref, b, err := s.chatAndMessage(p.ChatID, p.MessageID)
	if err != nil {
		return nil, err
	}
	return nil, b.EditText(c.ctx, ref, p.Text)
}

func (s *Server) delete(c *call) (any, error) {
	p, err := decode[messageParams](c.params)
	if err != nil {
		return nil, err
	}
	ref, b, err := s.chatAndMessage(p.ChatID, p.MessageID)
	if err != nil {
		return nil, err
	}
	return nil, b.Delete(c.ctx, ref)
}

func (s *Server) react(c *call) (any, error) {
	p, err := decode[struct {
		messageParams
		Emoji *string `json:"emoji"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	emoji := ""
	if p.Emoji != nil {
		emoji = *p.Emoji
	}
	if len(emoji) > 64 || !utf8.ValidString(emoji) || strings.ContainsFunc(emoji, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return nil, proto.Err(proto.BadRequest, "That isn't a reaction.")
	}
	ref, b, err := s.chatAndMessage(p.ChatID, p.MessageID)
	if err != nil {
		return nil, err
	}
	return nil, b.React(c.ctx, ref, emoji)
}

func (s *Server) markRead(c *call) (any, error) {
	p, err := decode[messageParams](c.params)
	if err != nil {
		return nil, err
	}
	ref, b, err := s.chatAndMessage(p.ChatID, p.MessageID)
	if err != nil {
		return nil, err
	}
	return nil, b.MarkRead(c.ctx, ref)
}

func (s *Server) typing(c *call) (any, error) {
	p, err := decode[struct {
		ChatID int64 `json:"chat_id"`
		Typing bool  `json:"typing"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	chat, b, err := s.chat(p.ChatID)
	if err != nil {
		return nil, err
	}
	return nil, b.SetTyping(c.ctx, chat, p.Typing)
}

func (s *Server) download(c *call) (any, error) {
	p, err := decode[struct {
		FileID   int32  `json:"file_id"`
		Priority string `json:"priority"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	prio := proto.Priority(p.Priority)
	switch prio {
	case proto.High, proto.Low:
	case "":
		prio = proto.Low
	default:
		return nil, proto.Err(proto.BadRequest, "priority is high or low.")
	}
	if p.FileID <= 0 {
		return nil, proto.Err(proto.BadRequest, "file_id is missing.")
	}
	ref, ok := s.Files.Get(p.FileID)
	if !ok {
		return nil, proto.ErrNoFile
	}
	if err := s.ready(ref.Network); err != nil {
		return nil, err
	}
	return nil, s.Downloads.Download(p.FileID, prio)
}

func (s *Server) search(c *call) (any, error) {
	p, err := decode[struct {
		Network string `json:"network"`
		Query   string `json:"query"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	n, b, err := s.network(p.Network)
	if err != nil {
		return nil, err
	}
	if err := s.ready(n); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(p.Query)
	if len(q) > MaxQuery {
		return nil, proto.Err(proto.BadRequest, "That search is too long.")
	}
	results := []proto.SearchResult{}
	if q != "" {
		found, err := b.Search(c.ctx, q)
		if err != nil {
			return nil, err
		}
		results = append(results, found...)
	}
	return map[string]any{"results": results}, nil
}

func (s *Server) openDM(c *call) (any, error) {
	p, err := decode[struct {
		Network string `json:"network"`
		UserID  int64  `json:"user_id"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	n, b, err := s.network(p.Network)
	if err != nil {
		return nil, err
	}
	if err := s.ready(n); err != nil {
		return nil, err
	}
	un, netID, ok := s.IDs.LookupUser(p.UserID)
	if !ok || un != n {
		return nil, proto.ErrNoUser
	}
	if p.UserID == s.Events.Self(n) {
		return nil, proto.Err(proto.BadRequest, "That's you.")
	}
	id, err := b.OpenDM(c.ctx, backend.UserRef{ID: p.UserID, Network: n, NetID: netID})
	if err != nil {
		return nil, err
	}
	return map[string]any{"chat_id": id}, nil
}

func (s *Server) mute(c *call) (any, error) {
	p, err := decode[struct {
		ChatID int64 `json:"chat_id"`
		Muted  bool  `json:"muted"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	chat, b, err := s.chat(p.ChatID)
	if err != nil {
		return nil, err
	}
	return nil, b.Mute(c.ctx, chat, p.Muted)
}
