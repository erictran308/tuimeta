// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"

	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// waitFor polls until ok, failing the test after a second.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func accountStates(r *recorder) []string {
	var out []string
	for _, e := range r.events("account") {
		s := e["state"].(string)
		if msg, ok := e["error"].(string); ok {
			s += ": " + msg
		}
		out = append(out, s)
	}
	return out
}

func TestLoginConnectsThenSavesOnlyTheInstagramCookies(t *testing.T) {
	h := newHarness(t)
	h.api.inbox = mailbox(t, "", false, dmThread(textMsg("mid.1", mayaFBID, tsBase, "hi")))
	set, err := cookies.Parse(proto.Instagram, cookieText)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.b.LoginCookies(t.Context(), set); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := accountStates(h.rec); strings.Join(got, ",") != "connecting,ready" {
		t.Errorf("account states = %v", got)
	}
	ready := h.rec.events("account")[1]
	if ready["user_id"].(float64) != float64(h.userID(selfFBID)) || ready["name"] != "Robin Hale" {
		t.Errorf("ready = %v", ready)
	}
	if names := h.api.names(); len(names) < 2 || names[0] != "LoadIndex" || names[1] != "Connect" {
		t.Errorf("requests = %v", names)
	}
	// instameow got the cookies it uses, not the stray one.
	if h.api.cookies.Get("sessionid") != "s3ss10n" || h.api.cookies.Get("mid") != "mid1" || h.api.cookies.Get("other") != "" {
		t.Errorf("cookies given to instameow: %v", h.api.cookies.GetAll())
	}
	path := filepath.Join(h.dir, "instagram", sessionFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("session file: %v %v", info, err)
	}
	var saved savedSession
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &saved); err != nil || saved.Cookies["sessionid"] != "s3ss10n" || saved.Cookies["other"] != "" {
		t.Errorf("saved = %+v (%v)", saved, err)
	}
	// Nothing of the account reached tuimeta before ready.
	if strings.Contains(h.rec.all(), "s3ss10n") {
		t.Error("a cookie reached the wire")
	}
}

func TestALoginWhoseSocketIsRefusedSavesNothing(t *testing.T) {
	h := newHarness(t)
	h.api.onConnect = []slidetypes.ClientEvent{&slidetypes.Disconnected{Error: websocket.CloseError{Code: dgw.CloseStatusUnauthorized}}}
	set, _ := cookies.Parse(proto.Instagram, cookieText)
	err := h.b.LoginCookies(t.Context(), set)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.BadCookies {
		t.Fatalf("login error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "instagram", sessionFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a session was saved: %v", err)
	}
	if got := accountStates(h.rec); strings.Join(got, ",") != "connecting,logged_out" {
		t.Errorf("account states = %v", got)
	}
}

func TestLoginErrorsSayWhatToDoWithoutQuotingInstagram(t *testing.T) {
	secret := "https://www.instagram.com/challenge/?token=SECRET-TOKEN"
	cases := []struct {
		err           error
		authenticated bool
		code          proto.Code
		says          string
	}{
		{httpclient.ErrTokenInvalidated, true, proto.BadCookies, "copy them again"},
		{httpclient.RedirectedError{Type: httpclient.ErrCheckpointRequired, URL: secret}, true, proto.Checkpoint, "Instagram wants you to confirm it's you: open instagram.com in a browser or the Instagram app"},
		{httpclient.RedirectedError{Type: httpclient.ErrChallengeRequired, URL: secret}, true, proto.Checkpoint, "confirm it's you"},
		{httpclient.RedirectedError{Type: httpclient.ErrConsentRequired, URL: secret}, true, proto.Checkpoint, "accept its terms"},
		{httpclient.RedirectedError{Type: httpclient.ErrAccountSuspended, URL: secret}, true, proto.Checkpoint, "suspended"},
		{fmt.Errorf("%w: %s", httpclient.ErrRateLimited, secret), true, proto.NetworkError, "limiting requests"},
		{fmt.Errorf("%w: dial tcp: %s", httpclient.ErrRequestFailed, secret), true, proto.NetworkError, "check your connection"},
		{fmt.Errorf("graphql: %s", secret), false, proto.BadCookies, "didn't accept these cookies"},
	}
	for _, tc := range cases {
		h := newHarness(t)
		h.api.loadErr = tc.err
		h.api.authenticated = tc.authenticated
		set, _ := cookies.Parse(proto.Instagram, cookieText)
		err := h.b.LoginCookies(t.Context(), set)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != tc.code || !strings.Contains(pe.Message, tc.says) {
			t.Errorf("%v: got %v", tc.err, err)
			continue
		}
		if strings.Contains(pe.Message, "SECRET") || strings.Contains(h.rec.all(), "SECRET") {
			t.Errorf("%v: Instagram's text reached tuimeta", tc.err)
		}
	}
	// An account Instagram doesn't name is refused.
	h := newHarness(t)
	h.api.fbid = 0
	set, _ := cookies.Parse(proto.Instagram, cookieText)
	if err := h.b.LoginCookies(t.Context(), set); !errors.Is(err, errNoAccount) {
		t.Errorf("no account: %v", err)
	}
}

