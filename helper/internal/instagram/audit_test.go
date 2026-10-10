// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Checks for what a security audit found; everything here is made up, and
// nothing talks to Instagram.

func TestAMessageKeepsAtMost256MentionsAndGivesNoOneElseAnID(t *testing.T) {
	h := newHarness(t)
	h.convertOne(groupThread(), textMsg("mid.0", samFBID, tsBase, "hi")) // the chat and its people
	// 600 people mentioned, each range sent twice under another person, and
	// 400 ranges overlapping them: 1600 mentions naming 1600 people.
	var text strings.Builder
	var ms []string
	mention := func(offset, length, fbid int) {
		ms = append(ms, fmt.Sprintf(`{"offset":%d,"length":%d,"profile_range_type":"PROFILE","user_fbid":"%d"}`, offset, length, fbid))
	}
	for i := range 600 {
		text.WriteString("@x ")
		mention(3*i, 2, 900000+i)
	}
	for i := range 600 {
		mention(3*i, 2, 800000+i)
	}
	for i := range 400 {
		mention(3*i+1, 1, 700000+i)
	}
	msg := fmt.Sprintf(`{"id":"mid.many","sender_fbid":"%d","timestamp_ms":"%d","content":{"__typename":"SlideMessageText","text_body":%q},"mentions":[%s]}`,
		samFBID, tsBase+1, text.String(), strings.Join(ms, ","))

	before := h.deps.IDs.User(proto.Instagram, "probe-before")
	_, parts := h.convertOne(groupThread(), msg)
	if made := h.deps.IDs.User(proto.Instagram, "probe-after") - before - 1; made != maxMentions {
		t.Errorf("%d people were given ids, want %d", made, maxMentions)
	}
	var got []proto.Entity
	for _, e := range parts[0].Entities {
		if e.Type == proto.Mention {
			got = append(got, e)
		}
	}
	if parts[0].Text != text.String() || len(got) != maxMentions {
		t.Fatalf("%d mentions in %d bytes", len(got), len(parts[0].Text))
	}
	// The first in the text count, each range under the person first named.
	if got[0].UserID != h.userID(900000) || got[0].Offset != 0 || got[255].UserID != h.userID(900255) || got[255].Offset != 3*255 {
		t.Errorf("first %+v, last %+v", got[0], got[255])
	}
}

func TestAMessageRequestIsNeverMarkedRead(t *testing.T) {
	for _, folder := range []string{"PENDING", "SPAM"} {
		h := newHarness(t)
		req := strings.Replace(dmThread(textMsg("mid.r", mayaFBID, tsBase, "hi, it's Maya")), `"system_folder":"INBOX"`, `"system_folder":"`+folder+`"`, 1)
		h.login(mailbox(t, "", false, req))
		if _, err := h.b.LoadChats(t.Context(), 5); err != nil {
			t.Fatal(err)
		}
		h.rec.reset()
		if err := h.b.MarkRead(t.Context(), h.msgRef(mayaFBID, "mid.r")); err != nil {
			t.Errorf("%s: %v", folder, err)
		}
		noCalls(t, h.api, "MarkRead", "MarkReadValidation", "AcceptMessageRequest")
		if reads := h.rec.events("read"); len(reads) != 0 {
			t.Errorf("%s: the request was taken as read: %v", folder, reads)
		}
		// Accepted elsewhere, it's an ordinary chat, and reading it says so.
		_ = h.api.emit(delta(t, "SlideUQPPUpdateThreadFolder", dmIGID, `"folder":"INBOX"`))
		if err := h.b.MarkRead(t.Context(), h.msgRef(mayaFBID, "mid.r")); err != nil || h.api.count("MarkRead") != 1 {
			t.Errorf("%s, accepted: %v %v", folder, err, h.api.names())
		}
	}
}

func TestSomeoneElsesMessageNamingASendsThreadingIDIsntItsConfirmation(t *testing.T) {
	h := loaded(t)
	h.api.sendText = func(req *slidetypes.SendTextRequest) (*slidetypes.SendTextResponse, error) {
		theirs := fmt.Sprintf(`{"id":"mid.theirs","sender_fbid":"%d","timestamp_ms":"%d","offline_threading_id":%q,
			"content":{"__typename":"SlideMessageText","text_body":"not yours"}}`, mayaFBID, tsBase+9000, req.OfflineThreadingID)
		_ = h.api.emit(delta(t, "SlideUQPPNewMessage", dmIGID, `"message":`+theirs))
		return &slidetypes.SendTextResponse{Message: sentMessage("mid.mine", tsBase+9001)}, nil
	}
	if _, err := h.send(mayaFBID, "hello", nil); err != nil {
		t.Fatal(err)
	}
	sent := sentEvents(h.rec)
	if len(sent) != 1 {
		t.Fatalf("message_sent = %v", sent)
	}
	if m := sent[0]["message"].(map[string]any); m["text"] != "hello" || m["outgoing"] != true {
		t.Errorf("your message = %v", m)
	}
	msgs := h.rec.events("message")
	if len(msgs) != 1 || msgs[0]["message"].(map[string]any)["text"] != "not yours" || msgs[0]["message"].(map[string]any)["outgoing"] == true {
		t.Errorf("their message = %v", msgs)
	}
}

