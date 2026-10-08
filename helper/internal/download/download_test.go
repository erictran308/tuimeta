// SPDX-License-Identifier: AGPL-3.0-or-later

package download

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestSafeNameKeepsOnlyHarmlessCharacters(t *testing.T) {
	cases := map[[2]string]string{
		{"IMG_0001.JPG", "image/jpeg"}:             "IMG_0001.jpg",
		{"../../etc/passwd", ""}:                   "passwd.bin",
		{`C:\Users\x\evil.pdf`, "application/pdf"}: "evil.pdf",
		{".hidden", "text/plain"}:                  "hidden.txt",
		{"", "image/png"}:                          "file.png",
		{"photo.command", "image/jpeg"}:            "photo.command.jpg",
		{"photo.jpeg", "image/jpeg"}:               "photo.jpeg",
		{"résumé ‮gpj.exe", "application/zip"}:     "r_sum_gpj.exe",
		{"voice", "audio/mp4"}:                     "voice.m4a",
		{strings.Repeat("a", 300) + ".pdf", ""}:    strings.Repeat("a", 50) + ".pdf",
	}
	for in, want := range cases {
		if got := SafeName(in[0], in[1]); got != want {
			t.Errorf("SafeName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

type recorder struct {
	mu     sync.Mutex
	events []proto.File
}

func (r *recorder) emit(f proto.File) {
	r.mu.Lock()
	r.events = append(r.events, f)
	r.mu.Unlock()
}

func (r *recorder) wait(t *testing.T, id int32, done func(proto.File) bool) proto.File {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, f := range r.events {
			if f.ID == id && done(f) {
				r.mu.Unlock()
				return f
			}
		}
		r.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no matching file event for %d", id)
	return proto.File{}
}

func TestADownloadIsSavedPrivatelyThenServedFromDisk(t *testing.T) {
	dir := t.TempDir()
	files := ids.NewFiles()
	id := files.Register(ids.FileRef{Network: proto.Messenger, Key: "photo-1", Name: "a.png", Mime: "image/png"})
	var fetches atomic.Int32
	fetch := func(ctx context.Context, ref ids.FileRef, w io.Writer) error {
		fetches.Add(1)
		SetSize(w, 8)
		_, err := io.WriteString(w, "PNGBYTES")
		return err
	}
	rec := &recorder{}
	m := New(dir, files, func(proto.Network) Fetch { return fetch }, rec.emit)
	defer m.Close(time.Second)

	if err := m.Download(id, proto.High); err != nil {
		t.Fatal(err)
	}
	done := rec.wait(t, id, func(f proto.File) bool { return f.Done })
	if done.Path == nil || done.Size != 8 || done.Downloaded != 8 {
		t.Fatalf("done = %+v", done)
	}
	info, err := os.Stat(*done.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v %v", info, err)
	}
	if !strings.HasPrefix(*done.Path, dir) {
		t.Errorf("saved outside the folder: %s", *done.Path)
	}

	rec.events = nil
	if err := m.Download(id, proto.Low); err != nil {
		t.Fatal(err)
	}
	again := rec.wait(t, id, func(f proto.File) bool { return f.Done })
	if *again.Path != *done.Path || fetches.Load() != 1 {
		t.Errorf("fetched again: %d fetches, %+v", fetches.Load(), again)
	}
}

func TestOneDownloadPerFileAtATime(t *testing.T) {
	files := ids.NewFiles()
	id := files.Register(ids.FileRef{Network: proto.Messenger, Key: "slow"})
	release := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(ctx context.Context, ref ids.FileRef, w io.Writer) error {
		fetches.Add(1)
		<-release
		_, err := w.Write([]byte("x"))
		return err
	}
	rec := &recorder{}
	m := New(t.TempDir(), files, func(proto.Network) Fetch { return fetch }, rec.emit)
	defer m.Close(time.Second)
	for range 5 {
		if err := m.Download(id, proto.Low); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Download(id, proto.High); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	rec.wait(t, id, func(f proto.File) bool { return f.Done })
	if n := fetches.Load(); n != 1 {
		t.Errorf("%d fetches", n)
	}
}

func TestTooBigAndFailedDownloadsSayWhy(t *testing.T) {
	files := ids.NewFiles()
	big := files.Register(ids.FileRef{Network: proto.Messenger, Key: "big", Size: MaxSize + 1})
	grows := files.Register(ids.FileRef{Network: proto.Messenger, Key: "grows"})
	broken := files.Register(ids.FileRef{Network: proto.Messenger, Key: "broken"})
	fetch := func(ctx context.Context, ref ids.FileRef, w io.Writer) error {
		if ref.Key == "broken" {
			return io.ErrUnexpectedEOF
		}
		_, err := w.Write(make([]byte, 64))
		return err
	}
	rec := &recorder{}
	m := New(t.TempDir(), files, func(proto.Network) Fetch { return fetch }, rec.emit)
	m.Max = 32
	defer m.Close(time.Second)
	for _, id := range []int32{big, grows, broken} {
		if err := m.Download(id, proto.High); err != nil {
			t.Fatal(err)
		}
		f := rec.wait(t, id, func(f proto.File) bool { return f.Error != "" })
		if f.Done || f.Path != nil {
			t.Errorf("failed download %d: %+v", id, f)
		}
	}
	if err := m.Download(9999, proto.High); err == nil {
		t.Error("an unknown file id was accepted")
	}
}
