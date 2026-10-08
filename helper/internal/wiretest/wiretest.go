// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wiretest drives the helper's protocol in tests, as tuimeta would:
// it writes request lines and reads every line the helper writes.
package wiretest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Timeout is how long a test waits for any one line.
const Timeout = 5 * time.Second

// Line is one line the helper wrote.
type Line struct {
	Seq    int
	Raw    string
	Fields map[string]json.RawMessage
	Event  string
	// IsResponse is set when the line has an "id" (null included).
	IsResponse bool
	ID         *uint64
	Result     json.RawMessage
	Error      *proto.Error
}

// Client talks to a helper.
type Client struct {
	t  testing.TB
	w  io.Writer
	mu sync.Mutex
	// lines are all the lines read, in order.
	lines  []Line
	cond   *sync.Cond
	ended  bool
	readEr error
	next   uint64
}

// New reads r (the helper's stdout) in the background and writes requests
// to w (its stdin).
func New(t testing.TB, w io.Writer, r io.Reader) *Client {
	c := &Client{t: t, w: w, next: 1000}
	c.cond = sync.NewCond(&c.mu)
	go c.read(r)
	return c
}

func (c *Client) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		raw := sc.Text()
		l := Line{Raw: raw}
		if err := json.Unmarshal([]byte(raw), &l.Fields); err != nil {
			l.Event = "!unparsable"
		}
		if ev, ok := l.Fields["event"]; ok {
			_ = json.Unmarshal(ev, &l.Event)
		}
		if id, ok := l.Fields["id"]; ok && l.Event == "" {
			l.IsResponse = true
			_ = json.Unmarshal(id, &l.ID)
			l.Result = l.Fields["result"]
			if e, ok := l.Fields["error"]; ok {
				_ = json.Unmarshal(e, &l.Error)
			}
		}
		c.mu.Lock()
		l.Seq = len(c.lines)
		c.lines = append(c.lines, l)
		c.cond.Broadcast()
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.ended = true
	c.readEr = sc.Err()
	c.cond.Broadcast()
	c.mu.Unlock()
}

// Send writes one raw line.
func (c *Client) Send(raw string) {
	c.t.Helper()
	if _, err := io.WriteString(c.w, raw+"\n"); err != nil {
		c.t.Fatalf("writing a request: %v", err)
	}
}

// Request sends a request without waiting, returning its id.
func (c *Client) Request(method string, params any) uint64 {
	c.t.Helper()
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()
	req := map[string]any{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	data, err := json.Marshal(req)
	if err != nil {
		c.t.Fatalf("encoding a request: %v", err)
	}
	c.Send(string(data))
	return id
}

// Call sends a request and waits for its response.
func (c *Client) Call(method string, params any) Line {
	c.t.Helper()
	return c.Response(c.Request(method, params))
}

// Response waits for the response to request id.
func (c *Client) Response(id uint64) Line {
	c.t.Helper()
	return c.Wait(fmt.Sprintf("the response to %d", id), 0, func(l Line) bool {
		return l.IsResponse && l.ID != nil && *l.ID == id
	})
}

// Mark is how many lines have been read so far, for Wait's from.
func (c *Client) Mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

// Wait returns the first line from line number from on that match accepts,
// failing the test if none comes within Timeout.
func (c *Client) Wait(what string, from int, match func(Line) bool) Line {
	c.t.Helper()
	l, ok := c.WaitFor(Timeout, from, match)
	if !ok {
		c.t.Fatalf("timed out waiting for %s", what)
	}
	return l
}

// WaitFor is Wait without failing: false if nothing matched in time.
func (c *Client) WaitFor(timeout time.Duration, from int, match func(Line) bool) (Line, bool) {
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer timer.Stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := from
	for {
		for ; seen < len(c.lines); seen++ {
			if match(c.lines[seen]) {
				return c.lines[seen], true
			}
		}
		if c.ended || time.Now().After(deadline) {
			return Line{}, false
		}
		c.cond.Wait()
	}
}

// Event waits for the next event called name from line from on.
func (c *Client) Event(name string, from int, match func(Line) bool) Line {
	c.t.Helper()
	return c.Wait("a "+name+" event", from, func(l Line) bool {
		return l.Event == name && (match == nil || match(l))
	})
}

// Lines is every line read so far.
func (c *Client) Lines() []Line {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Line(nil), c.lines...)
}

// Ended waits up to timeout for the helper to close its stdout.
func (c *Client) Ended(timeout time.Duration) bool {
	c.WaitFor(timeout, math.MaxInt, func(Line) bool { return false })
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ended
}

// Decode unmarshals raw into a T, failing the test if it can't.
func Decode[T any](t testing.TB, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return v
}

// Field decodes one field of a line.
func Field[T any](t testing.TB, l Line, name string) T {
	t.Helper()
	raw, ok := l.Fields[name]
	if !ok {
		t.Fatalf("line has no %q: %s", name, l.Raw)
	}
	return Decode[T](t, raw)
}
