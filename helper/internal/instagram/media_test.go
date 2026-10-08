// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"

	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// cdnStub answers downloads from canned responses, by URL.
type cdnStub struct {
	mu        sync.Mutex
	responses map[string]func(*http.Request) *http.Response
	asked     []string
}

func (s *cdnStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.asked = append(s.asked, req.Method+" "+req.URL.String())
	f := s.responses[req.URL.String()]
	s.mu.Unlock()
	if f == nil {
		return nil, errors.New("not stubbed")
	}
	return f(req), nil
}

func respond(status int, body string, header ...string) func(*http.Request) *http.Response {
	return func(req *http.Request) *http.Response {
		h := http.Header{}
		for i := 0; i+1 < len(header); i += 2 {
			h.Set(header[i], header[i+1])
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)), Request: req}
	}
}

func (h *harness) stubCDN() *cdnStub {
	stub := &cdnStub{responses: map[string]func(*http.Request) *http.Response{}}
	client := newMediaClient()
	client.Transport = stub
	h.b.media = client
	return stub
}

func TestDownloadsComeOnlyFromInstagramsCDN(t *testing.T) {
	h := loaded(t)
	stub := h.stubCDN()
	good := cdn + "/v/t51/photo_n.jpg?oh=sig&oe=1"
	stub.responses[good] = respond(200, "JPEGDATA")
	stub.responses[cdn+"/v/redirect.jpg"] = respond(302, "", "Location", "https://evil.example/steal.jpg")

	fetch := func(raw string) (string, error) {
		var buf bytes.Buffer
		err := h.b.Fetch(t.Context(), ids.FileRef{Network: proto.Instagram, Mime: "image/jpeg", Source: &mediaSource{url: raw}}, &buf)
		return buf.String(), err
	}
	if got, err := fetch(good); err != nil || got != "JPEGDATA" {
		t.Errorf("CDN download = %q, %v", got, err)
	}
	for _, bad := range []string{
		"https://evil.example/x.jpg",
		"http://scontent.cdninstagram.com/x.jpg",
		"https://cdninstagram.com.evil.example/x.jpg",
		"https://user@scontent.cdninstagram.com/x.jpg",
		"file:///etc/passwd",
		cdn + "/v/redirect.jpg",
	} {
		if _, err := fetch(bad); !errors.Is(err, errNotCDN) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	for _, asked := range stub.asked {
		if strings.Contains(asked, "evil") || strings.HasPrefix(asked, "GET http:") {
			t.Errorf("fetched %s", asked)
		}
	}
}

func TestAnExpiredAddressIsLookedUpAgainInItsMessage(t *testing.T) {
	photo := func(sig string) string {
		return fmt.Sprintf(`{"id":"mid.p","sender_fbid":"2002","timestamp_ms":"%d","content":{"__typename":"SlideMessageImageContent",
			"attachments":[{"attachment_fbid":"606","attachment_cdn_url":"%s/v/t1/p_n.jpg?oh=%s"}]}}`, tsBase, cdn, sig)
	}
	h := loaded(t, photo("old"))
	h.api.threads[dmIGID] = threadInfo(t, thread(dmIGID, fmt.Sprint(mayaFBID), dmLong, false, "", []string{mayaJSON}, tsBase,
		[]string{photo("new")}, "", false, ""))
	stub := h.stubCDN()
	stub.responses[cdn+"/v/t1/p_n.jpg?oh=old"] = respond(403, "")
	stub.responses[cdn+"/v/t1/p_n.jpg?oh=new"] = respond(200, "FRESH")

	h.b.mu.Lock()
	id := h.b.chats[mayaFBID].msgs["mid.p"].media[0].FileID
	h.b.mu.Unlock()
	ref, _ := h.deps.Files.Get(id)
	var buf bytes.Buffer
	if err := h.b.Fetch(t.Context(), ref, &buf); err != nil || buf.String() != "FRESH" {
		t.Fatalf("download = %q, %v", buf.String(), err)
	}
	if h.api.count("GetThread") != 1 {
		t.Errorf("requests = %v", h.api.names())
	}
	noCalls(t, h.api, "MarkRead", "MarkReadValidation")
}

func TestTheLogHoldsKindsNotWords(t *testing.T) {
	var log bytes.Buffer
	hlog.SetOutput(&log)
	defer hlog.SetOutput(io.Discard)

	h := newHarness(t)
	h.api.loadErr = fmt.Errorf("%w: redirected to https://www.instagram.com/accounts/login/?next=SECRET-NEXT", httpclient.ErrTokenInvalidated)
	set, _ := cookies.Parse(proto.Instagram, cookieText)
	if err := h.b.LoginCookies(t.Context(), set, browser.Default()); err == nil {
		t.Fatal("login worked")
	}
	h.api.loadErr = nil
	h.login(mailbox(t, "", false, dmThread()))
	_ = h.api.emit(delta(t, "SlideUQPPNewMessage", "unknown-thread", `"message":`+textMsg("mid.s", mayaFBID, tsBase, "my secret message")))
	_ = h.api.emit(&slidetypes.Disconnected{Error: fmt.Errorf("dial wss://edge-chat.instagram.com/?token=SECRET-WS: refused"), FailureCount: 2})
	_ = h.api.emit(delta(t, "SlideUQPPNewMessage", dmIGID, `"message":`+textMsg("mid.t", mayaFBID, tsBase, "another secret message")))

	text := log.String()
	if !strings.Contains(text, "instagram") {
		t.Fatalf("nothing was logged: %q", text)
	}
	for _, secret := range []string{"SECRET", "secret message", "s3ss10n", "csrf", "Maya", "maya.lens", dmIGID} {
		if strings.Contains(text, secret) {
			t.Errorf("the log holds %q:\n%s", secret, text)
		}
	}
}
