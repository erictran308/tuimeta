// SPDX-License-Identifier: AGPL-3.0-or-later

// Package download fetches files tuimeta asks for, one download per file at
// a time, what's on screen first, into <dir>/<network>/ (0600), reporting
// progress as file events. Nothing is downloaded unless asked for.
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/fsutil"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// MaxSize is the biggest file the helper downloads.
const MaxSize = 200 << 20

// Workers is how many downloads run at once.
const Workers = 4

// ProgressEvery is how often a download reports progress at most.
const ProgressEvery = 50 * time.Millisecond

// Fetch writes a file's bytes to w (a backend's Fetch).
type Fetch func(ctx context.Context, ref ids.FileRef, w io.Writer) error

var errTooBig = proto.Err(proto.BadRequest, "This file is bigger than 200 MB, too big to download here.")

// Manager runs downloads.
type Manager struct {
	dir   string
	files *ids.Files
	fetch func(proto.Network) Fetch
	emit  func(proto.File)

	// Max is the size limit (MaxSize; tests lower it).
	Max int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	cond      *sync.Cond
	high, low []int32
	jobs      map[int32]*job
	closed    bool
}

type job struct {
	ref     ids.FileRef
	prio    proto.Priority
	running bool
	cancel  context.CancelFunc
}

// New starts a manager saving into dir. fetch gives the backend's Fetch for
// a network, emit writes a file event.
func New(dir string, files *ids.Files, fetch func(proto.Network) Fetch, emit func(proto.File)) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		dir: dir, files: files, fetch: fetch, emit: emit, Max: MaxSize,
		ctx: ctx, cancel: cancel, jobs: map[int32]*job{},
	}
	m.cond = sync.NewCond(&m.mu)
	m.removeLeftovers()
	for range Workers {
		m.wg.Add(1)
		hlog.Go("download worker", func() {
			defer m.wg.Done()
			m.work()
		})
	}
	return m
}

// removeLeftovers deletes downloads a crash cut short.
func (m *Manager) removeLeftovers() {
	parts, _ := filepath.Glob(filepath.Join(m.dir, "*", ".part-*"))
	for _, p := range parts {
		_ = os.Remove(p)
	}
}

// Path is where the file is saved: named after its key's hash, so the same
// content is found again next run, and its own name made safe.
func (m *Manager) Path(ref ids.FileRef) string {
	sum := sha256.Sum256([]byte(string(ref.Network) + "\x00" + ref.Key))
	return filepath.Join(m.dir, string(ref.Network), hex.EncodeToString(sum[:10])+"-"+SafeName(ref.Name, ref.Mime))
}

// Download starts fetching file id (or raises its priority). A file already
// on disk is reported done at once.
func (m *Manager) Download(id int32, prio proto.Priority) error {
	ref, ok := m.files.Get(id)
	if !ok {
		return proto.ErrNoFile
	}
	if prio != proto.High {
		prio = proto.Low
	}
	path := m.Path(ref)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		size := info.Size()
		m.emit(proto.File{ID: id, Size: size, Downloaded: size, Done: true, Path: &path})
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return proto.Err(proto.Internal, "The helper is quitting.")
	}
	if j := m.jobs[id]; j != nil {
		if prio == proto.High && j.prio == proto.Low && !j.running {
			j.prio = proto.High
			m.low = slices.DeleteFunc(m.low, func(x int32) bool { return x == id })
			m.high = append(m.high, id)
		}
		return nil
	}
	m.jobs[id] = &job{ref: ref, prio: prio}
	if prio == proto.High {
		m.high = append(m.high, id)
	} else {
		m.low = append(m.low, id)
	}
	m.emit(proto.File{ID: id, Size: ref.Size})
	m.cond.Signal()
	return nil
}

func (m *Manager) work() {
	for {
		m.mu.Lock()
		for !m.closed && len(m.high) == 0 && len(m.low) == 0 {
			m.cond.Wait()
		}
		if m.closed {
			m.mu.Unlock()
			return
		}
		var id int32
		if len(m.high) > 0 {
			id, m.high = m.high[0], m.high[1:]
		} else {
			id, m.low = m.low[0], m.low[1:]
		}
		j := m.jobs[id]
		ctx, cancel := context.WithCancel(m.ctx)
		j.running = true
		j.cancel = cancel
		m.mu.Unlock()

		m.run(ctx, j.ref)
		cancel()

		m.mu.Lock()
		delete(m.jobs, id)
		m.mu.Unlock()
	}
}

func (m *Manager) run(ctx context.Context, ref ids.FileRef) {
	path, size, err := m.save(ctx, ref)
	switch {
	case err == nil:
		m.emit(proto.File{ID: ref.ID, Size: size, Downloaded: size, Done: true, Path: &path})
	case ctx.Err() != nil:
		// Quitting or logged out: nobody is waiting for it.
	default:
		hlog.Warn("download failed", hlog.Str("network", string(ref.Network)), hlog.Kind(err))
		msg := "The download failed; try again."
		var pe *proto.Error
		if errors.As(err, &pe) {
			msg = pe.Message
		}
		m.emit(proto.File{ID: ref.ID, Size: ref.Size, Error: msg})
	}
}

