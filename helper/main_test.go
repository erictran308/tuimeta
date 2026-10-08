// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/fake"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/wiretest"
)

// The test binary doubles as the helper for TestTheBinary..., which runs it
// as tuimeta does.
func TestMain(m *testing.M) {
	if os.Getenv("TUIMETA_HELPER_TEST_AS_MAIN") == "1" {
		os.Args = append([]string{"tuimeta-helper"}, strings.Fields(os.Getenv("TUIMETA_HELPER_TEST_ARGS"))...)
		main()
		return
	}
	os.Exit(m.Run())
}

type helper struct {
	t     *testing.T
	c     *wiretest.Client
	stdin *io.PipeWriter
	done  chan struct{}
	code  int
	dir   string
}

// startHelper runs the helper in fake mode through run, on pipes.
func startHelper(t *testing.T, dir string) *helper {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &helper{t: t, stdin: inW, done: make(chan struct{}), dir: dir}
	h.c = wiretest.New(t, inW, outR)
	go func() {
		h.code = run([]string{"--data-dir", dir, "--fake"}, inR, outW, io.Discard, nil)
		outW.Close()
		close(h.done)
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			t.Error("the helper didn't stop")
		}
	})
	return h
}

var goodCookies = map[proto.Network]string{
	proto.Messenger: "c_user=1; xs=2; datr=3",
	proto.Instagram: `[{"name":"sessionid","value":"a"},{"name":"ds_user_id","value":"2"},{"name":"csrftoken","value":"c"}]`,
}

func (h *helper) login(n proto.Network) {
	h.t.Helper()
	l := h.c.Call("login_cookies", map[string]any{"network": n, "cookies": goodCookies[n]})
	if l.Error != nil {
		h.t.Fatalf("login %s: %s", n, l.Raw)
	}
}

// link logs a linking network in with its QR code, which the fake takes as
// scanned fake.LinkAfter later.
func (h *helper) link(n proto.Network) {
	h.t.Helper()
	l := h.c.Call("login_link", map[string]any{"network": n})
	if l.Error != nil {
		h.t.Fatalf("link %s: %s", n, l.Raw)
	}
}

// quickLinks makes fake links take d for the rest of the test. Call it
// before startHelper, so the helper has stopped when it's put back.
func quickLinks(t *testing.T, d time.Duration) {
	old := fake.LinkAfter
	fake.LinkAfter = d
	t.Cleanup(func() { fake.LinkAfter = old })
}

func (h *helper) ok(method string, params any) json.RawMessage {
	h.t.Helper()
	l := h.c.Call(method, params)
	if l.Error != nil {
		h.t.Fatalf("%s: %s", method, l.Raw)
	}
	return l.Result
}

// loadChats loads the network's chat list (all networks if n is ""), page
// by page, and returns the chats in the order they came.
func (h *helper) loadChats(n proto.Network, limit int) []proto.Chat {
	h.t.Helper()
	params := map[string]any{"limit": limit}
	if n != "" {
		params["network"] = n
	}
	var chats []proto.Chat
	for range 20 {
		from := h.c.Mark()
		l := h.c.Call("load_chats", params)
		if l.Error != nil {
			h.t.Fatalf("load_chats: %s", l.Raw)
		}
		var page []proto.Chat
		for _, line := range h.c.Lines()[from:] {
			if line.Event == "chat" && line.Seq < l.Seq {
				page = append(page, wiretest.Field[proto.Chat](h.t, line, "chat"))
			}
		}
		perNet := map[proto.Network]int{}
		for i, c := range page {
			perNet[c.Network]++
			if i > 0 && page[i-1].Network == c.Network && page[i-1].Order < c.Order {
				h.t.Errorf("chats not newest first: %d after %d", c.Order, page[i-1].Order)
			}
		}
		for net, count := range perNet {
			if count > limit {
				h.t.Errorf("%d chats of %s for limit %d", count, net, limit)
			}
		}
		chats = append(chats, page...)
		if !wiretest.Decode[struct {
			HasMore bool `json:"has_more"`
		}](h.t, l.Result).HasMore {
			return chats
		}
	}
	h.t.Fatal("load_chats never said has_more false")
	return nil
}

type page struct {
	Messages []proto.Message `json:"messages"`
	HasMore  bool            `json:"has_more"`
}

func (h *helper) history(params map[string]any) page {
	h.t.Helper()
	return wiretest.Decode[page](h.t, h.ok("history", params))
}

// everything pages back from the newest message to the first.
func (h *helper) everything(chat int64, limit int) []proto.Message {
	h.t.Helper()
	p := h.history(map[string]any{"chat_id": chat, "limit": limit})
	all := p.Messages
	for p.HasMore {
		p = h.history(map[string]any{"chat_id": chat, "limit": limit, "before": all[0].ID})
		if len(p.Messages) == 0 {
			h.t.Fatal("has_more with an empty page")
		}
		all = append(slices.Clone(p.Messages), all...)
	}
	return all
}

