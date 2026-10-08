// SPDX-License-Identifier: AGPL-3.0-or-later

package history

import (
	"testing"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// ids returns the page's ids.
func ids(p Page) []int64 {
	var out []int64
	for _, m := range p.Messages {
		out = append(out, m.ID)
	}
	return out
}

func ptr(v int64) *int64 { return &v }

// tens holds messages 10, 20, … 100, with 50, 51, 52 an album.
func tens() *Log {
	l := New()
	for i := int64(10); i <= 100; i += 10 {
		l.Put(proto.Message{ID: i})
	}
	l.Put(proto.Message{ID: 51, Album: 50}, proto.Message{ID: 52, Album: 50})
	l.Put(proto.Message{ID: 50, Album: 50})
	l.SetComplete(true)
	return l
}

func eq(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPagingByPositionsInTime(t *testing.T) {
	l := tens()
	cases := []struct {
		name string
		q    Query
		want []int64
		more bool
	}{
		{"newest", Query{Limit: 3}, []int64{80, 90, 100}, true},
		{"everything", Query{Limit: 100}, []int64{10, 20, 30, 40, 50, 51, 52, 60, 70, 80, 90, 100}, false},
		{"before a message", Query{Before: ptr(40), Limit: 2}, []int64{20, 30}, true},
		{"before a gap", Query{Before: ptr(35), Limit: 2}, []int64{20, 30}, true},
		{"before the start", Query{Before: ptr(10), Limit: 5}, nil, false},
		{"after a message", Query{After: ptr(70), Limit: 2}, []int64{80, 90}, true},
		{"after id-1 starts at the message", Query{After: ptr(79), Limit: 2}, []int64{80, 90}, true},
		{"after the end", Query{After: ptr(100), Limit: 2}, nil, false},
		{"after reaching the end", Query{After: ptr(85), Limit: 5}, []int64{90, 100}, false},
		{"around a message", Query{Around: ptr(30), Limit: 3}, []int64{20, 30, 40}, true},
		{"around a gap", Query{Around: ptr(35), Limit: 2}, []int64{30, 40}, true},
		{"around the start", Query{Around: ptr(10), Limit: 3}, []int64{10, 20, 30}, false},
		{"around the end", Query{Around: ptr(100), Limit: 3}, []int64{80, 90, 100}, true},
		{"an album isn't cut going back", Query{Before: ptr(60), Limit: 2}, []int64{50, 51, 52}, true},
		{"an album isn't cut going forward", Query{After: ptr(40), Limit: 2}, []int64{50, 51, 52}, true},
		{"before a middle part", Query{Before: ptr(52), Limit: 1}, []int64{50, 51}, true},
	}
	for _, c := range cases {
		p := l.Page(c.q)
		if !eq(ids(p), c.want) || p.HasMore != c.more {
			t.Errorf("%s: got %v more=%v, want %v more=%v", c.name, ids(p), p.HasMore, c.want, c.more)
		}
		if p.Messages == nil {
			t.Errorf("%s: nil list (would be null on the wire)", c.name)
		}
	}
}

func TestAnIncompleteLogHasMoreBeforeItsOldest(t *testing.T) {
	l := tens()
	l.SetComplete(false)
	if p := l.Page(Query{Limit: 100}); !p.HasMore {
		t.Error("an incomplete log said it had nothing older")
	}
}

func TestPutReplacesAndRemoveDeletes(t *testing.T) {
	l := tens()
	l.Put(proto.Message{ID: 30, Text: "edited"})
	if m, _ := l.Get(30); m.Text != "edited" {
		t.Error("Put didn't replace")
	}
	if got := l.Remove(30, 31); len(got) != 1 || got[0] != 30 {
		t.Errorf("Remove = %v", got)
	}
	if _, ok := l.Get(30); ok {
		t.Error("removed message still there")
	}
	if n, _ := l.Newest(); n.ID != 100 {
		t.Errorf("Newest = %d", n.ID)
	}
}