func (m *Manager) save(ctx context.Context, ref ids.FileRef) (string, int64, error) {
	if ref.Size > m.Max {
		return "", 0, errTooBig
	}
	fetch := m.fetch(ref.Network)
	if fetch == nil {
		return "", 0, proto.ErrLoggedOut(ref.Network)
	}
	dir := filepath.Join(m.dir, string(ref.Network))
	if err := fsutil.PrivateDir(dir); err != nil {
		return "", 0, err
	}
	f, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	pw := &progress{w: f, max: m.Max, size: ref.Size}
	pw.report = func(n int64) {
		m.emit(proto.File{ID: ref.ID, Size: pw.size, Downloaded: n})
	}
	err = fetch(ctx, ref, pw)
	if err == nil && pw.n == 0 {
		err = errors.New("empty file")
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return "", 0, err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", 0, err
	}
	path := m.Path(ref)
	if err := os.Rename(tmp, path); err != nil {
		return "", 0, err
	}
	return path, pw.n, nil
}

// progress counts what's written, stops at the size limit, and reports.
type progress struct {
	w            io.Writer
	n, max, size int64
	last         time.Time
	report       func(int64)
}

// SetSize tells the download its full size once the backend learns it (a
// Content-Length), for progress events; w is the writer Fetch was given.
func SetSize(w io.Writer, size int64) {
	if p, ok := w.(*progress); ok && size > 0 {
		p.size = size
	}
}

func (p *progress) Write(b []byte) (int, error) {
	if p.n+int64(len(b)) > p.max {
		return 0, errTooBig
	}
	n, err := p.w.Write(b)
	p.n += int64(n)
	if now := time.Now(); now.Sub(p.last) >= ProgressEvery {
		p.last = now
		p.report(p.n)
	}
	return n, err
}

// Forget cancels the network's downloads and deletes its saved files
// (logout).
func (m *Manager) Forget(n proto.Network) error {
	m.mu.Lock()
	for id, j := range m.jobs {
		if j.ref.Network != n {
			continue
		}
		if j.running {
			j.cancel()
		} else {
			delete(m.jobs, id)
			m.high = slices.DeleteFunc(m.high, func(x int32) bool { return x == id })
			m.low = slices.DeleteFunc(m.low, func(x int32) bool { return x == id })
		}
	}
	m.mu.Unlock()
	return os.RemoveAll(filepath.Join(m.dir, string(n)))
}

// Close stops every download, waiting at most wait for them to end.
func (m *Manager) Close(wait time.Duration) {
	m.mu.Lock()
	m.closed = true
	m.cond.Broadcast()
	m.mu.Unlock()
	m.cancel()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

var extensions = map[string]string{
	"image/jpeg": "jpg", "image/png": "png", "image/gif": "gif", "image/webp": "webp",
	"video/mp4": "mp4", "video/quicktime": "mov", "video/webm": "webm",
	"audio/mp4": "m4a", "audio/mpeg": "mp3", "audio/ogg": "ogg", "audio/aac": "aac",
	"application/pdf": "pdf", "text/plain": "txt", "application/zip": "zip",
}

// SafeName makes a name a sender chose fit for a file name: its last path
// element only, letters, digits, '.', '-' and '_' (others become '_'), not
// starting with a dot, at most 60 bytes, with an extension from mime when it
// has none.
func SafeName(name, mime string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	var b strings.Builder
	under := false
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-'
		if ok {
			b.WriteRune(r)
			under = false
		} else if !under {
			b.WriteByte('_')
			under = true
		}
	}
	name = strings.TrimLeft(b.String(), "._-")
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i+1:]
	}
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	mimeExt := extensions[mime]
	switch {
	case ext == "" || len(ext) > 8:
		stem, ext = name, mimeExt
	case mimeExt != "" && !sameType(strings.ToLower(ext), mimeExt) &&
		(strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/")):
		// A picture, video or recording keeps its type's extension, so one
		// named "x.command" can't open as something else.
		stem, ext = name, mimeExt
	}
	if ext == "" {
		ext = "bin"
	}
	stem = strings.Trim(stem, "._-")
	if len(stem) > 50 {
		stem = strings.TrimRight(stem[:50], "._-")
	}
	if stem == "" {
		stem = "file"
	}
	return stem + "." + strings.ToLower(ext)
}

func sameType(ext, mimeExt string) bool {
	switch ext {
	case mimeExt:
		return true
	case "jpeg", "jpe":
		return mimeExt == "jpg"
	case "m4v", "mp4":
		return mimeExt == "mp4" || mimeExt == "m4a"
	case "oga", "opus":
		return mimeExt == "ogg"
	}
	return false
}