func TestFakeLoginNeedsTheRequiredCookieNames(t *testing.T) {
	h := startHelper(t, t.TempDir())
	l := h.c.Call("login_cookies", map[string]any{"network": "messenger", "cookies": "c_user=1; xs=2"})
	if l.Error == nil || l.Error.Code != proto.BadCookies || !strings.Contains(l.Error.Message, "datr") {
		t.Fatalf("got %s", l.Raw)
	}
	l = h.c.Call("load_chats", map[string]any{"network": "messenger", "limit": 5})
	if l.Error == nil || l.Error.Code != proto.NotLoggedIn {
		t.Fatalf("logged out load_chats: %s", l.Raw)
	}
	from := h.c.Mark()
	h.login(proto.Messenger)
	ready := h.c.Event("account", from, func(l wiretest.Line) bool {
		return wiretest.Field[string](t, l, "state") == "ready"
	})
	ev := wiretest.Decode[proto.AccountEvent](t, json.RawMessage(ready.Raw))
	if ev.Network != proto.Messenger || ev.UserID <= 0 || ev.Name == "" {
		t.Errorf("ready = %s", ready.Raw)
	}
	l = h.c.Call("login_cookies", map[string]any{"network": "instagram", "cookies": "c_user=1; xs=2; datr=3"})
	if l.Error == nil || l.Error.Code != proto.BadCookies {
		t.Fatalf("instagram with messenger cookies: %s", l.Raw)
	}
	h.login(proto.Instagram)
}

func TestFakeWhatsAppLinksByQRCode(t *testing.T) {
	h := startHelper(t, t.TempDir())
	if l := h.c.Call("login_cookies", map[string]any{"network": "whatsapp", "cookies": goodCookies[proto.Messenger]}); l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Fatalf("cookies for whatsapp: %s", l.Raw)
	}
	began := time.Now()
	from := h.c.Mark()
	id := h.c.Request("login_link", map[string]any{"network": "whatsapp"})
	code := h.c.Event("login_code", from, nil)
	ev := wiretest.Decode[proto.LoginCodeEvent](t, json.RawMessage(code.Raw))
	now := time.Now()
	if ev.Network != proto.WhatsApp || !strings.HasPrefix(ev.QR, "2@fake") || ev.Pairing != "" ||
		ev.Expires <= now.Unix() || ev.Expires > now.Add(61*time.Second).Unix() {
		t.Fatalf("code: %s", code.Raw)
	}
	resp := h.c.Response(id)
	if resp.Error != nil {
		t.Fatalf("login_link: %s", resp.Raw)
	}
	if d := time.Since(began); d < fake.LinkAfter-200*time.Millisecond {
		t.Errorf("linked after %v", d)
	}
	ready := h.c.Event("account", code.Seq, func(l wiretest.Line) bool {
		return wiretest.Field[string](t, l, "state") == "ready"
	})
	acct := wiretest.Decode[proto.AccountEvent](t, json.RawMessage(ready.Raw))
	if acct.Network != proto.WhatsApp || acct.UserID <= 0 || acct.Name == "" || ready.Seq > resp.Seq {
		t.Errorf("ready (line %d, answer %d): %s", ready.Seq, resp.Seq, ready.Raw)
	}

	chats := h.loadChats(proto.WhatsApp, 4)
	if len(chats) != 6 {
		t.Fatalf("whatsapp has %d chats", len(chats))
	}
	var groups, muted, archived, unread int
	for _, c := range chats {
		if c.Network != proto.WhatsApp || !c.Encrypted || c.Request || !c.CanSend || c.LastMessage == nil {
			t.Errorf("chat: %+v", c)
		}
		for _, flag := range []struct {
			on bool
			n  *int
		}{{c.Kind == proto.Group, &groups}, {c.Muted, &muted}, {c.Archived, &archived}, {c.Unread > 0, &unread}} {
			if flag.on {
				*flag.n++
			}
		}
	}
	if groups != 2 || muted != 1 || archived != 1 || unread != 4 {
		t.Errorf("groups %d, muted %d, archived %d, unread %d", groups, muted, archived, unread)
	}
	if l := h.c.Call("login_link", map[string]any{"network": "whatsapp"}); l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Errorf("linking again while linked: %s", l.Raw)
	}
}

func TestFakeWhatsAppLinksByPhoneNumber(t *testing.T) {
	quickLinks(t, 200*time.Millisecond)
	h := startHelper(t, t.TempDir())
	from := h.c.Mark()
	for _, bad := range []string{"12345", "+0 7700 900100", "+44 7700 900100 1234567"} {
		if l := h.c.Call("login_link", map[string]any{"network": "whatsapp", "phone": bad}); l.Error == nil || l.Error.Code != proto.BadRequest {
			t.Errorf("phone %q: %s", bad, l.Raw)
		}
	}
	for _, l := range h.c.Lines()[from:] {
		if l.Event == "login_code" || l.Event == "account" && !strings.Contains(l.Raw, `"logged_out"`) {
			t.Errorf("a refused number got as far as: %s", l.Raw)
		}
	}
	from = h.c.Mark()
	id := h.c.Request("login_link", map[string]any{"network": "whatsapp", "phone": "+44 7700 900100"})
	code := h.c.Event("login_code", from, nil)
	ev := wiretest.Decode[proto.LoginCodeEvent](t, json.RawMessage(code.Raw))
	now := time.Now()
	if ev.Pairing != "FAKE-C0DE" || ev.QR != "" || ev.Expires <= now.Add(170*time.Second).Unix() || ev.Expires > now.Add(181*time.Second).Unix() {
		t.Fatalf("code: %s", code.Raw)
	}
	if resp := h.c.Response(id); resp.Error != nil {
		t.Fatalf("login_link: %s", resp.Raw)
	}
	if got := h.loadChats(proto.WhatsApp, 10); len(got) != 6 {
		t.Errorf("%d chats after linking by number", len(got))
	}
}