func TestASavedSessionResumesAtStart(t *testing.T) {
	h := newHarness(t)
	h.login(mailbox(t, "", false))
	dir := h.dir
	h.b.Close()

	h2 := newHarnessIn(t, dir)
	h2.api.inbox = mailbox(t, "", false)
	h2.b.Start(t.Context())
	waitFor(t, "ready", func() bool { return len(h2.rec.events("account")) >= 2 })
	if got := accountStates(h2.rec); strings.Join(got, ",") != "connecting,ready" {
		t.Errorf("account states = %v", got)
	}
	if h2.api.cookies.Get("sessionid") != "s3ss10n" {
		t.Errorf("resumed with %v", h2.api.cookies.GetAll())
	}
}

func TestStartWithoutASessionIsLoggedOut(t *testing.T) {
	h := newHarness(t)
	h.b.Start(t.Context())
	if got := accountStates(h.rec); strings.Join(got, ",") != "logged_out" {
		t.Errorf("account states = %v", got)
	}
	if len(h.api.names()) != 0 {
		t.Errorf("requests without a session: %v", h.api.names())
	}
}

func TestASessionInstagramEndsBecomesAnErrorAndIsForgotten(t *testing.T) {
	h := newHarness(t)
	h.login(mailbox(t, "", false))
	_ = h.api.emit(&slidetypes.AuthError{Error: fmt.Errorf("%w: sessionid cookie was deleted", httpclient.ErrTokenInvalidated)})
	got := accountStates(h.rec)
	if len(got) != 1 || !strings.HasPrefix(got[0], "error: Instagram ended this session") {
		t.Errorf("account states = %v", got)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "instagram", sessionFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dead session was kept: %v", err)
	}
	if _, err := h.b.LoadChats(t.Context(), 10); err == nil {
		t.Error("requests still go through a dead session")
	}
}

func TestACheckpointLaterSaysToConfirmInABrowserOrTheApp(t *testing.T) {
	h := newHarness(t)
	h.login(mailbox(t, "", false))
	_ = h.api.emit(&slidetypes.AuthError{Error: httpclient.RedirectedError{Type: httpclient.ErrCheckpointRequired, URL: "https://x/?t=SECRET"}})
	got := accountStates(h.rec)
	want := "error: Instagram wants you to confirm it's you: open instagram.com in a browser or the Instagram app, then log in here again."
	if len(got) != 1 || got[0] != want {
		t.Errorf("account states = %v", got)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "instagram", sessionFile)); err != nil {
		t.Errorf("a checkpoint shouldn't delete the session: %v", err)
	}
}

func TestLogoutForgetsEverything(t *testing.T) {
	h := newHarness(t)
	h.login(mailbox(t, "", false, dmThread()))
	if err := h.b.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "instagram")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session folder kept: %v", err)
	}
	if got := accountStates(h.rec); strings.Join(got, ",") != "logged_out" {
		t.Errorf("account states = %v", got)
	}
	if len(h.b.chats) != 0 {
		t.Error("chats kept after logout")
	}
}

