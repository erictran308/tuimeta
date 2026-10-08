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

// readUploads reads the files to send, each once and now: what's sent is
// what was there when the request came. Only absolute paths of plain files
// on this machine are read.
func readUploads(paths []string) ([]backend.Upload, error) {
	if len(paths) == 0 {
		return nil, proto.Err(proto.BadRequest, "There are no files to send.")
	}
	if len(paths) > MaxFiles {
		return nil, proto.Err(proto.BadRequest, "Send at most 32 files at once.")
	}
	var total int64
	uploads := make([]backend.Upload, 0, len(paths))
	for _, path := range paths {
		u, err := readUpload(path)
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

func readUpload(path string) (backend.Upload, error) {
	// A \\server\share path would make Windows hand the server the user's
	// login hash just by looking at it.
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return backend.Upload{}, proto.Err(proto.BadRequest, "Files on another machine can't be sent; copy them here first.")
	}
	if !filepath.IsAbs(path) {
		return backend.Upload{}, proto.Err(proto.BadRequest, "Files to send need absolute paths.")
	}
	tooBig := proto.Err(proto.BadRequest, "A file is bigger than 100 MB, too big to send.")
	info, err := os.Stat(path)
	if err != nil {
		return backend.Upload{}, openError(err)
	}
	if !info.Mode().IsRegular() {
		return backend.Upload{}, proto.Err(proto.BadRequest, "Only plain files can be sent.")
	}
	if info.Size() > MaxUpload {
		return backend.Upload{}, tooBig
	}
	// Non-blocking, so a file swapped for a pipe since Stat can't hang here.
	f, err := os.OpenFile(path, os.O_RDONLY|openNonBlock, 0)
	if err != nil {
		return backend.Upload{}, openError(err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return backend.Upload{}, proto.Err(proto.BadRequest, "Only plain files can be sent.")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxUpload+1))
	if err != nil {
		return backend.Upload{}, openError(err)
	}
	if len(data) > MaxUpload {
		return backend.Upload{}, tooBig
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

func openError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return proto.Err(proto.NotFound, "A file to send isn't there any more.")
	}
	if errors.Is(err, os.ErrPermission) {
		return proto.Err(proto.BadRequest, "A file to send can't be read: permission denied.")
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