func TestFakeWhatsAppLinkEndsWhenCancelledReplacedOrLoggedOut(t *testing.T) {
	quickLinks(t, 400*time.Millisecond)
	h := startHelper(t, t.TempDir())
	cancelled := func(id uint64) wiretest.Line {
		t.Helper()
		resp := h.c.Response(id)
		if resp.Error == nil || resp.Error.Code != proto.Cancelled {
			t.Fatalf("waiting login_link: %s", resp.Raw)
		}
		return resp
	}

	from := h.c.Mark()
	id := h.c.Request("login_link", map[string]any{"network": "whatsapp"})
	h.c.Event("login_code", from, nil)
	h.ok("cancel_login", map[string]any{"network": "whatsapp"})
	resp := cancelled(id)
	if l, ok := h.c.WaitFor(fake.LinkAfter+600*time.Millisecond, resp.Seq, func(l wiretest.Line) bool {
		return l.Event == "account" && strings.Contains(l.Raw, `"ready"`)
	}); ok {
		t.Fatalf("a cancelled link logged in: %s", l.Raw)
	}
	if l := h.c.Call("load_chats", map[string]any{"network": "whatsapp", "limit": 5}); l.Error == nil || l.Error.Code != proto.NotLoggedIn {
		t.Errorf("load_chats after cancelling: %s", l.Raw)
	}

	from = h.c.Mark()
	id = h.c.Request("login_link", map[string]any{"network": "whatsapp"})
	h.c.Event("login_code", from, nil)
	h.ok("logout", map[string]any{"network": "whatsapp"})
	cancelled(id)

	from = h.c.Mark()
	first := h.c.Request("login_link", map[string]any{"network": "whatsapp"})
	h.c.Event("login_code", from, nil)
	second := h.c.Request("login_link", map[string]any{"network": "whatsapp", "phone": "+44 7700 900100"})
	cancelled(first)
	if resp := h.c.Response(second); resp.Error != nil {
		t.Fatalf("the newer login_link: %s", resp.Raw)
	}
	readies := 0
	for _, l := range h.c.Lines()[from:] {
		if l.Event == "account" && strings.Contains(l.Raw, `"ready"`) {
			readies++
		}
	}
	if readies != 1 {
		t.Errorf("%d ready accounts for one link", readies)
	}
}

func TestFakeWhatsAppHistoryHasWhatTheUIDraws(t *testing.T) {
	quickLinks(t, 50*time.Millisecond)
	h := startHelper(t, t.TempDir())
	h.link(proto.WhatsApp)
	chats := h.loadChats(proto.WhatsApp, 10)
	priya := chats[0] // the newest
	if priya.Title != "Priya Shah" || priya.Kind != proto.DM || priya.Unread != 2 {
		t.Fatalf("first chat: %+v", priya)
	}
	all := h.everything(priya.ID, 20)
	newestPage := h.history(map[string]any{"chat_id": priya.ID, "limit": 20}).Messages
	var album []proto.Message
	var reply *proto.Message
	var edited, file, mine, link, pre, code bool
	for i, m := range all {
		if m.Album != 0 {
			album = append(album, m)
		}
		if m.ReplyTo != nil && m.ReplyTo.MessageID < newestPage[0].ID && reply == nil {
			reply = &all[i]
		}
		edited = edited || m.Edited
		file = file || m.Media != nil && m.Media.Kind == proto.FileMedia && m.Media.Name == "lisbon-booking.pdf"
		for _, e := range m.Entities {
			link = link || e.Type == proto.Link && e.URL != ""
			pre = pre || e.Type == proto.Pre
			code = code || e.Type == proto.InlineCode
		}
		for _, r := range m.Reactions {
			mine = mine || r.Mine
		}
	}
	if len(album) != 3 || album[1].ID != album[0].ID+1 || album[2].Text == "" {
		t.Errorf("album: %+v", album)
	}
	if reply == nil {
		t.Fatal("no reply to a message older than the newest page")
	}
	got := wiretest.Decode[struct {
		Message proto.Message `json:"message"`
	}](t, h.ok("get_message", map[string]any{"chat_id": priya.ID, "message_id": reply.ReplyTo.MessageID}))
	if got.Message.SenderID != reply.ReplyTo.SenderID {
		t.Errorf("get_message of the answered message: %+v", got.Message)
	}
	if !edited || !file || !mine || !link || !pre || !code {
		t.Errorf("edited %v, file %v, mine %v, link %v, pre %v, code %v", edited, file, mine, link, pre, code)
	}

	// People are found by name or number, and those without a chat can
	// get one.
	res := wiretest.Decode[struct {
		Results []proto.SearchResult `json:"results"`
	}](t, h.ok("search", map[string]any{"network": "whatsapp", "query": "+44 7700 900108"})).Results
	if len(res) != 1 || res[0].Title != "Hannah Berg" || res[0].ChatID != 0 || res[0].Username != "" {
		t.Fatalf("search by number: %+v", res)
	}
	dm := wiretest.Decode[struct {
		ChatID int64 `json:"chat_id"`
	}](t, h.ok("open_dm", map[string]any{"network": "whatsapp", "user_id": res[0].UserID})).ChatID
	if dm == 0 {
		t.Error("open_dm made no chat")
	}
	if res := wiretest.Decode[struct {
		Results []proto.SearchResult `json:"results"`
	}](t, h.ok("search", map[string]any{"network": "whatsapp", "query": "5-a-side"})).Results; len(res) != 1 || res[0].Kind != proto.Group {
		t.Errorf("search for the group: %+v", res)
	}
}

