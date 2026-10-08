// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/gif" // decoders for making thumbnails of photos to send
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waTypes "go.mau.fi/whatsmeow/types"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/download"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// inlineSource is a file carried in the message itself (a thumbnail).
type inlineSource []byte

// avatarSource is a person's or group's picture, looked up when it's
// downloaded.
type avatarSource struct{ jid string }

var (
	errNoPicture = proto.Err(proto.NotFound, "There's no picture to show.")
	errGone      = proto.Err(proto.NotFound, "This file is no longer on WhatsApp's servers; open it on your phone.")
)

// Fetch writes a file's bytes, for a download: an attachment, downloaded
// and decrypted by whatsmeow from WhatsApp's media servers; a thumbnail the
// message carried; or a profile picture.
func (w *WhatsApp) Fetch(ctx context.Context, ref ids.FileRef, out io.Writer) error {
	switch src := ref.Source.(type) {
	case inlineSource:
		download.SetSize(out, int64(len(src)))
		_, err := out.Write(src)
		return err
	case *media:
		if src.ViewOnce || src.DirectPath == "" {
			return proto.ErrNoFile
		}
		limit := downloadLimit(src)
		if src.Size > limit {
			return proto.Err(proto.Unsupported, "That file is too big for tuimeta to download; open it on your phone.")
		}
		cli, err := w.connected()
		if err != nil {
			return err
		}
		dir, err := w.d.Session.Dir()
		if err != nil {
			return err
		}
		if src.Size > 0 {
			download.SetSize(out, src.Size)
		}
		if err := cli.Download(ctx, src, dir, limit, out); err != nil {
			hlog.Info("whatsapp: download failed", hlog.Kind(err))
			switch {
			case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404), errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
				return errGone
			case errors.Is(err, errTooBig):
				return proto.Err(proto.Unsupported, "That file is too big for tuimeta to download; open it on your phone.")
			}
			return requestError(err)
		}
		return nil
	case avatarSource:
		return w.fetchAvatar(ctx, src, out)
	}
	return proto.ErrNoFile
}

// Photos and stickers are drawn from the file itself, downloaded as soon as
// they're on screen: what a sender can make you fetch by scrolling past is
// bounded, whatever size their message claims.
const (
	MaxPhoto   = 16 << 20
	MaxSticker = 2 << 20
)

// downloadLimit is the most of src that's downloaded.
func downloadLimit(src *media) int64 {
	switch {
	case src.Kind == proto.Sticker:
		return MaxSticker
	case src.Kind == proto.Photo, src.Kind == proto.GIF && strings.HasPrefix(src.Mime, "image/"):
		return MaxPhoto
	}
	return download.MaxSize
}