func chatEvents(r *recorder) []map[string]any {
	var out []map[string]any
	for _, e := range r.events("chat") {
		out = append(out, e["chat"].(map[string]any))
	}
	return out
}

func TestLoadChatsPagesThroughTheInboxNewestFirst(t *testing.T) {
	h := newHarness(t)
	older := thread(reqIGID, fmt.Sprint(dealsFBID), reqLong, false, "", []string{user(dealsFBID, "34000000007", "deals.4.you", "Promo Deals")},
		tsBase-3_600_000, []string{textMsg("mid.d", dealsFBID, tsBase-3_600_000, "Hi! Quick question")}, "", false, "")
	older = strings.Replace(older, `"system_folder":"INBOX"`, `"system_folder":"PENDING"`, 1)
	h.api.inboxPages["page2"] = decode[*slidetypes.MailboxPage](t, `{"threads_by_folder":{"edges":[{"node":{"as_ig_direct_thread":`+older+`}}],"page_info":{"has_next_page":false}}}`)
	h.login(mailbox(t, "page2", true, groupThread(), dmThread(textMsg("mid.1", mayaFBID, tsBase, "Did you see it? 👆"))))

	more, err := h.b.LoadChats(t.Context(), 1)
	if err != nil || !more {
		t.Fatalf("first load: %v %v", more, err)
	}
	chats := chatEvents(h.rec)
	if len(chats) != 1 || chats[0]["id"].(float64) != float64(h.chatID(mayaFBID)) {
		t.Fatalf("first page: %v", chats)
	}
	dm := chats[0]
	if dm["kind"] != "dm" || dm["title"] != "Maya Lopez" || dm["user_id"].(float64) != float64(h.userID(mayaFBID)) || dm["unread"].(float64) != 1 {
		t.Errorf("dm = %v", dm)
	}
	if lm := dm["last_message"].(map[string]any); lm["text"] != "Did you see it? 👆" {
		t.Errorf("last message = %v", lm)
	}
	// People come before the chats that name them.
	lines := h.rec.lines
	firstChat := -1
	for i, l := range lines {
		if l["event"] == "chat" && firstChat < 0 {
			firstChat = i
		}
		if l["event"] == "user" && firstChat >= 0 {
			t.Errorf("a user event came after a chat: %v", l)
		}
	}
	if len(h.api.names()) != 2 {
		t.Errorf("the first page needed no request: %v", h.api.names())
	}

	h.rec.reset()
	more, err = h.b.LoadChats(t.Context(), 5)
	if err != nil || more {
		t.Fatalf("second load: %v %v", more, err)
	}
	chats = chatEvents(h.rec)
	if len(chats) != 2 || chats[0]["title"] != "Climbing crew 🧗" || chats[1]["title"] != "Promo Deals" {
		t.Fatalf("second page: %v", chats)
	}
	if chats[0]["kind"] != "group" || chats[1]["request"] != true {
		t.Errorf("chats = %v", chats)
	}
	req := h.api.last("PaginateMailbox").(*slidetypes.PaginateMailboxRequest)
	if req.Cursor != "page2" || req.Folder != "INBOX" || req.ViewerFBID != selfFBID {
		t.Errorf("inbox request = %+v", req)
	}
	// tuimeta never sees Instagram's own ids.
	all := h.rec.all()
	for _, id := range []string{dmIGID, dmLong, groupLong, reqIGID, mayaIGID, `"2002"`, "mid.1"} {
		if strings.Contains(all, id) {
			t.Errorf("Instagram's id %s reached tuimeta", id)
		}
	}
}

