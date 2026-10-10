// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"errors"
	"image"
	_ "image/gif" // DecodeConfig reads these headers for a photo's size
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Limits on send_files.
const (
	MaxFiles      = 32
	MaxUpload     = 100 << 20 // one file
	MaxUploadsAll = 200 << 20 // all of a request's files
)

// UploadFile is one file send_files names: its path, and what tuimeta saw
// there, without following a link, when it listed the file in the composer
// and checked it on Enter. Dev, Ino and CtimeNs are 0 where the system has
// none (Windows); the times are nanoseconds since 1970.
type UploadFile struct {
	Path    string `json:"path"`
	Dev     uint64 `json:"dev"`
	Ino     uint64 `json:"ino"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns"`
}

var errChanged = proto.Err(proto.BadRequest, "A file changed since it was attached; attach it again.")

// Listed is what tuimeta sends for the file at path when it lists it in the
// composer: its identity, read without following a link. Tests drive
// send_files with it.
func Listed(path string) (UploadFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return UploadFile{}, err
	}
	dev, ino, ctime, _ := systemIdentity(info)
	return UploadFile{Path: path, Dev: dev, Ino: ino, Size: info.Size(), MtimeNs: info.ModTime().UnixNano(), CtimeNs: ctime}, nil
}

// readUploads reads the files to send, each once and now: what's sent is
// what was there when the request came, and only if it's what tuimeta
// listed. Only absolute paths of plain files on this machine are read.
func readUploads(files []UploadFile) ([]backend.Upload, error) {
	if len(files) == 0 {
		return nil, proto.Err(proto.BadRequest, "There are no files to send.")
	}
	if len(files) > MaxFiles {
		return nil, proto.Err(proto.BadRequest, "Send at most 32 files at once.")
	}
	var total int64
	uploads := make([]backend.Upload, 0, len(files))
	for _, f := range files {
		u, err := readUpload(f)
		if err != nil {
			return nil, err
		}
		total += int64(len(u.Data))
		if total > MaxUploadsAll {
			return nil, proto.Err(proto.BadRequest, "These files add up to more than 200 MB; send fewer at once.")
		}
		uploads = append(uploads, u)
	}
	return uploads, nil
}

// readUpload reads the file tuimeta listed, or nothing. tuimeta checked the
// file on Enter, but another user who can rename things in its folder could
// put a link to a file of yours (or another file) in its place before the
// helper reads it. So the path is opened without following a link, and the
// open file itself must be the one listed (its device, inode, size and
// times) and give exactly its size; the name is never looked at again.
func readUpload(want UploadFile) (backend.Upload, error) {
	path := want.Path
	// A \\server\share path would make Windows hand the server the user's
	// login hash just by looking at it.
	if onAnotherMachine(path) {
		return backend.Upload{}, proto.Err(proto.BadRequest, "Files on another machine can't be sent; copy them here first.")
	}
	if !filepath.IsAbs(path) {
		return backend.Upload{}, proto.Err(proto.BadRequest, "Files to send need absolute paths.")
	}
	tooBig := proto.Err(proto.BadRequest, "A file is bigger than 100 MB, too big to send.")
	if want.Size > MaxUpload {
		return backend.Upload{}, tooBig
	}
	f, err := openUpload(path)
	if err != nil {
		return backend.Upload{}, openError(path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return backend.Upload{}, openError(path, err)
	}
	if !info.Mode().IsRegular() || !sameFile(info, want) {
		return backend.Upload{}, errChanged
	}
	data, err := io.ReadAll(io.LimitReader(f, want.Size+1))
	if err != nil {
		return backend.Upload{}, openError(path, err)
	}
	// Written to since, in place, keeping its times: not what was listed.
	if int64(len(data)) != want.Size {
		return backend.Upload{}, errChanged
	}
	if len(data) == 0 {
		return backend.Upload{}, proto.Err(proto.BadRequest, "A file to send is empty.")
	}
	u := backend.Upload{Name: filepath.Base(path), Data: data}
	u.Mime = sniff(u.Name, data)
	u.Kind = kindOf(u.Mime)
	if u.Kind == proto.Photo || u.Kind == proto.GIF {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			u.Width, u.Height = cfg.Width, cfg.Height
		}
	}
	return u, nil
}

// sameFile reports whether the open file info describes is the one tuimeta
// listed: the same size and modification time, and, where tuimeta could
// tell them, the same device, inode and status-change time (which a rewrite
// that puts the old modification time back still changes).
func sameFile(info os.FileInfo, want UploadFile) bool {
	if info.Size() != want.Size || info.ModTime().UnixNano() != want.MtimeNs {
		return false
	}
	dev, ino, ctime, known := systemIdentity(info)
	same := func(have, want uint64) bool { return want == 0 || known && have == want }
	return same(dev, want.Dev) && same(ino, want.Ino) && same(uint64(ctime), uint64(want.CtimeNs))
}

// onAnotherMachine reports whether path reaches past this machine's own
// disks: a share (\\server\share, //server/share, \\?\UNC\…) or a device or
// object path (\\.\…, \??\…). A verbatim drive path, \\?\C:\…, is local:
// it's what tuimeta's canonical paths look like on Windows.
func onAnotherMachine(path string) bool {
	if rest, ok := strings.CutPrefix(path, `\\?\`); ok {
		letter := len(rest) >= 3 && (rest[0] >= 'a' && rest[0] <= 'z' || rest[0] >= 'A' && rest[0] <= 'Z')
		return !(letter && rest[1] == ':' && rest[2] == '\\')
	}
	sep := func(i int) bool { return len(path) > i && (path[i] == '\\' || path[i] == '/') }
	if sep(0) && sep(1) {
		return true
	}
	return sep(0) && strings.HasPrefix(path[1:], "??") && sep(3)
}

func openError(path string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return proto.Err(proto.NotFound, "A file to send isn't there any more.")
	}
	if errors.Is(err, os.ErrPermission) {
		return proto.Err(proto.BadRequest, "A file to send can't be read: permission denied.")
	}
	// Opening without following a link fails on one (ELOOP, or EMLINK or
	// EFTYPE on some BSDs): something took the listed file's place.
	if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		return errChanged
	}
	return proto.Err(proto.BadRequest, "A file to send can't be read.")
}

var byExtension = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".heic": "image/heic", ".mp4": "video/mp4", ".mov": "video/quicktime",
	".webm": "video/webm", ".m4a": "audio/mp4", ".mp3": "audio/mpeg", ".ogg": "audio/ogg",
	".opus": "audio/ogg", ".wav": "audio/wav", ".pdf": "application/pdf", ".txt": "text/plain",
	".zip": "application/zip",
}

// sniff is the file's type by its content, else its extension. Nothing is
// read from the system's MIME tables.
func sniff(name string, data []byte) string {
	mime := http.DetectContentType(data)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	if mime == "application/octet-stream" || mime == "text/plain" {
		if byExt, ok := byExtension[strings.ToLower(filepath.Ext(name))]; ok {
			return byExt
		}
	}
	return mime
}

func kindOf(mime string) proto.MediaKind {
	switch {
	case mime == "image/gif":
		return proto.GIF
	case mime == "image/jpeg" || mime == "image/png" || mime == "image/webp":
		return proto.Photo
	case strings.HasPrefix(mime, "video/"):
		return proto.Video
	case strings.HasPrefix(mime, "audio/"):
		return proto.Audio
	}
	return proto.FileMedia
}
