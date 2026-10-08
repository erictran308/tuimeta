// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

type lines struct {
	mu  sync.Mutex
	out []string
}

func (l *lines) add(v any) {
	data, _ := json.Marshal(v)
	l.mu.Lock()
	l.out = append(l.out, string(data))
	l.mu.Unlock()
}

func (l *lines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.out...)
}

func TestTypingStopsByItselfAfterTheTimeout(t *testing.T) {
	var got lines
	e := NewEvents(got.add)
	e.SetTypingTimeout(80 * time.Millisecond)
	e.Typing(1, 2, true)
	time.Sleep(40 * time.Millisecond)
	e.Typing(1, 2, true) // a repeat keeps it going, and isn't said twice
	time.Sleep(50 * time.Millisecond)
	if n := len(got.all()); n != 1 {
		t.Fatalf("after a repeat: %v", got.all())
	}
	time.Sleep(80 * time.Millisecond)
	all := got.all()
	if len(all) != 2 || !strings.Contains(all[1], `"typing":false`) {
		t.Fatalf("no stop after the timeout: %v", all)
	}
	e.Typing(1, 2, false) // already stopped: nothing more
	if len(got.all()) != 2 {
		t.Errorf("a second stop was sent: %v", got.all())
	}
}

func TestAMessageEndsItsSendersTyping(t *testing.T) {
	var got lines
	e := NewEvents(got.add)
	e.Typing(1, 2, true)
	e.Message(proto.Message{ID: 5, ChatID: 1, SenderID: 2})
	all := got.all()
	if len(all) != 3 || !strings.Contains(all[1], `"typing":false`) || !strings.Contains(all[2], `"event":"message"`) {
		t.Fatalf("got %v", all)
	}
	e.Close()
}

func TestSentPairsTemporaryIDsWithPartsAndOnlyOnce(t *testing.T) {
	var got lines
	e := NewEvents(got.add)
	msgs := ids.NewMessages()
	box := NewOutbox(e, msgs)
	chat := ChatRef{ID: 7, Network: proto.Messenger}
	temps := msgs.Temp(7, 3)
	out := box.New(chat, temps, "", nil, nil)
	out.SetKey("otid-1")
	if box.Find(proto.Messenger, "otid-1") != out || box.Find(proto.Instagram, "otid-1") != nil {
		t.Fatal("Find")
	}
	// The network made two messages of three files.
	if !out.Sent(proto.Message{ID: 100, ChatID: 7}, proto.Message{ID: 101, ChatID: 7}) {
		t.Fatal("Sent returned false")
	}
	if out.Sent(proto.Message{ID: 100}) || out.Failed(errors.New("late")) {
		t.Error("a second outcome counted")
	}
	all := got.all()
	if len(all) != 3 ||
		!strings.Contains(all[0], `"message_sent"`) || !strings.Contains(all[0], `"id":100`) ||
		!strings.Contains(all[1], `"message_sent"`) || !strings.Contains(all[1], `"id":101`) ||
		!strings.Contains(all[2], `"message_deleted"`) {
		t.Fatalf("got %v", all)
	}
	if box.Find(proto.Messenger, "otid-1") != nil {
		t.Error("still in the outbox")
	}
	if _, ok := msgs.Lookup(7, temps[0]); ok {
		t.Error("temporary id still names something")
	}
}

func TestFailedShowsOnlyTheHelpersOwnWords(t *testing.T) {
	var got lines
	e := NewEvents(got.add)
	box := NewOutbox(e, ids.NewMessages())
	out := box.New(ChatRef{ID: 1}, []int64{5}, "", nil, nil)
	out.Failed(errors.New("https://example.com/?token=SECRET failed"))
	all := got.all()
	if len(all) != 1 || strings.Contains(all[0], "SECRET") || !strings.Contains(all[0], `"message_failed"`) {
		t.Fatalf("got %v", all)
	}
	out2 := box.New(ChatRef{ID: 1}, []int64{6}, "", nil, nil)
	out2.Failed(proto.Err(proto.NetworkError, "No connection."))
	if all := got.all(); !strings.Contains(all[1], "No connection.") {
		t.Fatalf("got %v", all)
	}
}

func TestMessagesAlwaysHaveListsAndAState(t *testing.T) {
	data, err := json.Marshal(proto.Message{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`"entities":[]`, `"reactions":[]`, `"state":"sent"`, `"album":0`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	for _, absent := range []string{"media", "reply_to", "service", "link_preview", "unsupported", "editable_until"} {
		if strings.Contains(s, absent) {
			t.Errorf("%s present in %s", absent, s)
		}
	}
	e, ok := proto.EntityFor("🎉 @Ben hi", "@Ben", proto.Mention)
	if !ok || e.Offset != 3 || e.Length != 4 {
		t.Errorf("UTF-16 entity = %+v", e)
	}
}
