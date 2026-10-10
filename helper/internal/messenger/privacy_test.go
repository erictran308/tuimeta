// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow"
	waTypes "go.mau.fi/whatsmeow/types"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestTasksThatTellPeopleThingsOnlyGoOutForTheirOwnRequest(t *testing.T) {
	read := &socket.ThreadMarkReadTask{ThreadId: 1}
	typing := &socket.UpdatePresenceTask{ThreadKey: 1}
	active := &socket.ReportAppStateTask{AppState: table.FOREGROUND}
	for _, purpose := range []string{"send", "history", "search", "react", "edit", "open_dm", "mute"} {
		if guardTasks(purpose, []socket.Task{read}) == nil {
			t.Errorf("%s let a read receipt through", purpose)
		}
		if guardTasks(purpose, []socket.Task{&socket.SendMessageTask{}, typing}) == nil {
			t.Errorf("%s let typing through", purpose)
		}
	}
	if guardTasks("mark_read", []socket.Task{read}) != nil || guardTasks("typing", []socket.Task{typing}) != nil {
		t.Error("the requests that are for this were refused")
	}
	for _, purpose := range []string{"mark_read", "typing", "send"} {
		if guardTasks(purpose, []socket.Task{active}) == nil {
			t.Errorf("an activity report went out for %s", purpose)
		}
	}
	// Through run, a refused task never reaches the connection.
	h := newHarness(t)
	if _, err := h.m.run(context.Background(), "history", read); err == nil || len(h.meta.sent()) != 0 {
		t.Error("run sent a read receipt")
	}
}

func TestTheEncryptedConnectionCantSetAPresence(t *testing.T) {
	forbidden := []string{"SendPresence", "SubscribePresence", "SetForceActiveDeliveryReceipts", "SetPassive"}
	for _, typ := range []reflect.Type{reflect.TypeFor[e2eeAPI](), reflect.TypeFor[*e2eeConn](), reflect.TypeFor[metaAPI](), reflect.TypeFor[*metaConn]()} {
		for i := range typ.NumMethod() {
			name := typ.Method(i).Name
			for _, f := range forbidden {
				if name == f {
					t.Errorf("%v has %s", typ, name)
				}
			}
			if strings.Contains(name, "AppState") || (strings.Contains(name, "Presence") && name != "SendChatPresence") {
				t.Errorf("%v has %s", typ, name)
			}
		}
	}
	var conn any = &e2eeConn{}
	if _, ok := conn.(interface {
		SendPresence(context.Context, waTypes.Presence) error
	}); ok {
		t.Error("the encrypted connection can send a presence")
	}
}

func TestWhatsmeowIsConfiguredToKeepQuiet(t *testing.T) {
	st, err := openStore(context.Background(), filepath.Join(t.TempDir(), storeFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dev, _, err := st.device(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	cli := whatsmeow.NewClient(dev, nil)
	cli.UseRetryMessageStore = true
	cli.AutomaticMessageRerequestFromPhone = true
	cli.SendReportingTokens = true
	configureE2EE(cli)
	if cli.UseRetryMessageStore || cli.AutomaticMessageRerequestFromPhone || cli.SendReportingTokens {
		t.Error("whatsmeow keeps or sends more than it needs to")
	}
	if !cli.SynchronousAck || !cli.EnableAutoReconnect {
		t.Error("acks should wait for the message to be kept, and the socket should reconnect")
	}
	if cli.IsConnected() {
		t.Error("configuring connected")
	}
}

func TestApplyingWhatArrivesTellsNobodyAnything(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -20, 3), groupThread(groupID, "Trip", -5)}})
	h.load()
	in := textMsg(aliceID, "mid.$p1", aliceID, -1, "are you there?")
	h.apply(&table.LSTable{
		LSInsertMessage:                 []*table.LSInsertMessage{in, textMsg(groupID, "mid.$p2", benID, -1, "hello all")},
		LSUpsertReaction:                []*table.LSUpsertReaction{{ThreadKey: aliceID, MessageId: "mid.$p1", ActorId: aliceID, Reaction: "👀"}},
		LSUpdateTypingIndicator:         []*table.LSUpdateTypingIndicator{{ThreadKey: aliceID, SenderId: aliceID, IsTyping: true}},
		LSUpdateReadReceipt:             []*table.LSUpdateReadReceipt{{ThreadKey: groupID, ContactId: benID, ReadWatermarkTimestampMs: ms(-1)}},
		LSUpdateOrInsertThread:          []*table.LSUpdateOrInsertThread{{ThreadKey: aliceID, ThreadType: table.ONE_TO_ONE, LastActivityTimestampMs: ms(-1)}},
		LSAddParticipantIdToGroupThread: []*table.LSAddParticipantIdToGroupThread{{ThreadKey: groupID, ContactId: benID}},
	})
	h.wa(waMsg(jid(aliceID), jid(aliceID), "W1", 0, waText("encrypted hi"), nil))
	for _, task := range h.meta.sent() {
		switch task.(type) {
		case *socket.GetContactsFullTask, *socket.CreateThreadTask:
			// Lookups of who someone is; nothing about you.
		default:
			t.Errorf("applying incoming data sent %T", task)
		}
	}
	if len(h.meta.stateless) != 0 || len(h.e2ee.reads) != 0 || len(h.e2ee.selfReads) != 0 || len(h.e2ee.presences) != 0 || len(h.e2ee.sent) != 0 {
		t.Error("applying incoming data sent something")
	}
	// The chats are still unread.
	if c := h.chat(aliceID); c.unread == 0 || c.readUpTo != ms(-20) {
		t.Errorf("alice's chat was marked read: %+v", c)
	}
}

