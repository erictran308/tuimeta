// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/wiretest"
)

// longest is a text at WhatsApp's limit made of a character JSON writes in
// six bytes, the longest a message's text gets on the wire.
var longest = strings.Repeat("<", 64<<10)

// longPage is n messages with ids 1…n, each with the longest text.
func longPage(n int) []proto.Message {
	page := make([]proto.Message, n)
	for i := range page {
		page[i] = proto.Message{ID: int64(i + 1), ChatID: 1, Text: longest}
	}
	return page
}

func wireSize(t *testing.T, m proto.Message) int {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return len(b) + 1
}

func idsOf(t *testing.T, raw []json.RawMessage) []int64 {
	t.Helper()
	var out []int64
	for _, r := range raw {
		var m struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(r, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m.ID)
	}
	return out
}

func span(from, to int64) []int64 {
	var out []int64
	for id := from; id <= to; id++ {
		out = append(out, id)
	}
	return out
}

func ptr(v int64) *int64 { return &v }

func TestAPageOfTheLongestMessagesStillFitsOnALine(t *testing.T) {
	// The audit's shape: 50 of them made a 19.7 MB line, past tuimeta's
	// 16 MiB, and cut it off from the helper for the rest of the run.
	msgs := longPage(50)
	h := startWith(t, func(h *harness) {
		h.stub.pages = func(history.Query) history.Page {
			return history.Page{Messages: slices.Clone(msgs)}
		}
	})
	l := h.c.Call("history", map[string]any{"chat_id": h.stub.chatID, "limit": 50})
	if l.Error != nil {
		t.Fatalf("got an error: %s", l.Error.Message)
	}
	if len(l.Raw) > MaxOutLine {
		t.Fatalf("the answer is %d bytes", len(l.Raw))
	}
	page := wiretest.Decode[struct {
		Messages []proto.Message `json:"messages"`
		HasMore  bool            `json:"has_more"`
	}](t, l.Result)
	each := wireSize(t, msgs[49]) // the kept ones all have two-digit ids
	fit := PageBudget / each
	if len(page.Messages) != fit || !page.HasMore {
		t.Fatalf("%d messages (has_more %v), want the newest %d and has_more", len(page.Messages), page.HasMore, fit)
	}
	if first, last := page.Messages[0], page.Messages[len(page.Messages)-1]; first.ID != int64(50-fit+1) || last.ID != 50 || last.Text != longest {
		t.Errorf("messages %d…%d", first.ID, last.ID)
	}
	// And the helper is still heard from.
	if l := h.c.Call("search", map[string]any{"network": "messenger", "query": "x"}); l.Error != nil {
		t.Fatalf("after the page: %s", l.Raw)
	}
}

func TestAPageThatFitsIsSentWholeAndOneByteOverIsCut(t *testing.T) {
	msgs := longPage(10)
	total := 0
	for _, m := range msgs {
		total += wireSize(t, m)
	}
	raw, more, err := fitPage(history.Page{Messages: msgs}, history.Query{}, total)
	if err != nil || len(raw) != 10 || more {
		t.Fatalf("at the budget: %d messages, more %v, %v", len(raw), more, err)
	}
	raw, more, _ = fitPage(history.Page{Messages: msgs}, history.Query{}, total-1)
	if got := idsOf(t, raw); !slices.Equal(got, span(2, 10)) || !more {
		t.Errorf("a byte over: %v, more %v", got, more)
	}
}

func TestAPageIsCutFromTheEndItReachesAwayFrom(t *testing.T) {
	msgs := longPage(10)
	budget := 4 * wireSize(t, msgs[9]) // id 10 is a byte longer
	cases := []struct {
		name string
		q    history.Query
		want []int64
		more bool
	}{
		{"the newest", history.Query{}, span(7, 10), true},
		{"before", history.Query{Before: ptr(11)}, span(7, 10), true},
		{"after", history.Query{After: ptr(0)}, span(1, 4), true},
		{"around the middle", history.Query{Around: ptr(5)}, span(4, 7), true},
		{"around the newest", history.Query{Around: ptr(10)}, span(7, 10), true},
		{"around the oldest, older ones kept", history.Query{Around: ptr(1)}, span(1, 4), false},
		{"around a gap", history.Query{Around: ptr(-3)}, span(1, 4), false},
	}
	for _, c := range cases {
		raw, more, err := fitPage(history.Page{Messages: msgs}, c.q, budget)
		if err != nil {
			t.Fatal(err)
		}
		if got := idsOf(t, raw); !slices.Equal(got, c.want) || more != c.more {
			t.Errorf("%s: %v (more %v), want %v (more %v)", c.name, got, more, c.want, c.more)
		}
	}
}

func TestAnAlbumIsntCutAndOneMessageAlwaysStays(t *testing.T) {
	msgs := longPage(6)
	for i := 1; i <= 3; i++ { // 2, 3, 4 are an album
		msgs[i].Album = 2
	}
	each := wireSize(t, msgs[0])
	raw, _, _ := fitPage(history.Page{Messages: msgs}, history.Query{}, 4*each)
	if got := idsOf(t, raw); !slices.Equal(got, span(5, 6)) {
		t.Errorf("the newest: %v", got)
	}
	raw, _, _ = fitPage(history.Page{Messages: msgs}, history.Query{After: ptr(0)}, 3*each)
	if got := idsOf(t, raw); !slices.Equal(got, span(1, 1)) {
		t.Errorf("after: %v", got)
	}
	raw, _, _ = fitPage(history.Page{Messages: msgs}, history.Query{Around: ptr(3)}, each)
	if got := idsOf(t, raw); !slices.Equal(got, span(2, 4)) {
		t.Errorf("around a part: %v", got)
	}
	raw, more, _ := fitPage(history.Page{Messages: msgs}, history.Query{}, 1)
	if got := idsOf(t, raw); !slices.Equal(got, span(6, 6)) || !more {
		t.Errorf("nothing fits: %v (more %v)", got, more)
	}
	raw, _, _ = fitPage(history.Page{}, history.Query{}, 1)
	if raw == nil {
		t.Error("an empty page would be null on the wire")
	}
}
