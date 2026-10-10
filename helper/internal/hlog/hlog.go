// SPDX-License-Identifier: AGPL-3.0-or-later

// Package hlog writes helper.log. It records connection states and the kinds
// of errors, never their text: library errors can quote URLs with tokens,
// and nothing a person wrote, nor any name, cookie or key, may reach the log.
// So Str is for fixed words (states, methods, networks) only, errors are
// logged with Kind, and panics with Recovered.
package hlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/fsutil"
)

// MaxSize is how big helper.log grows before it's moved to helper.log.1.
const MaxSize = 4 << 20

var (
	mu  sync.Mutex
	out io.Writer = io.Discard
)

// Open sends the log to path (0600, appended), starting a new file when the
// old one is over MaxSize. A link in its place is refused, not written
// through.
func Open(path string) (io.Closer, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() > MaxSize {
		_ = os.Rename(path, path+".1")
	}
	f, err := fsutil.OpenPrivate(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	SetOutput(f)
	return f, nil
}

// SetOutput sends the log to w (tests).
func SetOutput(w io.Writer) {
	mu.Lock()
	out = w
	mu.Unlock()
}

// Attr is one key=value on a log line.
type Attr struct{ key, val string }

// Str is for fixed words only: a state, a method, a network, a step.
func Str(key, val string) Attr { return Attr{key, val} }

// Int is a number: a count, a size, an id the helper assigned.
func Int(key string, v int64) Attr { return Attr{key, fmt.Sprint(v)} }

// Kind names what kind of error err is, without its text.
func Kind(err error) Attr { return Attr{"err", ErrKind(err)} }

// ErrKind is err's code when it has one, else the Go types it's made of.
func ErrKind(err error) string {
	if err == nil {
		return "none"
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, os.ErrNotExist):
		return "not_exist"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	}
	var kinds []string
	for e := err; e != nil && len(kinds) < 4; e = errors.Unwrap(e) {
		kinds = append(kinds, fmt.Sprintf("%T", e))
	}
	return strings.Join(kinds, "<")
}

func Info(msg string, attrs ...Attr)  { write("INFO", msg, attrs) }
func Warn(msg string, attrs ...Attr)  { write("WARN", msg, attrs) }
func Error(msg string, attrs ...Attr) { write("ERROR", msg, attrs) }

func write(level, msg string, attrs []Attr) {
	var b strings.Builder
	b.WriteString(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	b.WriteByte(' ')
	b.WriteString(level)
	b.WriteByte(' ')
	b.WriteString(oneLine(msg))
	for _, a := range attrs {
		b.WriteByte(' ')
		b.WriteString(oneLine(a.key))
		b.WriteByte('=')
		b.WriteString(oneLine(a.val))
	}
	b.WriteByte('\n')
	mu.Lock()
	defer mu.Unlock()
	_, _ = io.WriteString(out, b.String())
}

func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// Recovered logs a panic recovered in where: the panic value's type and the
// function and line that panicked, never the value itself, which may quote a
// message. Call it from the deferred function that called recover.
func Recovered(where string, v any) {
	Error("panic", Str("in", where), Str("type", fmt.Sprintf("%T", v)), Str("at", PanicSite()))
}

// PanicSite is the first function outside the runtime below the panic that
// is unwinding, as "pkg.Func (file.go:12)".
func PanicSite() string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	panicking := false
	for {
		f, more := frames.Next()
		if f.Function == "runtime.gopanic" {
			panicking = true
		} else if panicking && !strings.HasPrefix(f.Function, "runtime.") {
			return fmt.Sprintf("%s (%s:%d)", f.Function, filepath.Base(f.File), f.Line)
		}
		if !more {
			return "unknown"
		}
	}
}

// Go runs f on its own goroutine; a panic in it is logged and goes no
// further. Backends start their goroutines with it.
func Go(where string, f func()) {
	go func() {
		defer func() {
			if v := recover(); v != nil {
				Recovered(where, v)
			}
		}()
		f()
	}()
}