func TestFakeChatsPageUntilHasMoreIsFalse(t *testing.T) {
	h := startHelper(t, t.TempDir())
	h.login(proto.Messenger)
	h.login(proto.Instagram)

	m := h.loadChats(proto.Messenger, 4)
	if len(m) != 6 {
		t.Fatalf("messenger has %d chats", len(m))
	}
	// Without a network, each logged-in network sends what's left.
	rest := h.loadChats("", 4)
	if len(rest) != 6 {
		t.Fatalf("then %d more chats", len(rest))
	}
	for _, c := range rest {
		if c.Network != proto.Instagram {
			t.Errorf("messenger chat %d sent twice", c.ID)
		}
	}
	all := append(m, rest...)
	seen := map[int64]bool{}
	var encrypted, request, muted, archived, unread, groups int
	for _, c := range all {
		if seen[c.ID] {
			t.Errorf("chat %d twice", c.ID)
		}
		seen[c.ID] = true
		if c.LastMessage == nil || c.Order != c.LastMessage.ID>>8 || c.Title == "" || !c.CanSend {
			t.Errorf("chat %d: %+v", c.ID, c)
		}
		if c.Kind == proto.DM && c.UserID == 0 {
			t.Errorf("dm %d without its person", c.ID)
		}
		if c.Encrypted {
			encrypted++
			if c.Network != proto.Messenger {
				t.Errorf("an encrypted %s chat", c.Network)
			}
		}
		for _, flag := range []struct {
			on bool
			n  *int
		}{{c.Request, &request}, {c.Muted, &muted}, {c.Archived, &archived}, {c.Unread > 0, &unread}, {c.Kind == proto.Group, &groups}} {
			if flag.on {
				*flag.n++
			}
		}
	}
	if encrypted != 1 || request != 1 || muted < 1 || archived < 1 || unread < 3 || groups < 2 {
		t.Errorf("encrypted %d, request %d, muted %d, archived %d, unread %d, groups %d", encrypted, request, muted, archived, unread, groups)
	}
}

func TestFakeHistoryPagesEveryWay(t *testing.T) {
	h := startHelper(t, t.TempDir())
	h.login(proto.Messenger)
	chats := h.loadChats(proto.Messenger, 10)
	alice := chats[0] // the newest: the encrypted dm
	if !alice.Encrypted {
		t.Fatalf("first chat: %+v", alice)
	}

	all := h.everything(alice.ID, 20)
	networkMessages := map[int64]bool{}
	for i, m := range all {
		if i > 0 && m.ID <= all[i-1].ID {
			t.Fatalf("not oldest first at %d", i)
		}
		if m.ChatID != alice.ID {
			t.Errorf("message %d in chat %d", m.ID, m.ChatID)
		}
		key := m.ID
		if m.Album != 0 {
			key = m.Album
		}
		networkMessages[key] = true
	}
	if len(networkMessages) != 60 {
		t.Fatalf("%d network messages, %d parts", len(networkMessages), len(all))
	}
	if newest := all[len(all)-1]; newest.ID != alice.LastMessage.ID {
		t.Errorf("newest %d, chat's last %d", newest.ID, alice.LastMessage.ID)
	}

	// Forward from before the first, starting at a message with after: id-1.
	var forward []proto.Message
	after := all[0].ID - 1
	for {
		p := h.history(map[string]any{"chat_id": alice.ID, "limit": 25, "after": after})
		forward = append(forward, p.Messages...)
		if !p.HasMore {
			break
		}
		after = p.Messages[len(p.Messages)-1].ID
	}
	if len(forward) != len(all) || forward[0].ID != all[0].ID || forward[len(forward)-1].ID != all[len(all)-1].ID {
		t.Fatalf("forward paging got %d of %d", len(forward), len(all))
	}

	// Around a message, and around a position between two.
	mid := all[30]
	p := h.history(map[string]any{"chat_id": alice.ID, "limit": 10, "around": mid.ID})
	at := slices.IndexFunc(p.Messages, func(m proto.Message) bool { return m.ID == mid.ID })
	if at < 0 || len(p.Messages) < 10 || at < 3 || len(p.Messages)-at < 3 {
		t.Fatalf("around: target at %d of %d", at, len(p.Messages))
	}
	p = h.history(map[string]any{"chat_id": alice.ID, "limit": 10, "around": all[30].ID + 1})
	if len(p.Messages) < 10 {
		t.Errorf("around a gap: %d messages", len(p.Messages))
	}

	// What the encrypted dm holds.
	var album []proto.Message
	var reply *proto.Message
	newestPage := h.history(map[string]any{"chat_id": alice.ID, "limit": 20}).Messages
	for i, m := range all {
		if m.Album != 0 {
			album = append(album, m)
		}
		if m.ReplyTo != nil && m.ReplyTo.MessageID < newestPage[0].ID && reply == nil {
			reply = &all[i]
		}
	}
	if len(album) != 3 || album[0].ID != album[0].Album || album[1].ID != album[0].ID+1 || album[2].ID != album[0].ID+2 {
		t.Fatalf("album: %+v", album)
	}
	if album[2].Text == "" || album[0].Text != "" || len(album[0].Reactions) == 0 || len(album[2].Reactions) != 0 {
		t.Errorf("album fields on the wrong parts: %+v", album)
	}
	if reply == nil {
		t.Fatal("no reply to a message older than the newest page")
	}
	got := wiretest.Decode[struct {
		Message proto.Message `json:"message"`
	}](t, h.ok("get_message", map[string]any{"chat_id": alice.ID, "message_id": reply.ReplyTo.MessageID}))
	if got.Message.ID != reply.ReplyTo.MessageID || got.Message.SenderID != reply.ReplyTo.SenderID {
		t.Errorf("get_message of the answered message: %+v", got.Message)
	}
	var edited, file, bold, mine, link bool
	for _, m := range all {
		edited = edited || m.Edited
		file = file || m.Media != nil && m.Media.Kind == proto.FileMedia && m.Media.Name == "trip-plan.pdf"
		for _, e := range m.Entities {
			bold = bold || e.Type == proto.Bold
			link = link || e.Type == proto.Link && e.URL != ""
		}
		for _, r := range m.Reactions {
			mine = mine || r.Mine
		}
	}
	if !edited || !file || !bold || !mine || !link {
		t.Errorf("edited %v, file %v, bold %v, mine %v, link %v", edited, file, bold, mine, link)
	}
}