func TestMarkReadAndTypingSendExactlyWhatWasAsked(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -20, 0)}})
	h.load()
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$r1", aliceID, -3, "a"), textMsg(aliceID, "mid.$r2", aliceID, -2, "b")}})
	c := h.chat(aliceID)
	msgs := h.rec.messages()
	h.rec.reset()
	if err := h.m.MarkRead(context.Background(), h.ref(c, msgs[0].ID)); err != nil {
		t.Fatal(err)
	}
	sent := h.meta.sent()
	if len(sent) != 1 {
		t.Fatalf("tasks %+v", sent)
	}
	if r, ok := sent[0].(*socket.ThreadMarkReadTask); !ok || r.ThreadId != aliceID || r.LastReadWatermarkTs != ms(-3) {
		t.Errorf("read task %+v", sent[0])
	}
	reads := h.rec.of("read")
	if len(reads) != 1 || *reads[0].(proto.ReadEvent).Unread != 1 {
		t.Errorf("read event %+v", reads)
	}
	// Reading what's already read asks nothing.
	if err := h.m.MarkRead(context.Background(), h.ref(c, msgs[0].ID)); err != nil || len(h.meta.sent()) != 1 {
		t.Errorf("a second receipt went out: %v %d", err, len(h.meta.sent()))
	}
	chat := backend.ChatRef{ID: c.id}
	if err := h.m.SetTyping(context.Background(), chat, true); err != nil {
		t.Fatal(err)
	}
	if err := h.m.SetTyping(context.Background(), chat, false); err != nil {
		t.Fatal(err)
	}
	if len(h.meta.stateless) != 2 {
		t.Fatalf("typing %+v", h.meta.stateless)
	}
	on, off := h.meta.stateless[0].(*socket.UpdatePresenceTask), h.meta.stateless[1].(*socket.UpdatePresenceTask)
	if on.IsTyping != 1 || off.IsTyping != 0 || on.ThreadKey != aliceID || on.IsGroupThread != 0 {
		t.Errorf("typing tasks %+v %+v", on, off)
	}
}

func TestTheLibrariesLogNowhere(t *testing.T) {
	silenceLibraries()
	if zlog.Logger.GetLevel() != zerolog.Disabled {
		t.Error("zerolog's global logger is on")
	}
	cli := newMessagix(map[string]string{"c_user": "1", "xs": "2", "datr": "3", "presence": "EDvF3EtimeF1"}, browser.Default())
	if cli.Logger.GetLevel() != zerolog.Disabled {
		t.Error("messagix logs")
	}
	if cli.GetCookies().Get("presence") != "" || cli.GetCookies().Get("xs") != "2" {
		t.Error("the page's presence cookie was handed on")
	}
	if p, err := clientSettings().HTTPProxy(nil); p != nil || err != nil {
		t.Error("an environment proxy could be used")
	}
}

func TestNothingPrivateReachesTheLog(t *testing.T) {
	var buf bytes.Buffer
	hlog.SetOutput(&buf)
	defer hlog.SetOutput(&bytes.Buffer{})
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$secret", aliceID, -1, "my bank PIN is 4321")}})
	c := h.chat(aliceID)
	h.meta.answer = func([]socket.Task) (*table.LSTable, error) {
		return nil, errors.New("https://www.facebook.com/x?token=SECRETTOKEN said no to 'my bank PIN'")
	}
	err := h.m.Send(context.Background(), h.outgoing(c, "hello Alice Example", nil, nil))
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("send error %v", err)
	}
	_, _ = h.m.Search(context.Background(), "Alice")
	h.m.Close()
	log := buf.String()
	for _, secret := range []string{"SECRETTOKEN", "PIN", "Alice", "hello", "100002", "mid.$"} {
		if strings.Contains(log, secret) {
			t.Errorf("the log has %q:\n%s", secret, log)
		}
	}
}