// fetchAvatar asks WhatsApp where a picture is and downloads it.
func (w *WhatsApp) fetchAvatar(ctx context.Context, src avatarSource, out io.Writer) error {
	cli, err := w.connected()
	if err != nil {
		return err
	}
	jid, err := waTypes.ParseJID(src.jid)
	if err != nil {
		return proto.ErrNoFile
	}
	info, err := cli.ProfilePicture(ctx, jid)
	if err != nil || info == nil || info.URL == "" {
		if err != nil && !errors.Is(err, whatsmeow.ErrProfilePictureNotSet) && !errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized) {
			hlog.Info("whatsapp: picture lookup failed", hlog.Kind(err))
		}
		return errNoPicture
	}
	if !allowedMediaURL(info.URL) {
		return errNoPicture
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return errNoPicture
	}
	resp, err := mediaClient.Do(req)
	if err != nil {
		hlog.Info("whatsapp: picture download failed", hlog.Kind(err))
		return proto.Err(proto.NetworkError, "The picture couldn't be downloaded.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errNoPicture
	}
	if resp.ContentLength > 0 {
		download.SetSize(out, resp.ContentLength)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

// mediaHosts are where WhatsApp keeps pictures.
var mediaHosts = []string{"whatsapp.net"}

// allowedMediaURL reports whether raw is an https address on WhatsApp's
// media hosts.
func allowedMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range mediaHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// mediaClient fetches pictures: no proxy, no cookies, and redirects only to
// WhatsApp's hosts.
var mediaClient = &http.Client{
	Timeout: 2 * time.Minute,
	Transport: &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !allowedMediaURL(req.URL.String()) {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

// fileMessage uploads a file to WhatsApp's media servers (encrypted on the
// way) and wraps it as the right kind of message. Photos WhatsApp's apps
// show go as photos, MP4 videos as videos, recordings as audio, and
// everything else (GIFs and WebP pictures included) as a document, which
// every app can open.
func (w *WhatsApp) fileMessage(ctx context.Context, cli waAPI, f backend.Upload, caption string, ci *waE2E.ContextInfo) (*waE2E.Message, error) {
	kind := f.Kind
	switch {
	case kind == proto.Photo && f.Mime != "image/jpeg" && f.Mime != "image/png":
		kind = proto.FileMedia
	case kind == proto.Video && f.Mime != "video/mp4":
		kind = proto.FileMedia
	case kind == proto.GIF:
		kind = proto.FileMedia
	}
	mediaType := whatsmeow.MediaDocument
	switch kind {
	case proto.Photo:
		mediaType = whatsmeow.MediaImage
	case proto.Video:
		mediaType = whatsmeow.MediaVideo
	case proto.Audio, proto.Voice:
		mediaType = whatsmeow.MediaAudio
	}
	up, err := cli.Upload(ctx, f.Data, mediaType)
	if err != nil {
		hlog.Info("whatsapp: upload failed", hlog.Kind(err))
		return nil, proto.Err(proto.NetworkError, "A file couldn't be uploaded to WhatsApp; try again.")
	}
	var cap *string
	if caption != "" {
		cap = gproto.String(caption)
	}
	size := gproto.Uint64(uint64(len(f.Data)))
	switch kind {
	case proto.Photo:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: gproto.String(up.URL), DirectPath: gproto.String(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: size,
			Mimetype: gproto.String(f.Mime), Width: gproto.Uint32(uint32(f.Width)), Height: gproto.Uint32(uint32(f.Height)),
			JPEGThumbnail: thumbnail(f.Data), Caption: cap, ContextInfo: ci,
		}}, nil
	case proto.Video:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: gproto.String(up.URL), DirectPath: gproto.String(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: size,
			Mimetype: gproto.String(f.Mime), Caption: cap, ContextInfo: ci,
		}}, nil
	case proto.Audio, proto.Voice:
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL: gproto.String(up.URL), DirectPath: gproto.String(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: size,
			Mimetype: gproto.String(f.Mime), PTT: gproto.Bool(kind == proto.Voice), ContextInfo: ci,
		}}, nil
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		URL: gproto.String(up.URL), DirectPath: gproto.String(up.DirectPath), MediaKey: up.MediaKey,
		FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: size,
		Mimetype: gproto.String(f.Mime), FileName: gproto.String(f.Name), Title: gproto.String(f.Name),
		Caption: cap, ContextInfo: ci,
	}}, nil
}

// thumbSide is the longest side of the thumbnail sent with a photo, which
// the other side's app shows until the photo itself is downloaded.
const thumbSide = 72

// thumbnail is a small JPEG of a JPEG or PNG photo, or nil if it can't be
// read. A picture past 40 megapixels isn't decoded at all.
func thumbnail(data []byte) []byte {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > 40_000_000 {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	b := img.Bounds()
	tw, th := thumbSide, thumbSide
	if b.Dx() >= b.Dy() {
		th = max(1, b.Dy()*thumbSide/b.Dx())
	} else {
		tw = max(1, b.Dx()*thumbSide/b.Dy())
	}
	small := image.NewRGBA(image.Rect(0, 0, tw, th))
	// Each pixel of the thumbnail is the average of the block it covers.
	for y := range th {
		y0, y1 := b.Min.Y+y*b.Dy()/th, b.Min.Y+(y+1)*b.Dy()/th
		for x := range tw {
			x0, x1 := b.Min.X+x*b.Dx()/tw, b.Min.X+(x+1)*b.Dx()/tw
			var r, g, bl, a, n uint64
			for yy := y0; yy < max(y1, y0+1); yy++ {
				for xx := x0; xx < max(x1, x0+1); xx++ {
					cr, cg, cb, ca := img.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			i := small.PixOffset(x, y)
			small.Pix[i], small.Pix[i+1], small.Pix[i+2], small.Pix[i+3] = uint8(r/n>>8), uint8(g/n>>8), uint8(bl/n>>8), uint8(a/n>>8)
		}
	}
	var buf bytes.Buffer
	if jpeg.Encode(&buf, small, &jpeg.Options{Quality: 60}) != nil {
		return nil
	}
	return buf.Bytes()
}