func TestAnEditOlderThanTheTextShownDoesntReplaceIt(t *testing.T) {
	h := loaded(t, textMsg("mid.1", mayaFBID, tsBase, "hi"))
	h.b.mu.Lock()
	c := h.b.chats[mayaFBID]
	h.b.putLog(c, c.msgs["mid.1"]) // as if tuimeta loaded it
	h.b.mu.Unlock()
	edit := func(text string, at int64) {
		entry := ""
		if at > 0 {
			entry = fmt.Sprintf(`,"slide_edit_history_entry":{"timestamp_ms":"%d"}`, at)
		}
		_ = h.api.emit(delta(t, "SlideUQPPEditMessage", dmIGID, fmt.Sprintf(`"message_id":"mid.1","text_body":%q%s`, text, entry)))
	}
	shown := func() (string, int) {
		h.b.mu.Lock()
		defer h.b.mu.Unlock()
		return c.msgs["mid.1"].text, c.msgs["mid.1"].editCount
	}
	edit("second", tsBase+2000)
	edit("first", tsBase+1000) // came late
	if text, edits := shown(); text != "second" || edits != 1 {
		t.Errorf("text %q after %d edits", text, edits)
	}
	if msgs := h.rec.events("message"); len(msgs) != 1 || msgs[0]["message"].(map[string]any)["text"] != "second" {
		t.Errorf("messages = %v", msgs)
	}
	// One without a time can only be taken as it comes.
	edit("third", 0)
	if text, _ := shown(); text != "third" {
		t.Errorf("text %q", text)
	}
	// A message fetched again knows when it was last edited.
	h.b.mu.Lock()
	n := h.b.convert(c, message(t, fmt.Sprintf(`{"id":"mid.1","sender_fbid":"%d","timestamp_ms":"%d","content":{"__typename":"SlideMessageText","text_body":"fourth"},
		"slide_edit_history":[{"body":"hi","timestamp_ms":"%d"},{"body":"third","timestamp_ms":"%d"}]}`, mayaFBID, tsBase, tsBase+1000, tsBase+5000)))
	h.b.keep(c, n)
	h.b.mu.Unlock()
	edit("late", tsBase+4000)
	if text, _ := shown(); text != "fourth" {
		t.Errorf("text %q", text)
	}
}

func TestALinkInsideSeveralShimsIsTakenOutOfThemAll(t *testing.T) {
	inner := "https://example.com/routes/arete"
	wrap := func(host, u string) string { return "https://" + host + "/l.php?u=" + url.QueryEscape(u) + "&h=AT0" }
	if got := unwrapRedirect(wrap("l.facebook.com", wrap("l.instagram.com", inner))); got != inner {
		t.Errorf("two shims: %q", got)
	}
	deep := inner
	for range maxUnwrap + 1 {
		deep = wrap("lm.facebook.com", deep)
	}
	if got := unwrapRedirect(deep); !strings.HasPrefix(got, "https://lm.facebook.com/") {
		t.Errorf("past the limit: %q", got)
	}
	// And so in a shared link's card.
	h := newHarness(t)
	msg := fmt.Sprintf(`{"id":"mid.link","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageXMAContent",
		"xma":{"target_url":%q,"title_text":"The Arête"}}}`, tsBase, wrap("l.facebook.com", wrap("l.instagram.com", inner)))
	_, parts := h.convertOne(dmThread(), msg)
	if lp := parts[0].LinkPreview; lp == nil || lp.URL != inner || parts[0].Text != inner {
		t.Errorf("card %+v, text %q", lp, parts[0].Text)
	}
}

func TestALogoutRacingTheConnectionLeavesNoSessionOnDisk(t *testing.T) {
	saved := func(h *harness) bool {
		t.Helper()
		var s savedSession
		found, err := h.deps.Session.LoadJSON(sessionFile, &s)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	h := newHarness(t)
	h.login(mailbox(t, "", false, dmThread()))
	if !saved(h) {
		t.Fatal("a login saved no session")
	}
	h.b.mu.Lock()
	conn, epoch := h.b.conn, h.b.epoch
	h.b.mu.Unlock()
	if err := h.b.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The connection's last steps land after the logout: establishing it,
	// and its socket saying it's connected again.
	h.b.establish(conn, epoch)
	h.b.onConnected(conn)
	if saved(h) {
		t.Error("the cookies are on disk after logging out")
	}
	// Quitting after logging out saves nothing either.
	h.b.Close()
	if saved(h) {
		t.Error("the cookies are on disk after quitting")
	}
}

func TestAPanicApplyingInstagramsDataLeavesTheBackendUsable(t *testing.T) {
	h := loaded(t)
	h.b.mu.Lock()
	c, conn, gen, epoch := h.b.chats[mayaFBID], h.b.conn, h.b.chats[mayaFBID].histGen, h.b.epoch
	h.b.mu.Unlock()
	free := func(what string) {
		t.Helper()
		if !h.b.mu.TryLock() {
			t.Fatalf("%s: the backend's lock is still held after a panic", what)
		}
		h.b.mu.Unlock()
	}
	func() {
		defer func() { _ = recover() }()
		h.b.applyPage(conn, c, gen, func() { panic("something unforeseen") })
	}()
	free("a history page")
	func() {
		defer func() { _ = recover() }()
		h.b.adopt(conn, epoch, selfFBID, nil, nil) // no viewer: it panics
	}()
	free("the inbox")
}
