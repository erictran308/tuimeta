// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
	"go.mau.fi/mautrix-meta/pkg/messagix/useragent"

	"github.com/erictran308/tuimeta/helper/internal/download"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// mediaSource is where a file is: Instagram's CDN address for it (with an
// expiring signature), and the message it came in, to find a fresh address
// once that one expires.
type mediaSource struct {
	url  string
	chat int64  // the thread key, 0 for a profile picture
	msg  string // the message's id
}

// cdnHosts are the only sites files are downloaded from: Meta's CDNs. Every
// address a download starts at or is redirected to must be on one, so no
// address from a message's text (or a sender's crafted attachment) is ever
// fetched.
var cdnHosts = []string{"fbcdn.net", "cdninstagram.com", "fbsbx.com"}

// chunkedVideoHost serves some videos only in ranges, as the connector found.
const chunkedVideoHost = "video.xx.fbcdn.net"

const chunkSize = 1 << 20

var (
	errNotCDN  = proto.Err(proto.BadRequest, "That file isn't on Instagram's servers, so it isn't downloaded.")
	errExpired = proto.Err(proto.NetworkError, "Instagram's link to this file has expired; reload the chat and try again.")
	errFetch   = proto.Err(proto.NetworkError, "The download from Instagram failed; try again.")
)

