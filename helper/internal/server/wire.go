// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// MaxLine is the longest request line read; a longer one is skipped and
// answered bad_request.
const MaxLine = 16 << 20

// MaxOutLine is the longest line the helper writes (PROTOCOL.md): tuimeta
// reads lines of up to 16 MiB, and a longer one costs it the request it
// answers. An answer that would be longer is sent as an internal error for
// its id instead, and an event is dropped.
const MaxOutLine = 8 << 20

var errTooLong = proto.Err(proto.Internal, "That answer was too big to send to tuimeta.")

// writer is the one goroutine that writes stdout, so lines never mix. It
// writes in batches, calling before (which keeps new ids on disk) once per
// batch, ahead of the lines that may carry them.
type writer struct {
	w      *bufio.Writer
	lines  chan []byte
	stop   chan struct{}
	done   chan struct{}
	before func()
	broken bool
}

func newWriter(w io.Writer, first any, before func()) *writer {
	wr := &writer{
		w:      bufio.NewWriterSize(w, 64<<10),
		lines:  make(chan []byte, 4096),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		before: before,
	}
	line, err := encode(first)
	if err != nil {
		panic("server: can't encode the first line")
	}
	go wr.loop(line)
	return wr
}

func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // Encode ends the line with '\n'
		return nil, err
	}
	return b.Bytes(), nil
}

// send writes v as one line. It waits while the batch ahead is written (so
// a reader that stops reading slows the helper rather than growing memory),
// and drops the line once the writer has stopped.
func (wr *writer) send(v any) {
	line, err := encode(v)
	if err == nil && len(line) > MaxOutLine {
		r, answer := v.(response)
		if !answer {
			hlog.Warn("dropped an event too long for tuimeta", hlog.Str("type", fmt.Sprintf("%T", v)), hlog.Int("bytes", int64(len(line))))
			return
		}
		hlog.Warn("an answer too long for tuimeta became an error", hlog.Int("bytes", int64(len(line))))
		line, err = encode(response{ID: r.ID, Error: errTooLong})
	}
	if err != nil {
		hlog.Error("can't encode a line", hlog.Kind(err))
		return
	}
	select {
	case wr.lines <- line:
	case <-wr.done:
	}
}

func (wr *writer) loop(first []byte) {
	defer close(wr.done)
	wr.write([][]byte{first})
	batch := make([][]byte, 0, 256)
	for {
		select {
		case line := <-wr.lines:
			batch = append(batch[:0], line)
			batch = wr.drain(batch, 255)
			wr.write(batch)
		case <-wr.stop:
			wr.write(wr.drain(batch[:0], len(wr.lines)))
			return
		}
	}
}

func (wr *writer) drain(batch [][]byte, n int) [][]byte {
	for range n {
		select {
		case line := <-wr.lines:
			batch = append(batch, line)
		default:
			return batch
		}
	}
	return batch
}

func (wr *writer) write(batch [][]byte) {
	if len(batch) == 0 || wr.broken {
		return
	}
	if wr.before != nil {
		wr.before()
	}
	for _, line := range batch {
		if _, err := wr.w.Write(line); err != nil {
			wr.fail(err)
			return
		}
	}
	if err := wr.w.Flush(); err != nil {
		wr.fail(err)
	}
}

func (wr *writer) fail(err error) {
	// tuimeta stopped reading; keep draining so nobody blocks on send.
	wr.broken = true
	hlog.Warn("stdout closed", hlog.Kind(err))
}

// close writes what's queued and stops, waiting at most wait.
func (wr *writer) close(wait time.Duration) {
	select {
	case <-wr.stop:
	default:
		close(wr.stop)
	}
	select {
	case <-wr.done:
	case <-time.After(wait):
	}
}

// readLines calls handle with each line of r (without its line ending),
// skipping blank ones, and tooLong for each line over max, until r ends.
func readLines(r io.Reader, max int, handle func([]byte), tooLong func()) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !skipping {
			if len(buf)+len(bytes.TrimRight(chunk, "\r\n")) > max {
				skipping = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if skipping {
			tooLong()
			skipping = false
		} else if line := bytes.TrimSpace(buf); len(line) > 0 {
			handle(line)
		}
		buf = nil
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
	}
}
