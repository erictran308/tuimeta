// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"

	"go.mau.fi/mautrix-meta/pkg/messagix/methods"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/useragent"

	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/download"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// fbSource is an attachment on Meta's servers: a signed URL that expires,
// and where it came from, to ask for a fresh URL once it has.
type fbSource struct {
	URL          string
	Mime         string
	Expires      int64 // ms; 0 when unknown
	Thread       int64
	MessageID    string
	AttachmentID string
	Part         int
	Preview      bool // the attachment's preview image, not the file
	Legacy       bool
}

// waSource is an encrypted chat's attachment: where it is and its keys.
type waSource struct {
	Integral  *waMediaTransport.WAMediaTransport_Integral
	MediaType whatsmeow.MediaType
}

// inlineSource is a file already here: a thumbnail that came in a message,
// or a file you sent.
type inlineSource struct{ Data []byte }

// mediaHosts are where attachments may be downloaded from: Meta's own
// servers. Anything else (a URL a sender could have chosen) is never
// fetched.
var mediaHosts = []string{"fbcdn.net", "facebook.com", "fbsbx.com", "messenger.com", "cdninstagram.com", "whatsapp.net"}

// allowedMediaURL reports whether raw is an https URL on Meta's servers.
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

// errForbidden is a download refused because its signed URL expired.
var errForbidden = errors.New("forbidden")

// chunkSize is how much of a video is asked for at a time where Messenger
// wants videos fetched in ranges.
const chunkSize = 1 << 20

// mediaClient downloads attachments: no proxy from the environment, Meta's
// hosts only, even on redirects.
var mediaClient = &http.Client{
	Timeout: 5 * time.Minute,
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
		if req.URL.Hostname() == "video.xx.fbcdn.net" {
			// Fetched in ranges, as Messenger's web client does.
			return http.ErrUseLastResponse
		}
		return nil
	},
}

func (m *Messenger) Fetch(ctx context.Context, ref ids.FileRef, w io.Writer) error {
	switch src := ref.Source.(type) {
	case *inlineSource:
		download.SetSize(w, int64(len(src.Data)))
		_, err := w.Write(src.Data)
		return err
	case *waSource:
		e2ee, err := m.encrypted()
		if err != nil {
			return err
		}
		data, err := e2ee.DownloadFB(ctx, src.Integral, src.MediaType)
		if err != nil {
			hlog.Info("messenger: encrypted download failed", hlog.Kind(err))
			return proto.Err(proto.NetworkError, "The encrypted file couldn't be downloaded; try again.")
		}
		download.SetSize(w, int64(len(data)))
		_, err = w.Write(data)
		return err
	case *fbSource:
		m.mu.Lock()
		as := m.as
		m.mu.Unlock()
		u := src.URL
		if src.Expires > 0 && m.now().UnixMilli() > src.Expires-5*60*1000 && src.MessageID != "" {
			if fresh := m.refreshURL(ctx, ref); fresh != "" {
				u = fresh
			}
		}
		err := fetchURL(ctx, as, u, src.Mime, w)
		if errors.Is(err, errForbidden) && src.MessageID != "" {
			if fresh := m.refreshURL(ctx, ref); fresh != "" && fresh != u {
				err = fetchURL(ctx, as, fresh, src.Mime, w)
			}
		}
		if err != nil {
			hlog.Info("messenger: download failed", hlog.Kind(err))
			if errors.Is(err, context.Canceled) {
				return err
			}
			return proto.Err(proto.NetworkError, "The file couldn't be downloaded; try again.")
		}
		return nil
	}
	return proto.ErrNoFile
}

// refreshURL asks Messenger for the message an attachment is in again,
// which brings fresh URLs; the files it holds are registered anew with them.
func (m *Messenger) refreshURL(ctx context.Context, ref ids.FileRef) string {
	src := ref.Source.(*fbSource)
	meta, err := m.connected()
	if err != nil {
		return ""
	}
	ts, _ := methods.ParseMessageID(src.MessageID)
	tbl, err := meta.ExecuteTasks(ctx, &socket.FetchMessagesTask{
		ThreadKey: src.Thread, ReferenceTimestampMs: ts + 1, ReferenceMessageId: src.MessageID,
		SyncGroup: 1, Cursor: meta.Cursor(1),
	})
	if err != nil {
		hlog.Info("messenger: refreshing a file's address failed", hlog.Kind(err))
		return ""
	}
	m.mu.Lock()
	m.applyTable(tbl, fromResponse)
	m.mu.Unlock()
	if now, ok := m.d.Files.Get(ref.ID); ok {
		if s, ok := now.Source.(*fbSource); ok {
			return s.URL
		}
	}
	return ""
}

// fetchURL downloads a file from Meta's servers into w, with the headers a
// browser (the one the session says it is) sends for it.
func fetchURL(ctx context.Context, as browser.Identity, raw, mime string, w io.Writer) error {
	if !allowedMediaURL(raw) {
		return errors.New("not a media host")
	}
	resp, err := mediaGet(ctx, as, raw, mime, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently {
		loc, lerr := resp.Location()
		if lerr == nil && loc.Hostname() == "video.xx.fbcdn.net" && allowedMediaURL(loc.String()) {
			return fetchRanges(ctx, as, loc.String(), mime, w)
		}
	}
	if resp.StatusCode == http.StatusForbidden {
		return errForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	download.SetSize(w, resp.ContentLength)
	_, err = io.Copy(w, resp.Body)
	return err
}

// fetchRanges downloads a video a range at a time.
func fetchRanges(ctx context.Context, as browser.Identity, raw, mime string, w io.Writer) error {
	var offset, total int64 = 0, -1
	for total < 0 || offset < total {
		resp, err := mediaGet(ctx, as, raw, mime, "bytes="+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(offset+chunkSize-1, 10))
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			return errForbidden
		}
		if resp.StatusCode == http.StatusOK {
			// The server ignored the range: this is the whole file.
			download.SetSize(w, resp.ContentLength)
			_, err = io.Copy(w, resp.Body)
			resp.Body.Close()
			return err
		}
		if resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		if total < 0 {
			total = rangeTotal(resp.Header.Get("Content-Range"))
			if total <= 0 {
				resp.Body.Close()
				return errors.New("no content range")
			}
			download.SetSize(w, total)
		}
		n, err := io.Copy(w, resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("empty range")
		}
		offset += n
	}
	return nil
}

// rangeTotal is the full size a Content-Range header gives ("bytes 0-9/100").
func rangeTotal(h string) int64 {
	_, total, ok := strings.Cut(h, "/")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func mediaGet(ctx context.Context, as browser.Identity, raw, mime, byteRange string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Accept", "*/*")
	switch strings.SplitN(mime, "/", 2)[0] {
	case "image":
		h.Set("Accept", "image/avif,image/webp,*/*")
		h.Set("Sec-Fetch-Dest", "image")
	case "video":
		h.Set("Sec-Fetch-Dest", "video")
	case "audio":
		h.Set("Sec-Fetch-Dest", "audio")
	default:
		h.Set("Sec-Fetch-Dest", "empty")
	}
	h.Set("Sec-Fetch-Mode", "no-cors")
	h.Set("Sec-Fetch-Site", "cross-site")
	h.Set("User-Agent", useragent.UserAgent)
	h.Set("sec-ch-ua", useragent.SecCHUserAgent)
	h.Set("sec-ch-ua-platform", useragent.SecCHPlatform)
	as.Rewrite(h)
	if byteRange != "" {
		h.Set("Range", byteRange)
	}
	return mediaClient.Do(req)
}
