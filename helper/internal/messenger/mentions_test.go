// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waArmadilloApplication"
	"go.mau.fi/whatsmeow/proto/waArmadilloXMA"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	gproto "google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"
)

// strangers are made-up ids a message names.
const strangers = 300000

// named counts the people a run knows of among the made-up strangers.
func (h *harness) named() int {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	n := 0
	for fbid := range h.m.people {
		if fbid >= strangers && fbid < strangers+100000 {
			n++
		}
	}
	return n
}

// mentioning is n structured mentions of different strangers (or one
// stranger n times), and the text they're in.
func mentioning(n int, same bool) (string, []*waCommon.Mention) {
	var out []*waCommon.Mention
	for i := range n {
		who := strangers + i
		if same {
			who = strangers
		}
		out = append(out, &waCommon.Mention{
			MentionedJID: gproto.String(strconv.Itoa(who) + "@msgr"), Offset: gproto.Uint32(uint32(i)), Length: gproto.Uint32(1),
		})
		if same {
			out[i].Offset = gproto.Uint32(0)
		}
	}
	return strings.Repeat("x", n), out
}

func TestAMessageMentionsAtMost256PeopleEachOnce(t *testing.T) {
	h := newHarness(t)
	h.load()
	text, list := mentioning(1000, false)
	h.wa(waMsg(waGroup, jid(aliceID), "M1", -3, &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_MessageText{
		MessageText: &waCommon.MessageText{Text: gproto.String(text), Mentions: list},
	}}, nil))
	if n := h.named(); n != MaxMentions {
		t.Errorf("a message named %d people", n)
	}
	if msgs := h.rec.messages(); len(msgs[0].Entities) != MaxMentions {
		t.Errorf("%d mentions shown", len(msgs[0].Entities))
	}
	// The same one over and over is one.
	h.rec.reset()
	text, list = mentioning(1000, true)
	h.wa(waMsg(waGroup, jid(aliceID), "M2", -2, &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_MessageText{
		MessageText: &waCommon.MessageText{Text: gproto.String(text), Mentions: list},
	}}, nil))
	if msgs := h.rec.messages(); len(msgs[0].Entities) != 1 {
		t.Errorf("one person repeated is %d mentions", len(msgs[0].Entities))
	}
	// Nor can an edit name more.
	_, list = mentioning(1000, false)
	for _, mn := range list {
		mn.MentionedJID = gproto.String("5" + mn.GetMentionedJID()) // others again
	}
	edit := waEditOf(waGroup, jid(aliceID), "E1", -1, "M1", true, "", text, 1)
	edit.Message.(*waConsumerApplication.ConsumerApplication).GetPayload().GetContent().GetEditMessage().Message.Mentions = list
	before := len(h.peopleIDs())
	h.wa(edit)
	if n := len(h.peopleIDs()) - before; n > MaxMentions {
		t.Errorf("an edit named %d more people", n)
	}
}

// peopleIDs is everyone the run knows.
func (h *harness) peopleIDs() []int64 {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	var out []int64
	for fbid := range h.m.people {
		out = append(out, fbid)
	}
	return out
}

func TestMentionsWrittenInTheTextAreCappedAndFoundInOnePass(t *testing.T) {
	h := newHarness(t)
	h.load()
	// A thousand people, each written out in a long text.
	var b strings.Builder
	var jids []string
	for i := range 1000 {
		j := strconv.Itoa(strangers+i) + "@msgr"
		jids = append(jids, j)
		b.WriteString("@" + j + " " + strings.Repeat("é", 300) + " ")
	}
	start := time.Now()
	h.wa(waMsg(waGroup, jid(aliceID), "M1", -1, waText(b.String(), jids...), nil))
	took := time.Since(start)
	if n := h.named(); n != MaxMentions {
		t.Errorf("named %d people", n)
	}
	msgs := h.rec.messages()
	if len(msgs) != 1 || len(msgs[0].Entities) != MaxMentions {
		t.Fatalf("%d messages", len(msgs))
	}
	// Each mention covers "@someone" where the id was.
	first := msgs[0].Entities[0]
	if first.Offset != 0 || first.Length != len("@someone") {
		t.Errorf("first mention %+v", first)
	}
	second := msgs[0].Entities[1]
	if want := len("@someone") + 1 + 300 + 1; second.Offset != want {
		t.Errorf("second mention at %d, want %d", second.Offset, want)
	}
	// Finding them took one pass, not one per person (over a second here).
	if took > 150*time.Millisecond {
		t.Errorf("a message's mentions took %v", took)
	}
}

func TestAnXMAsAndATablesMentionsAreCappedToo(t *testing.T) {
	h := newHarness(t)
	h.load()
	text, list := mentioning(1000, false)
	evt := waMsg(jid(aliceID), jid(aliceID), "X1", -2, waText("x"), nil)
	evt.Message = armadilloMsg(&waArmadilloApplication.Armadillo_Content{Content: &waArmadilloApplication.Armadillo_Content_ExtendedContentMessage{
		ExtendedContentMessage: &waArmadilloXMA.ExtendedContentMessage{MessageText: gproto.String(text), Mentions: list},
	}})
	h.wa(evt)
	if n := h.named(); n != MaxMentions {
		t.Errorf("a shared post named %d people", n)
	}

	h2 := newHarness(t)
	h2.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{groupThread(groupID, "Trip", -5)}})
	h2.load()
	var ids, offsets, lengths, types []string
	for i := range 1000 {
		ids = append(ids, strconv.Itoa(strangers+i))
		offsets = append(offsets, strconv.Itoa(i))
		lengths = append(lengths, "1")
		types = append(types, "p")
	}
	in := textMsg(groupID, "mid.$m1", aliceID, -1, text)
	in.MentionIds, in.MentionOffsets, in.MentionLengths, in.MentionTypes = strings.Join(ids, ","), strings.Join(offsets, ","), strings.Join(lengths, ","), strings.Join(types, ",")
	h2.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{in}})
	if n := h2.named(); n != MaxMentions {
		t.Errorf("a Messenger message named %d people", n)
	}
}

func TestAMentionReachingPastTheTextIsDropped(t *testing.T) {
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	in := textMsg(aliceID, "mid.$m1", aliceID, -1, "hi @Ben and all")
	// The second one's end overflows; the third is past the text.
	in.MentionIds, in.MentionOffsets, in.MentionLengths, in.MentionTypes = "100003,100003,100003", "3,1,40", "4,9223372036854775807,2", "p,p,p"
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{in}})
	msgs := h.rec.messages()
	if len(msgs) != 1 || len(msgs[0].Entities) != 1 || msgs[0].Entities[0].Offset != 3 || msgs[0].Entities[0].Length != 4 {
		t.Errorf("message %+v", msgs)
	}
	if !h.m.mu.TryLock() {
		t.Fatal("the lock was left held")
	}
	h.m.mu.Unlock()
}
