// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"

	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// PageBudget is how many bytes a history page's messages may come to on
// the wire: well within MaxOutLine, so a page of long messages from someone
// (each within its network's limit, but JSON writes some characters six
// bytes long) is still an answer.
const PageBudget = 6 << 20

// fitPage is the page's messages as they go on the wire, cut short to fit
// budget bytes. Messages go from the end the page reaches away from: the
// oldest, or the newest for after, so has_more says there's more and the
// next page picks them up. Around a message, the end farther from it goes
// first, and that message stays. Albums go whole, and at least one message
// (or album) stays, even over budget: MaxOutLine still holds.
func fitPage(page history.Page, q history.Query, budget int) ([]json.RawMessage, bool, error) {
	msgs := page.Messages
	raw := make([]json.RawMessage, len(msgs))
	sizes := make([]int, len(msgs))
	total := 0
	for i, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, false, err
		}
		raw[i], sizes[i] = b, len(b)+1 // and its comma
		total += sizes[i]
	}
	if total <= budget {
		return raw, page.HasMore, nil
	}
	// raw[lo:hi] is kept; the page's own message (around) is raw[keepLo:keepHi].
	lo, hi := 0, len(raw)
	keepLo, keepHi := 0, 0
	if q.Around != nil {
		t := 0
		for i, m := range msgs {
			if m.ID <= *q.Around {
				t = i
			}
		}
		keepLo, keepHi = albumStart(msgs, 0, t), albumEnd(msgs, t, len(msgs))
	}
	droppedOld, droppedNew := false, false
	for total > budget {
		older := q.After == nil
		if q.Around != nil {
			before, after := keepLo-lo, hi-keepHi
			if before == 0 && after == 0 {
				break
			}
			older = before >= after
		}
		if older {
			end := albumEnd(msgs, lo, hi)
			if end >= hi {
				break
			}
			for ; lo < end; lo++ {
				total -= sizes[lo]
			}
			droppedOld = true
		} else {
			start := albumStart(msgs, lo, hi-1)
			if start <= lo {
				break
			}
			for hi > start {
				hi--
				total -= sizes[hi]
			}
			droppedNew = true
		}
	}
	more := page.HasMore
	if q.After != nil {
		more = more || droppedNew
	} else {
		more = more || droppedOld
	}
	return raw[lo:hi], more, nil
}

// albumStart is where the album msgs[i] is part of starts, not before lo.
func albumStart(msgs []proto.Message, lo, i int) int {
	for i > lo && msgs[i].Album != 0 && msgs[i-1].Album == msgs[i].Album {
		i--
	}
	return i
}

// albumEnd is just past the album msgs[i] is part of, not past hi.
func albumEnd(msgs []proto.Message, i, hi int) int {
	j := i + 1
	for j < hi && msgs[i].Album != 0 && msgs[j].Album == msgs[i].Album {
		j++
	}
	return j
}