// onCDN reports whether raw is an https address on Meta's CDN.
func onCDN(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range cdnHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// newMediaClient is the client downloads go through: no cookies (the CDN
// addresses carry their own signature), no proxy from the environment, and
// redirects only within the CDN.
func newMediaClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 || !onCDN(req.URL) {
				return errNotCDN
			}
			if strings.EqualFold(req.URL.Hostname(), chunkedVideoHost) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// register gives a file of a message (or a profile picture) its file id; the
// same content keeps its key, and so its id and its download, whatever its
// address; b.mu is held.
func (b *Instagram) register(key, raw, mime string, c *chat, n *netMsg) int32 {
	src := &mediaSource{url: raw}
	if c != nil {
		src.chat = c.key
	}
	if n != nil {
		src.msg = n.netID
	}
	return b.d.Files.Register(ids.FileRef{Network: proto.Instagram, Key: key, Mime: mime, Source: src})
}

// Fetch downloads a file from Instagram's CDN. An address that has expired
// is looked up again in its message once, then tried again.
func (b *Instagram) Fetch(ctx context.Context, ref ids.FileRef, w io.Writer) error {
	src, ok := ref.Source.(*mediaSource)
	if !ok || src == nil {
		return proto.ErrNoFile
	}
	err := b.download(ctx, src.url, ref.Mime, w)
	if !errors.Is(err, errExpired) || src.msg == "" {
		return err
	}
	if rerr := b.refresh(ctx, src); rerr != nil {
		hlog.Warn("instagram: can't refresh a file's address", hlog.Kind(rerr))
		return err
	}
	fresh, ok := b.d.Files.Get(ref.ID)
	if !ok {
		return err
	}
	if s, ok := fresh.Source.(*mediaSource); ok && s.url != src.url {
		return b.download(ctx, s.url, ref.Mime, w)
	}
	return err
}

// download writes the file at raw to w.
func (b *Instagram) download(ctx context.Context, raw, mime string, w io.Writer) error {
	u, err := url.Parse(raw)
	if err != nil || !onCDN(u) {
		return errNotCDN
	}
	resp, err := b.get(ctx, http.MethodGet, raw, mime, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently ||
		resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusTemporaryRedirect:
		loc, lerr := resp.Location()
		if lerr != nil || !onCDN(loc) {
			return errNotCDN
		}
		return b.chunked(ctx, loc.String(), mime, w)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone:
		return errExpired
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return errFetch
	}
	download.SetSize(w, resp.ContentLength)
	_, err = io.Copy(w, resp.Body)
	return err
}

// chunked downloads a video served in ranges.
func (b *Instagram) chunked(ctx context.Context, raw, mime string, w io.Writer) error {
	head, err := b.get(ctx, http.MethodHead, raw, mime, "")
	if err != nil {
		return err
	}
	head.Body.Close()
	if head.StatusCode == http.StatusForbidden {
		return errExpired
	}
	total := head.ContentLength
	if head.StatusCode != http.StatusOK || total <= 0 || head.Header.Get("Accept-Ranges") != "bytes" {
		return b.plain(ctx, raw, mime, w)
	}
	download.SetSize(w, total)
	for offset := int64(0); offset < total; offset += chunkSize {
		end := min(offset+chunkSize, total) - 1
		resp, err := b.get(ctx, http.MethodGet, raw, mime, fmt.Sprintf("bytes=%d-%d", offset, end))
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return errFetch
		}
		_, err = io.Copy(w, io.LimitReader(resp.Body, end-offset+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// plain downloads raw in one piece, following no further redirect.
func (b *Instagram) plain(ctx context.Context, raw, mime string, w io.Writer) error {
	resp, err := b.get(ctx, http.MethodGet, raw, mime, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errFetch
	}
	download.SetSize(w, resp.ContentLength)
	_, err = io.Copy(w, resp.Body)
	return err
}

// get asks the CDN for raw, as the web client's media requests do.
func (b *Instagram) get(ctx context.Context, method, raw, mime, byteRange string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, raw, nil)
	if err != nil {
		return nil, errNotCDN
	}
	req.Header.Set("Accept", "*/*")
	dest := "empty"
	switch strings.Split(mime, "/")[0] {
	case "image":
		req.Header.Set("Accept", "image/avif,image/webp,*/*")
		dest = "image"
	case "video":
		dest = "video"
	case "audio":
		dest = "audio"
	}
	req.Header.Set("Sec-Fetch-Dest", dest)
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("User-Agent", useragent.UserAgent)
	req.Header.Set("Sec-Ch-Ua", useragent.SecCHUserAgent)
	req.Header.Set("Sec-Ch-Ua-Platform", useragent.SecCHPlatform)
	b.mu.Lock()
	as := b.as
	b.mu.Unlock()
	as.Rewrite(req.Header)
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	resp, err := b.media.Do(req)
	if err != nil {
		if errors.Is(err, errNotCDN) {
			return nil, errNotCDN
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		hlog.Warn("instagram: download failed", hlog.Kind(err))
		return nil, errFetch
	}
	return resp, nil
}

// refresh looks a file's message up again, which registers its files anew
// with fresh addresses (their keys, and so their ids, stay the same). The
// page asked for is the one ending just before the next newer message, as
// the connector does, or the newest.
func (b *Instagram) refresh(ctx context.Context, src *mediaSource) error {
	conn, err := b.current()
	if err != nil {
		return err
	}
	b.mu.Lock()
	c := b.chats[src.chat]
	if c == nil || c.igid == "" {
		b.mu.Unlock()
		return proto.ErrNoChat
	}
	igid := c.igid
	target := c.msgs[src.msg]
	next := ""
	if target != nil {
		var best *netMsg
		for _, m := range c.msgs {
			if m.counted && m.ms > target.ms && (best == nil || m.ms < best.ms) {
				best = m
			}
		}
		if best != nil {
			next = best.netID
		}
	}
	b.mu.Unlock()

	var messages *slidetypes.SlideMessages
	if next != "" {
		resp, err := conn.cli.PaginateMessages(withQuietLog(ctx), &slidetypes.PaginateMessagesRequest{
			ThreadID: igid, FirstN: pageSize, OlderThanMessageID: &next, InitialMessagePageCount: pageSize,
		})
		if err != nil {
			return err
		}
		if t := resp.ThreadInfo.AsIGDirectThread; t != nil {
			messages = t.Messages
		}
	} else {
		resp, err := conn.cli.GetThread(withQuietLog(ctx), slidetypes.MakeGetThreadInfoRequest(igid))
		if err != nil {
			return err
		}
		if t := resp.ThreadInfo.AsIGDirectThread; t != nil {
			messages = t.SlideMessages
		}
	}
	if messages == nil {
		return proto.ErrNoMessage
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, edge := range messages.Edges {
		if n := b.convert(c, edge.Node); n != nil && !n.skip {
			old := c.msgs[n.netID]
			n = b.keep(c, n)
			if old != nil && old.inLog {
				b.putLog(c, n)
			}
		}
	}
	return nil
}