func TestFakeContentCoversWhatTheUIDraws(t *testing.T) {
	quickLinks(t, 50*time.Millisecond)
	h := startHelper(t, t.TempDir())
	h.login(proto.Messenger)
	h.login(proto.Instagram)
	h.link(proto.WhatsApp)
	found := map[string]bool{}
	onWhatsApp := map[string]bool{}
	for _, c := range h.loadChats("", 20) {
		for _, m := range h.everything(c.ID, 50) {
			if c.Network == proto.WhatsApp {
				whatsAppHas(onWhatsApp, m)
			}
			if m.Service != "" {
				found["service"] = true
				if m.Text != "" {
					t.Errorf("service message with text: %+v", m)
				}
			}
			if m.LinkPreview != nil && m.LinkPreview.Image != nil && c.Network == proto.Instagram {
				found["link preview"] = true
			}
			if m.Unsupported == "[Poll]" {
				found["poll"] = true
			}
			if m.Forwarded {
				found["forwarded"] = true
			}
			if strings.Contains(m.Text, "unsent") || strings.Contains(m.Service, "unsent") {
				found["unsent"] = true
			}
			if strings.ContainsAny(m.Text, "🎉🌄") {
				found["emoji"] = true
			}
			if len(m.Text) > 200 {
				found["long"] = true
			}
			if md := m.Media; md != nil {
				switch {
				case md.Kind == proto.Sticker:
					found["sticker"] = true
				case md.Kind == proto.Video && md.Thumbnail != nil && md.Duration > 0:
					found["video"] = true
				case md.Kind == proto.Voice && md.Mime == "audio/mp4":
					found["voice"] = true
				case md.ViewOnce && md.FileID == 0:
					found["view once"] = true
				case md.Kind == proto.Photo && md.Height > md.Width:
					found["tall photo"] = true
				case md.Kind == proto.Photo && md.Width > md.Height:
					found["wide photo"] = true
				}
			}
			for _, e := range m.Entities {
				found[string(e.Type)] = true
			}
		}
	}
	for _, want := range []string{"service", "link preview", "poll", "forwarded", "unsent", "emoji", "long",
		"sticker", "video", "voice", "view once", "tall photo", "wide photo",
		"bold", "italic", "code", "strike", "pre", "quote", "link", "mention"} {
		if !found[want] {
			t.Errorf("no %s", want)
		}
	}
	for _, want := range []string{"service", "poll", "location", "forwarded", "album", "file", "sticker", "video",
		"voice", "view once", "edited", "mine", "rtl", "bold", "italic", "code", "strike", "pre", "quote", "link", "mention"} {
		if !onWhatsApp[want] {
			t.Errorf("no %s on WhatsApp", want)
		}
	}
}

// whatsAppHas notes what kinds of content m is.
func whatsAppHas(found map[string]bool, m proto.Message) {
	note := func(on bool, what string) {
		if on {
			found[what] = true
		}
	}
	note(m.Service != "", "service")
	note(m.Unsupported == "[Poll]", "poll")
	note(m.Unsupported == "[Location]", "location")
	note(m.Forwarded, "forwarded")
	note(m.Album != 0, "album")
	note(m.Edited, "edited")
	note(strings.ContainsRune(m.Text, 'ع'), "rtl")
	if md := m.Media; md != nil {
		note(md.Kind == proto.FileMedia && md.Mime == "application/pdf", "file")
		note(md.Kind == proto.Sticker, "sticker")
		note(md.Kind == proto.Video && md.Thumbnail != nil, "video")
		note(md.Kind == proto.Voice, "voice")
		note(md.ViewOnce && md.FileID == 0, "view once")
	}
	for _, r := range m.Reactions {
		note(r.Mine, "mine")
	}
	for _, e := range m.Entities {
		found[string(e.Type)] = true
	}
}

