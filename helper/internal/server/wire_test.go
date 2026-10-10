// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// written is every line a writer wrote to out, once it's closed.
func written(t *testing.T, out *bytes.Buffer) []string {
	t.Helper()
	var lines []string
	sc := bufio.NewScanner(out)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

func TestNoLineWrittenIsLongerThanTuimetaReads(t *testing.T) {
	logBuf := &syncBuffer{}
	hlog.SetOutput(logBuf)
	var out bytes.Buffer
	wr := newWriter(&out, proto.NewHello("test"), nil)
	// {"id":N,"result":"…"} and its newline: 21 bytes around the text.
	id := func(n uint64) *uint64 { return &n }
	wr.send(response{ID: id(1), Result: strings.Repeat("a", MaxOutLine-21)})
	wr.send(response{ID: id(2), Result: strings.Repeat("a", MaxOutLine-20)})
	wr.send(proto.MessageEvent{Event: "message", Message: proto.Message{ID: 1, Text: strings.Repeat("<", MaxOutLine/6)}})
	wr.send(response{ID: id(3), Result: "after"})
	wr.close(5 * time.Second)

	lines := written(t, &out)
	if len(lines) != 4 {
		t.Fatalf("%d lines", len(lines))
	}
	for i, l := range lines {
		if len(l)+1 > MaxOutLine {
			t.Errorf("line %d is %d bytes", i, len(l)+1)
		}
	}
	if len(lines[1])+1 != MaxOutLine {
		t.Errorf("a line of exactly the limit became %d bytes", len(lines[1])+1)
	}
	var over response
	if err := json.Unmarshal([]byte(lines[2]), &over); err != nil || over.ID == nil || *over.ID != 2 || over.Error == nil || over.Error.Code != proto.Internal {
		t.Errorf("the answer over the limit became %.200s", lines[2])
	}
	if !strings.Contains(lines[3], `"result":"after"`) {
		t.Errorf("then %.200s", lines[3])
	}
	if log := logBuf.String(); !strings.Contains(log, "dropped an event too long") || strings.Contains(log, "<<<") {
		t.Errorf("the log: %.300s", log)
	}
}