func TestChatsAndPeopleKeepTheirIDsAcrossRuns(t *testing.T) {
	h := newHarness(t)
	h.login(mailbox(t, "", false, dmThread(), groupThread()))
	if _, err := h.b.LoadChats(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	chat, person := h.chatID(mayaFBID), h.userID(mayaFBID)
	if err := h.deps.IDs.Flush(); err != nil {
		t.Fatal(err)
	}
	h.b.Close()
	h2 := newHarnessIn(t, h.dir)
	if h2.chatID(mayaFBID) != chat || h2.userID(mayaFBID) != person || h2.chatID(groupKey) == chat {
		t.Errorf("ids changed: chat %d→%d, person %d→%d", chat, h2.chatID(mayaFBID), person, h2.userID(mayaFBID))
	}
}

func TestHistoryFetchesTheNewestPageThenOlderOnesAndOnlyReads(t *testing.T) {
	h := newHarness(t)
	h.login(mailbox(t, "", false, dmThread(textMsg("mid.5", mayaFBID, tsBase, "five"))))
	h.api.threads[dmIGID] = threadInfo(t, thread(dmIGID, fmt.Sprint(mayaFBID), dmLong, false, "", []string{mayaJSON}, tsBase,
		[]string{textMsg("mid.5", mayaFBID, tsBase, "five"), textMsg("mid.4", selfFBID, tsBase-1000, "four"), textMsg("mid.3", mayaFBID, tsBase-2000, "three")},
		"c1", true, ""))
	h.api.pages["c1"] = decode[*slidetypes.MessagesOnlyThread](t, `{"id":"x","slide_messages":{"edges":[`+
		`{"node":`+textMsg("mid.2", selfFBID, tsBase-3000, "two")+`},{"node":`+textMsg("mid.1", mayaFBID, tsBase-4000, "one")+`}],
		"page_info":{"end_cursor":"c2","has_next_page":false}}}`)
	ref := h.chatRef(mayaFBID)

	page, err := h.b.History(t.Context(), ref, history.Query{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[0].Text != "four" || page.Messages[1].Text != "five" || !page.HasMore {
		t.Fatalf("newest page = %+v", page)
	}
	if h.api.count("GetThread") != 1 || h.api.count("PaginateMessages") != 0 {
		t.Errorf("requests = %v", h.api.names())
	}

	before := page.Messages[0].ID
	page, err = h.b.History(t.Context(), ref, history.Query{Before: &before, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 3 || page.Messages[0].Text != "one" || page.HasMore {
		t.Fatalf("older page = %+v", page)
	}
	req := h.api.last("PaginateMessages").(*slidetypes.PaginateMessagesRequest)
	if *req.AfterCursor != "c1" || req.ThreadID != dmIGID || req.FirstN != pageSize {
		t.Errorf("page request = %+v", req)
	}
	// Asking again needs nothing more from Instagram.
	n := len(h.api.names())
	if _, err := h.b.History(t.Context(), ref, history.Query{Limit: 50}); err != nil || len(h.api.names()) != n {
		t.Errorf("complete history asked again: %v", h.api.names()[n:])
	}
	noCalls(t, h.api, "MarkRead", "MarkReadValidation", "SetTyping")
}

func TestHistoryAroundAnOldPositionPagesBackUntilItIsCovered(t *testing.T) {
	h := newHarness(t)
	h.login(mailbox(t, "", false, dmThread()))
	h.api.threads[dmIGID] = threadInfo(t, thread(dmIGID, fmt.Sprint(mayaFBID), dmLong, false, "", []string{mayaJSON}, tsBase,
		[]string{textMsg("mid.9", mayaFBID, tsBase, "nine")}, "p1", true, ""))
	page := func(cursor, next string, more bool, msgs ...string) {
		edges := make([]string, len(msgs))
		for i, m := range msgs {
			edges[i] = `{"node":` + m + `}`
		}
		h.api.pages[cursor] = decode[*slidetypes.MessagesOnlyThread](t, fmt.Sprintf(`{"slide_messages":{"edges":[%s],"page_info":{"end_cursor":%q,"has_next_page":%v}}}`,
			strings.Join(edges, ","), next, more))
	}
	page("p1", "p2", true, textMsg("mid.8", mayaFBID, tsBase-1000, "eight"))
	page("p2", "p3", true, textMsg("mid.7", mayaFBID, tsBase-2000, "seven"))
	page("p3", "p4", true, textMsg("mid.6", mayaFBID, tsBase-3000, "six"), textMsg("mid.5", mayaFBID, tsBase-4000, "five"))
	page("p4", "", false, textMsg("mid.4", mayaFBID, tsBase-5000, "four"))

	around := ids6(tsBase - 3000)
	got, err := h.b.History(t.Context(), h.chatRef(mayaFBID), history.Query{Around: &around, Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range got.Messages {
		texts = append(texts, m.Text)
	}
	// Two before "six" took the fourth page; nothing further was asked.
	if strings.Join(texts, ",") != "four,five,six,seven" {
		t.Errorf("around = %v", texts)
	}
	if h.api.count("PaginateMessages") != 4 {
		t.Errorf("pages fetched: %v", h.api.names())
	}
}

func ids6(ms int64) int64 { return ms << 8 }

func TestASocketMessageInAChatTuimetaLacksSendsTheChatFirst(t *testing.T) {
	h := newHarness(t)
	h.login(mailbox(t, "", false))
	h.api.threads[groupKey] = threadInfo(t, groupThread())
	d := delta(t, "SlideUQPPNewMessage", groupKey, `"message":`+textMsg("mid.new", samFBID, tsBase+1000, "Who's in for Tuesday?"))
	_ = h.api.emit(d)
	var order []string
	for _, l := range h.rec.lines {
		order = append(order, l["event"].(string))
	}
	joined := strings.Join(order, ",")
	if !strings.Contains(joined, "chat,message") || strings.Index(joined, "user") > strings.Index(joined, "chat") {
		t.Errorf("events = %v", order)
	}
	msgs := h.rec.events("message")
	if len(msgs) != 1 || msgs[0]["message"].(map[string]any)["text"] != "Who's in for Tuesday?" {
		t.Errorf("messages = %v", msgs)
	}
	if h.api.count("GetThread") != 1 {
		t.Errorf("requests = %v", h.api.names())
	}
	// A removal for a thread nobody knows fetches nothing.
	_ = h.api.emit(delta(t, "SlideUQPPDeleteMessage", "340282366841710301244276017799999999", `"message_id":"mid.x"`))
	if h.api.count("GetThread") != 1 {
		t.Errorf("a removal fetched a thread: %v", h.api.names())
	}
	noCalls(t, h.api, "MarkRead", "MarkReadValidation", "SetTyping")
}

// loaded logs in with the dm and group, and sends them to tuimeta.
func loaded(t *testing.T, msgs ...string) *harness {
	h := newHarness(t)
	h.login(mailbox(t, "", false, dmThread(msgs...), groupThread()))
	if _, err := h.b.LoadChats(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	h.rec.reset()
	return h
}

func TestSocketUpdatesKeepChatsCurrent(t *testing.T) {
	h := loaded(t, textMsg("mid.1", mayaFBID, tsBase, "hi"))
	emit := func(typename, body string) { _ = h.api.emit(delta(t, typename, dmIGID, body)) }
	h.b.mu.Lock()
	h.b.putLog(h.b.chats[mayaFBID], h.b.chats[mayaFBID].msgs["mid.1"]) // as if tuimeta loaded it
	h.b.mu.Unlock()

	emit("SlideUQPPEditMessage", `"message_id":"mid.1","text_body":"hi there","slide_edit_history_entry":{"timestamp_ms":"1759912399999"}`)
	if m := h.rec.events("message"); len(m) != 1 || m[0]["message"].(map[string]any)["edited"] != true || m[0]["message"].(map[string]any)["text"] != "hi there" {
		t.Errorf("edit: %v", m)
	}
	h.rec.reset()
	emit("SlideUQPPCreateReaction", `"message_id":"mid.1","reaction":{"reaction":"🔥","sender_fbid":"2002","log_message_id":"mid.react"}`)
	emit("SlideUQPPDeleteReaction", `"message_id":"mid.react","reaction":{"log_message_id":"mid.react"}`)
	if m := h.rec.events("message"); len(m) != 2 || len(m[0]["message"].(map[string]any)["reactions"].([]any)) != 1 || len(m[1]["message"].(map[string]any)["reactions"].([]any)) != 0 {
		t.Errorf("reactions: %v", m)
	}
	h.rec.reset()
	emit("SlideUQPPReadReceipt", `"read_receipt":{"participant_fbid":"2002","watermark_timestamp_ms":"1759912345678"}`)
	reads := h.rec.events("read")
	if len(reads) != 1 || reads[0]["outbox"].(float64) != float64(pos(tsBase)) || reads[0]["inbox"] != nil {
		t.Errorf("their receipt: %v", reads)
	}
	h.rec.reset()
	emit("SlideUQPPMarkRead", `"read_timestamp_ms":"1759912345678"`)
	reads = h.rec.events("read")
	if len(reads) != 1 || reads[0]["inbox"].(float64) != float64(pos(tsBase)) || reads[0]["unread"].(float64) != 0 {
		t.Errorf("read elsewhere: %v", reads)
	}
	h.rec.reset()
	emit("SlideUQPPChangeMuteSettings", `"is_muted_now":true`)
	if c := chatEvents(h.rec); len(c) != 1 || c[0]["muted"] != true {
		t.Errorf("mute: %v", c)
	}
	h.rec.reset()
	emit("SlideUQPPDeleteMessage", `"message_id":"mid.1"`)
	if d := h.rec.events("message_deleted"); len(d) != 1 {
		t.Errorf("unsend: %v", d)
	}
	h.rec.reset()
	emit("SlideUQPPDeleteThread", "")
	if r := h.rec.events("chat_removed"); len(r) != 1 || r[0]["chat_id"].(float64) != float64(h.chatID(mayaFBID)) {
		t.Errorf("thread deleted: %v", r)
	}
	noCalls(t, h.api, "MarkRead", "MarkReadValidation", "SetTyping")
}

func TestTypingIsFoundByTheLongThreadIDAndTheInstagramUserID(t *testing.T) {
	h := loaded(t)
	_ = h.api.emit(&slidetypes.TypingNotification{ThreadID: dmLong, SenderID: 34000000002, ActivityStatus: 1})
	typing := h.rec.events("typing")
	if len(typing) != 1 || typing[0]["chat_id"].(float64) != float64(h.chatID(mayaFBID)) ||
		typing[0]["user_id"].(float64) != float64(h.userID(mayaFBID)) || typing[0]["typing"] != true {
		t.Errorf("typing = %v", typing)
	}
	_ = h.api.emit(&slidetypes.TypingNotification{ThreadID: dmLong, SenderID: 34000000002, ActivityStatus: 0})
	if typing = h.rec.events("typing"); len(typing) != 2 || typing[1]["typing"] != false {
		t.Errorf("stop = %v", typing)
	}
	// Your own typing elsewhere, and unknown threads, say nothing.
	_ = h.api.emit(&slidetypes.TypingNotification{ThreadID: dmLong, SenderID: 34000000001, ActivityStatus: 1})
	_ = h.api.emit(&slidetypes.TypingNotification{ThreadID: "1", SenderID: 34000000002, ActivityStatus: 1})
	if len(h.rec.events("typing")) != 2 {
		t.Errorf("typing = %v", h.rec.events("typing"))
	}
}

func TestReadReceiptsAndTypingGoOutOnlyWhenAsked(t *testing.T) {
	h := loaded(t, textMsg("mid.2", mayaFBID, tsBase, "two"), textMsg("mid.1", mayaFBID, tsBase-1000, "one"))
	ref := h.chatRef(mayaFBID)
	// Everything that reads: the chat list, history, a message, socket news.
	h.api.threads[dmIGID] = threadInfo(t, thread(dmIGID, fmt.Sprint(mayaFBID), dmLong, false, "", []string{mayaJSON}, tsBase,
		[]string{textMsg("mid.2", mayaFBID, tsBase, "two"), textMsg("mid.1", mayaFBID, tsBase-1000, "one")}, "", false, ""))
	if _, err := h.b.History(t.Context(), ref, history.Query{Limit: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.b.GetMessage(t.Context(), h.msgRef(mayaFBID, "mid.1")); err != nil {
		t.Fatal(err)
	}
	_ = h.api.emit(delta(t, "SlideUQPPNewMessage", dmIGID, `"message":`+textMsg("mid.3", mayaFBID, tsBase+1000, "three")))
	_ = h.api.emit(&slidetypes.TypingNotification{ThreadID: dmLong, SenderID: 34000000002, ActivityStatus: 1})
	_ = h.api.emit(&slidetypes.Connected{})
	noCalls(t, h.api, "MarkRead", "MarkReadValidation", "SetTyping", "AcceptMessageRequest")

	if err := h.b.MarkRead(t.Context(), h.msgRef(mayaFBID, "mid.3")); err != nil {
		t.Fatal(err)
	}
	if h.api.count("MarkRead") != 1 || h.api.count("MarkReadValidation") != 1 {
		t.Fatalf("requests = %v", h.api.names())
	}
	req := h.api.last("MarkRead").(*slidetypes.MarkReadRequest)
	if req.Metadata.IGThreadIGID != dmLong || req.Data.MessageID != "mid.3" || req.Data.ItemID == nil {
		t.Errorf("mark read = %+v", req)
	}
	val := h.api.last("MarkReadValidation").(*slidetypes.MarkReadRequest)
	if val.Data.MessageTimestampMS.UnixMilli() != tsBase+1000 {
		t.Errorf("validation = %+v", val)
	}
	if reads := h.rec.events("read"); len(reads) == 0 || reads[len(reads)-1]["unread"].(float64) != 0 {
		t.Errorf("reads = %v", reads)
	}
	// Already read: nothing more goes out.
	if err := h.b.MarkRead(t.Context(), h.msgRef(mayaFBID, "mid.2")); err != nil || h.api.count("MarkRead") != 1 {
		t.Errorf("an older message was marked again: %v %v", err, h.api.names())
	}

	if err := h.b.SetTyping(t.Context(), ref, true); err != nil {
		t.Fatal(err)
	}
	if got := h.api.last("SetTyping").([]any); got[0] != dmIGID || got[1] != true {
		t.Errorf("typing = %v", got)
	}
}

func TestInstameowsLoggerWritesNothing(t *testing.T) {
	// Even with a default context logger set somewhere (it isn't, in the
	// helper), what's given to instameow writes nothing.
	var buf strings.Builder
	loud := zerolog.New(&buf).Level(zerolog.TraceLevel)
	old := zerolog.DefaultContextLogger
	zerolog.DefaultContextLogger = &loud
	defer func() { zerolog.DefaultContextLogger = old }()

	l := quietLogger()
	l.Error().Str("url", "https://x/?token=SECRET").Msg("message text")
	derived := l.With().Str("socket", "main").Logger()
	derived.Warn().Msg("derived")
	ctx := withQuietLog(context.Background())
	zerolog.Ctx(ctx).Error().Msg("from a context")
	zerolog.Ctx(ctx).Trace().Msg("trace")
	if buf.Len() != 0 {
		t.Errorf("instameow's logs went somewhere: %q", buf.String())
	}
	if zerolog.Ctx(ctx).GetLevel() != zerolog.PanicLevel {
		t.Errorf("context logger level = %v", zerolog.Ctx(ctx).GetLevel())
	}
	// And the global logger parts of mautrix-meta use is silenced by New.
	_ = New(newHarness(t).deps)
	zlog.Error().Msg("global")
	if zlog.Logger.GetLevel() != zerolog.Disabled {
		t.Errorf("global logger level = %v", zlog.Logger.GetLevel())
	}
}