func testPNG(w, h int) []byte {
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func chatEvent(t *testing.T, l wiretest.Line) proto.Chat {
	return wiretest.Field[proto.Chat](t, l, "chat")
}

func TestFakeSendGoesPendingSentTypingEcho(t *testing.T) {
	h := startHelper(t, t.TempDir())
	h.login(proto.Messenger)
	alice := h.loadChats(proto.Messenger, 10)[0]
	self := h.c.Event("account", 0, func(l wiretest.Line) bool {
		return wiretest.Decode[proto.AccountEvent](t, json.RawMessage(l.Raw)).UserID > 0
	})
	selfID := wiretest.Field[int64](t, self, "user_id")

	from := h.c.Mark()
	began := time.Now()
	id := h.c.Request("send_text", map[string]any{"chat_id": alice.ID, "text": "hi there", "reply_to": alice.LastMessage.ID})
	resp := h.c.Response(id)
	temp := wiretest.Decode[struct {
		ID int64 `json:"message_id"`
	}](t, resp.Result).ID
	pending := h.c.Event("message", from, nil)
	pm := wiretest.Field[proto.Message](t, pending, "message")
	if pending.Seq > resp.Seq || pm.ID != temp || pm.State != proto.Pending || !pm.Outgoing || pm.SenderID != selfID ||
		pm.Text != "hi there" || pm.ReplyTo == nil || pm.ReplyTo.MessageID != alice.LastMessage.ID || pm.ID <= alice.LastMessage.ID {
		t.Fatalf("pending message (line %d, response %d): %s", pending.Seq, resp.Seq, pending.Raw)
	}

	sent := h.c.Event("message_sent", from, nil)
	if d := time.Since(began); d < 250*time.Millisecond {
		t.Errorf("sent after %v", d)
	}
	if old := wiretest.Field[int64](t, sent, "old_id"); old != temp {
		t.Errorf("old_id %d, temp %d", old, temp)
	}
	final := wiretest.Field[proto.Message](t, sent, "message")
	if final.State != proto.Sent || final.Text != "hi there" || final.ID == temp || final.EditableUntil == 0 || !final.Deletable {
		t.Errorf("final: %+v", final)
	}

	read := h.c.Event("read", sent.Seq, nil)
	if out := wiretest.Field[int64](t, read, "outbox"); out != final.ID {
		t.Errorf("read outbox %d", out)
	}
	typing := h.c.Event("typing", sent.Seq, nil)
	if !wiretest.Field[bool](t, typing, "typing") || wiretest.Field[int64](t, typing, "user_id") != alice.UserID {
		t.Errorf("typing: %s", typing.Raw)
	}
	echo := h.c.Event("message", typing.Seq, func(l wiretest.Line) bool {
		return wiretest.Field[proto.Message](t, l, "message").Text == "echo: hi there"
	})
	stop := h.c.Event("typing", typing.Seq+1, nil)
	if wiretest.Field[bool](t, stop, "typing") || stop.Seq > echo.Seq {
		t.Errorf("typing didn't stop before the echo: %s", stop.Raw)
	}
	if d := time.Since(began); d < 2*time.Second {
		t.Errorf("echo after %v", d)
	}
	em := wiretest.Field[proto.Message](t, echo, "message")
	if em.Outgoing || em.SenderID != alice.UserID || em.ID <= final.ID {
		t.Errorf("echo: %+v", em)
	}
	c := chatEvent(t, h.c.Event("chat", echo.Seq, nil))
	if c.LastMessage.ID != em.ID || c.Unread != alice.Unread+1 {
		t.Errorf("chat after the echo: unread %d, last %d", c.Unread, c.LastMessage.ID)
	}
}

func TestFakeSendFilesIsAnAlbumWithTheCaptionLast(t *testing.T) {
	dir := t.TempDir()
	h := startHelper(t, filepath.Join(dir, "data"))
	h.login(proto.Instagram)
	chats := h.loadChats(proto.Instagram, 10)
	var group proto.Chat
	for _, c := range chats {
		if c.Kind == proto.Group {
			group = c
		}
	}
	var paths []string
	for i, data := range [][]byte{testPNG(40, 30), []byte("just some notes\n")} {
		p := filepath.Join(dir, []string{"pic.png", "notes.txt"}[i])
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	from := h.c.Mark()
	resp := h.c.Call("send_files", map[string]any{"chat_id": group.ID, "paths": paths, "caption": "two things"})
	temps := wiretest.Decode[struct {
		IDs []int64 `json:"message_ids"`
	}](t, resp.Result).IDs
	if len(temps) != 2 || temps[1] != temps[0]+1 {
		t.Fatalf("temps %v (%s)", temps, resp.Raw)
	}
	var pending []proto.Message
	for _, l := range h.c.Lines()[from:resp.Seq] {
		if l.Event == "message" {
			pending = append(pending, wiretest.Field[proto.Message](t, l, "message"))
		}
	}
	if len(pending) != 2 || pending[0].Album != temps[0] || pending[1].Text != "two things" || pending[0].Text != "" {
		t.Fatalf("pending: %+v", pending)
	}
	if m := pending[0].Media; m == nil || m.Kind != proto.Photo || m.Width != 40 || m.Name != "pic.png" || m.FileID != 0 {
		t.Errorf("pending photo: %+v", m)
	}
	if m := pending[1].Media; m == nil || m.Kind != proto.FileMedia || m.Mime != "text/plain" || m.Size != 16 {
		t.Errorf("pending file: %+v", m)
	}
	for _, temp := range temps {
		sent := h.c.Event("message_sent", from, func(l wiretest.Line) bool { return wiretest.Field[int64](t, l, "old_id") == temp })
		if m := wiretest.Field[proto.Message](t, sent, "message"); m.Media == nil || m.Media.FileID == 0 || m.State != proto.Sent {
			t.Errorf("sent part: %s", sent.Raw)
		}
	}
	// Relative paths and missing files are refused.
	if l := h.c.Call("send_files", map[string]any{"chat_id": group.ID, "paths": []string{"pic.png"}}); l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Errorf("relative path: %s", l.Raw)
	}
	if l := h.c.Call("send_files", map[string]any{"chat_id": group.ID, "paths": []string{filepath.Join(dir, "gone.png")}}); l.Error == nil || l.Error.Code != proto.NotFound {
		t.Errorf("missing file: %s", l.Raw)
	}
}

func TestFakeReadReactEditDeleteMute(t *testing.T) {
	h := startHelper(t, t.TempDir())
	h.login(proto.Messenger)
	alice := h.loadChats(proto.Messenger, 10)[0]
	if alice.Unread == 0 {
		t.Fatal("alice has no unread messages")
	}

	from := h.c.Mark()
	h.ok("mark_read", map[string]any{"chat_id": alice.ID, "message_id": alice.LastMessage.ID})
	read := h.c.Event("read", from, nil)
	if wiretest.Field[int64](t, read, "inbox") != alice.LastMessage.ID || wiretest.Field[int](t, read, "unread") != 0 {
		t.Errorf("read: %s", read.Raw)
	}

	target := alice.LastMessage.ID
	from = h.c.Mark()
	h.ok("react", map[string]any{"chat_id": alice.ID, "message_id": target, "emoji": "❤️"})
	m := wiretest.Field[proto.Message](t, h.c.Event("message", from, nil), "message")
	if m.ID != target || len(m.Reactions) != 1 || !m.Reactions[0].Mine || m.Reactions[0].Emoji != "❤️" {
		t.Errorf("after react: %+v", m.Reactions)
	}
	from = h.c.Mark()
	h.ok("react", map[string]any{"chat_id": alice.ID, "message_id": target, "emoji": nil})
	m = wiretest.Field[proto.Message](t, h.c.Event("message", from, nil), "message")
	if len(m.Reactions) != 0 {
		t.Errorf("after removing: %+v", m.Reactions)
	}

	// Old messages can't be edited; one just sent can.
	all := h.everything(alice.ID, 100)
	var oldOwn proto.Message
	for _, m := range all {
		if m.Outgoing && m.Media == nil && m.Service == "" {
			oldOwn = m
		}
	}
	if l := h.c.Call("edit_text", map[string]any{"chat_id": alice.ID, "message_id": oldOwn.ID, "text": "x"}); l.Error == nil || l.Error.Code != proto.Unsupported {
		t.Errorf("editing an old message: %s", l.Raw)
	}
	if l := h.c.Call("delete", map[string]any{"chat_id": alice.ID, "message_id": alice.LastMessage.ID}); l.Error == nil {
		t.Errorf("deleting someone else's message: %s", l.Raw)
	}
	from = h.c.Mark()
	h.ok("send_text", map[string]any{"chat_id": alice.ID, "text": "typo hree"})
	sent := wiretest.Field[proto.Message](t, h.c.Event("message_sent", from, nil), "message")
	from = h.c.Mark()
	h.ok("edit_text", map[string]any{"chat_id": alice.ID, "message_id": sent.ID, "text": "typo here"})
	edited := wiretest.Field[proto.Message](t, h.c.Event("message", from, nil), "message")
	if edited.ID != sent.ID || !edited.Edited || edited.Text != "typo here" {
		t.Errorf("edited: %+v", edited)
	}
	from = h.c.Mark()
	h.ok("delete", map[string]any{"chat_id": alice.ID, "message_id": sent.ID})
	del := h.c.Event("message_deleted", from, nil)
	if ids := wiretest.Field[[]int64](t, del, "message_ids"); len(ids) != 1 || ids[0] != sent.ID {
		t.Errorf("deleted: %s", del.Raw)
	}
	if l := h.c.Call("get_message", map[string]any{"chat_id": alice.ID, "message_id": sent.ID}); l.Error == nil || l.Error.Code != proto.NotFound {
		t.Errorf("deleted message still there: %s", l.Raw)
	}

	from = h.c.Mark()
	h.ok("mute", map[string]any{"chat_id": alice.ID, "muted": true})
	if c := chatEvent(t, h.c.Event("chat", from, nil)); !c.Muted || c.ID != alice.ID {
		t.Errorf("muted: %+v", c)
	}
	h.ok("typing", map[string]any{"chat_id": alice.ID, "typing": true})
}

func TestFakeDownloadWritesAPrivatePNG(t *testing.T) {
	dir := t.TempDir()
	h := startHelper(t, dir)
	h.login(proto.Messenger)
	alice := h.loadChats(proto.Messenger, 10)[0]
	if alice.Photo == nil {
		t.Fatal("alice has no photo")
	}
	from := h.c.Mark()
	h.ok("download", map[string]any{"file_id": alice.Photo.FileID, "priority": "high"})
	done := h.c.Event("file", from, func(l wiretest.Line) bool { return wiretest.Field[proto.File](t, l, "file").Done })
	f := wiretest.Field[proto.File](t, done, "file")
	progress := 0
	for _, l := range h.c.Lines()[from:done.Seq] {
		if l.Event == "file" {
			progress++
		}
	}
	if progress < 3 {
		t.Errorf("only %d progress events", progress)
	}
	if f.Path == nil || !strings.HasPrefix(*f.Path, filepath.Join(dir, "fake")) || f.Size != f.Downloaded || f.Size == 0 {
		t.Fatalf("done: %s", done.Raw)
	}
	info, err := os.Stat(*f.Path)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != f.Size {
		t.Fatalf("file on disk: %v %v", info, err)
	}
	data, _ := os.ReadFile(*f.Path)
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != alice.Photo.Width || img.Bounds().Dy() != alice.Photo.Height {
		t.Fatalf("not the PNG promised: %v", err)
	}

	// Already on disk: done at once.
	from = h.c.Mark()
	h.ok("download", map[string]any{"file_id": alice.Photo.FileID, "priority": "low"})
	again := h.c.Event("file", from, nil)
	if g := wiretest.Field[proto.File](t, again, "file"); !g.Done || *g.Path != *f.Path {
		t.Errorf("second download: %s", again.Raw)
	}
	if l := h.c.Call("download", map[string]any{"file_id": 99999, "priority": "high"}); l.Error == nil || l.Error.Code != proto.NotFound {
		t.Errorf("unknown file: %s", l.Raw)
	}
}

func TestFakeLogoutForgetsTheNetwork(t *testing.T) {
	dir := t.TempDir()
	h := startHelper(t, dir)
	h.login(proto.Messenger)
	chats := h.loadChats(proto.Messenger, 10)
	h.ok("download", map[string]any{"file_id": chats[0].Photo.FileID, "priority": "high"})
	h.c.Event("file", 0, func(l wiretest.Line) bool { return wiretest.Field[proto.File](t, l, "file").Done })

	from := h.c.Mark()
	h.ok("logout", map[string]any{"network": "messenger"})
	h.c.Event("account", from, func(l wiretest.Line) bool { return wiretest.Field[string](t, l, "state") == "logged_out" })
	if _, err := os.Stat(filepath.Join(dir, "fake", "messenger")); !os.IsNotExist(err) {
		t.Errorf("downloads kept after logout: %v", err)
	}
	if l := h.c.Call("history", map[string]any{"chat_id": chats[0].ID, "limit": 5}); l.Error == nil {
		t.Errorf("history after logout: %s", l.Raw)
	}
	// Logging in again works, with new chat ids.
	h.login(proto.Messenger)
	again := h.loadChats(proto.Messenger, 10)
	if len(again) != 6 || again[0].ID == chats[0].ID {
		t.Errorf("after logging in again: %d chats, first id %d (was %d)", len(again), again[0].ID, chats[0].ID)
	}
}

// snapshot is what a fresh run shows: chat ids, titles, and one chat's
// message ids and texts.
func snapshot(t *testing.T, dir string) string {
	h := startHelper(t, dir)
	h.login(proto.Messenger)
	h.login(proto.Instagram)
	h.link(proto.WhatsApp)
	var b strings.Builder
	for _, c := range h.loadChats("", 20) {
		b.WriteString(c.Title)
		data, _ := json.Marshal([]any{c.ID, c.Order, c.Unread, c.Photo})
		b.Write(data)
		for _, m := range h.everything(c.ID, 100) {
			data, _ := json.Marshal([]any{m.ID, m.SenderID, m.Text, m.Media, m.Reactions})
			b.Write(data)
		}
	}
	h.stdin.Close()
	<-h.done
	return b.String()
}

func TestFakeRunsAreTheSameEveryTime(t *testing.T) {
	quickLinks(t, 50*time.Millisecond)
	hour := time.Now().Truncate(time.Hour)
	a := snapshot(t, t.TempDir())
	b := snapshot(t, t.TempDir())
	if !time.Now().Truncate(time.Hour).Equal(hour) {
		t.Skip("the hour turned during the test")
	}
	if a != b {
		t.Error("two fresh runs differ")
	}
	// The same data dir keeps the same ids.
	dir := t.TempDir()
	if snapshot(t, dir) != snapshot(t, dir) {
		t.Error("a restart changed what's shown")
	}
}

// TestTheBinaryStartsQuietlyAndQuitsOnStdinEOF runs the helper as its own
// process, as tuimeta does.
func TestTheBinaryStartsQuietlyAndQuitsOnStdinEOF(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "helper")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "TUIMETA_HELPER_TEST_AS_MAIN=1", "TUIMETA_HELPER_TEST_ARGS=--data-dir "+dir+" --fake")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := wiretest.New(t, stdin, stdout)
	first := c.Wait("hello", 0, func(wiretest.Line) bool { return true })
	if first.Event != "hello" {
		t.Fatalf("first line: %s", first.Raw)
	}
	l := c.Call("login_cookies", map[string]any{"network": "messenger", "cookies": "c_user=SECRETCUSER; xs=SECRETXS; datr=SECRETDATR"})
	if l.Error != nil {
		t.Fatalf("login: %s", l.Raw)
	}
	c.Call("load_chats", map[string]any{"network": "messenger", "limit": 50})
	c.Call("send_text", map[string]any{"chat_id": 999999, "text": "SECRETTEXT"})

	began := time.Now()
	stdin.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		cmd.Process.Kill()
		t.Fatal("still running 2 s after stdin closed")
	}
	t.Logf("quit %v after stdin closed", time.Since(began))
	if stderr.Len() > 0 {
		t.Errorf("stderr: %q", stderr.String())
	}
	for _, l := range c.Lines() {
		if l.Event == "!unparsable" {
			t.Errorf("not a protocol line: %q", l.Raw)
		}
	}
	modes := map[string]os.FileMode{"": 0o700, "helper.log": 0o600, "ids.json": 0o600}
	for name, want := range modes {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", name, info.Mode(), err, want)
		}
	}
	log, _ := os.ReadFile(filepath.Join(dir, "helper.log"))
	for _, secret := range []string{"SECRET", "Alice", "Robin", "echo"} {
		if bytes.Contains(log, []byte(secret)) {
			t.Errorf("helper.log mentions %q:\n%s", secret, log)
		}
	}
}

func TestBadArgumentsAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--fake"},
		{"--data-dir", "relative/dir"},
		{"--data-dir", "/tmp/x", "extra"},
		{"--nope"},
	} {
		if code := run(args, strings.NewReader(""), io.Discard, io.Discard, nil); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"--data-dir", link}, strings.NewReader(""), io.Discard, io.Discard, nil); code != 1 {
		t.Errorf("symlinked data dir: exit %d", code)
	}
	// An existing folder gets its mode fixed.
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"--data-dir", loose, "--fake"}, strings.NewReader(""), io.Discard, io.Discard, nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if info, _ := os.Stat(loose); info.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", info.Mode())
	}
}
