// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"crypto/tls"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// fakeCDN stands in for Meta's media servers: every connection the media
// client makes goes to a local test server, whatever host it names.
func fakeCDN(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	old := mediaClient.Transport
	mediaClient.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (stdnet.Conn, error) {
			return (&stdnet.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
		// The test server's certificate isn't for Meta's names.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	t.Cleanup(func() { mediaClient.Transport = old })
}

func TestDownloadsFetchMetasURLsOnly(t *testing.T) {
	var hits atomic.Int32
	fakeCDN(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Cookie") != "" {
			t.Error("media request headers")
		}
		w.Write([]byte("photo bytes"))
	})
	h := newHarness(t)
	ok := ids.FileRef{ID: 1, Network: proto.Messenger, Source: &fbSource{URL: "https://scontent.xx.fbcdn.net/p.jpg", Mime: "image/jpeg"}}
	var buf writeBuf
	if err := h.m.Fetch(context.Background(), ok, &buf); err != nil || string(buf) != "photo bytes" {
		t.Fatalf("fetch %q %v", buf, err)
	}
	bad := ids.FileRef{ID: 2, Network: proto.Messenger, Source: &fbSource{URL: "https://tracker.example.com/p.jpg"}}
	if err := h.m.Fetch(context.Background(), bad, &buf); err == nil {
		t.Error("a non-Meta URL was fetched")
	}
	if hits.Load() != 1 {
		t.Errorf("requests %d", hits.Load())
	}
}

func TestVideosRedirectedToTheVideoHostComeInRanges(t *testing.T) {
	video := strings.Repeat("v", chunkSize+10)
	fakeCDN(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "video.xx.fbcdn.net" {
			http.Redirect(w, r, "https://video.xx.fbcdn.net/v.mp4?oe=1", http.StatusFound)
			return
		}
		var from, to int
		if _, err := fmtSscanf(r.Header.Get("Range"), &from, &to); err != nil {
			t.Errorf("range %q", r.Header.Get("Range"))
			return
		}
		to = min(to, len(video)-1)
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(from)+"-"+strconv.Itoa(to)+"/"+strconv.Itoa(len(video)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(video[from : to+1]))
	})
	var buf writeBuf
	if err := fetchURL(context.Background(), "https://scontent.xx.fbcdn.net/v.mp4", "video/mp4", &buf); err != nil || string(buf) != video {
		t.Fatalf("ranges: %d bytes, %v", len(buf), err)
	}
}

func fmtSscanf(header string, from, to *int) (int, error) {
	h := strings.TrimPrefix(header, "bytes=")
	a, b, _ := strings.Cut(h, "-")
	var err error
	if *from, err = strconv.Atoi(a); err != nil {
		return 0, err
	}
	*to, err = strconv.Atoi(b)
	return 2, err
}

func TestAnExpiredURLIsRefreshedFromTheMessage(t *testing.T) {
	fakeCDN(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "old") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte("fresh"))
	})
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	blob := func(url string) *table.LSInsertBlobAttachment {
		return &table.LSInsertBlobAttachment{ThreadKey: aliceID, MessageId: "mid.$pic", AttachmentFbid: "42", AttachmentType: table.AttachmentTypeImage, PlayableUrl: url, PlayableUrlMimeType: "image/jpeg"}
	}
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$pic", aliceID, -1, "")}, LSInsertBlobAttachment: []*table.LSInsertBlobAttachment{blob("https://scontent.xx.fbcdn.net/old.jpg")}})
	id := h.rec.messages()[0].Media.FileID
	h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
		f, ok := tasks[0].(*socket.FetchMessagesTask)
		if !ok || f.ReferenceMessageId != "mid.$pic" || f.ThreadKey != aliceID {
			t.Errorf("refresh task %+v", tasks[0])
		}
		return &table.LSTable{
			LSInsertNewMessageRange: []*table.LSInsertNewMessageRange{{ThreadKey: aliceID, HasMoreBefore: true}},
			LSUpsertMessage:         []*table.LSUpsertMessage{{ThreadKey: aliceID, MessageId: "mid.$pic", SenderId: aliceID, TimestampMs: ms(-1)}},
			LSInsertBlobAttachment:  []*table.LSInsertBlobAttachment{blob("https://scontent.xx.fbcdn.net/new.jpg")},
		}, nil
	}
	ref, _ := h.deps.Files.Get(id)
	var buf writeBuf
	if err := h.m.Fetch(context.Background(), ref, &buf); err != nil || string(buf) != "fresh" {
		t.Fatalf("fetch %q %v", buf, err)
	}
	if now, _ := h.deps.Files.Get(id); now.Source.(*fbSource).URL != "https://scontent.xx.fbcdn.net/new.jpg" || now.Key != ref.Key {
		t.Errorf("the file kept its old address: %+v", now)
	}
}

func TestSmallFilesHereAreServedAsTheyAre(t *testing.T) {
	h := newHarness(t)
	var buf writeBuf
	if err := h.m.Fetch(context.Background(), ids.FileRef{Source: &inlineSource{Data: []byte("thumb")}}, &buf); err != nil || string(buf) != "thumb" {
		t.Errorf("inline %q %v", buf, err)
	}
	if rangeTotal("bytes 0-9/100") != 100 || rangeTotal("bytes */*") != -1 || rangeTotal("") != -1 {
		t.Error("rangeTotal")
	}
	if w, hh := thumbSize(1600, 800, true); w != 400 || hh != 200 {
		t.Errorf("thumb %d %d", w, hh)
	}
	if w, hh := thumbSize(0, 0, true); w != 400 || hh != 400 {
		t.Errorf("unknown photo %d %d", w, hh)
	}
}
