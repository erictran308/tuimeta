// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/useragent"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waWa6"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const chrome = "Chrome 150.0.7712.45"

func namedChrome(t *testing.T) browser.Identity {
	t.Helper()
	as, err := browser.Parse(chrome)
	if err != nil {
		t.Fatal(err)
	}
	return as
}

func TestALoginHandsTheNamedBrowserToItsConnection(t *testing.T) {
	h := newHarness(t)
	var got browser.Identity
	h.m.dial = func(_ context.Context, _ context.Context, l login) error {
		got = l.as
		return errBadCookies
	}
	set, err := cookies.Parse(proto.Messenger, "c_user=1; xs=2; datr=3")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.m.LoginCookies(context.Background(), set, namedChrome(t))
	if got.Name() != chrome {
		t.Errorf("connected as %q", got.Name())
	}
}

func TestASavedSessionResumesAsTheBrowserItLoggedInAs(t *testing.T) {
	h := newHarness(t)
	if err := h.deps.Session.SaveJSON(sessionFile, savedSession{Version: 1, UserID: selfID, Cookies: map[string]string{"c_user": "100001", "xs": "x"}, Browser: chrome}); err != nil {
		t.Fatal(err)
	}
	resumed := make(chan browser.Identity, 1)
	h.m.dial = func(_ context.Context, _ context.Context, l login) error {
		select {
		case resumed <- l.as:
		default:
		}
		return errCheckpoint
	}
	h.m.Start(context.Background())
	select {
	case as := <-resumed:
		if as.Name() != chrome || as.UserAgent != namedChrome(t).UserAgent {
			t.Errorf("resumed as %q", as.Name())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session wasn't resumed")
	}
}

func TestASavedSessionNamingAnUnknownBrowserIsntResumed(t *testing.T) {
	h := newHarness(t)
	if err := h.deps.Session.SaveJSON(sessionFile, savedSession{Version: 1, UserID: selfID, Cookies: map[string]string{"c_user": "100001", "xs": "x"}, Browser: "Netscape 4.0"}); err != nil {
		t.Fatal(err)
	}
	h.m.Start(context.Background())
	acc := h.rec.of("account")
	if len(acc) != 1 || acc[0].(proto.AccountEvent).State != proto.LoggedOut {
		t.Errorf("account %+v", acc)
	}
}

// arrivesAs is the user agent and platform a request from a Messenger client
// made for as reaches a server with.
func arrivesAs(t *testing.T, as browser.Identity) (agent, platform string) {
	t.Helper()
	got := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
	}))
	t.Cleanup(srv.Close)
	cli := newMessagix(map[string]string{"c_user": "1", "xs": "2", "datr": "3"}, as)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header = cli.GetHTTP().BuildHeaders(false, false)
	resp, err := cli.GetHTTP().HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := <-got
	return h.Get("User-Agent"), h.Get("Sec-Ch-Ua-Platform")
}

func TestWithNoBrowserNamedTheClientSaysWhatTheLibrariesSay(t *testing.T) {
	as, err := browser.Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if agent, platform := arrivesAs(t, as); agent != useragent.UserAgent || platform != useragent.SecCHPlatform {
		t.Errorf("arrived as %q on %s", agent, platform)
	}
}

func TestTheMessengerClientSaysItIsTheNamedBrowser(t *testing.T) {
	as := namedChrome(t)
	if agent, platform := arrivesAs(t, as); agent != as.UserAgent || platform != `"`+as.Platform+`"` {
		t.Errorf("arrived as %q on %s", agent, platform)
	}
	cli := newMessagix(map[string]string{"c_user": "1", "xs": "2", "datr": "3"}, as)
	// messagix rebuilds its HTTP clients, unwrapped, only when it's given a
	// way to get a new proxy: with one, a session would quietly go back to
	// the libraries' browser after a network error.
	if cli.GetHTTP().GetNewProxy != nil {
		t.Error("messagix can replace its clients")
	}
}

func payloadLikeMessagix() *waWa6.ClientPayload {
	return &waWa6.ClientPayload{
		FbUserAgent: []byte(useragent.UserAgent),
		UserAgent: &waWa6.ClientPayload_UserAgent{
			Device:       gproto.String(useragent.BrowserName),
			Manufacturer: gproto.String(useragent.OSName),
		},
	}
}

func TestTheEncryptedChatsHandshakeNamesTheSameBrowser(t *testing.T) {
	as := namedChrome(t)
	cli := &whatsmeow.Client{MessengerConfig: &whatsmeow.MessengerConfig{UserAgent: useragent.UserAgent}}
	cli.GetClientPayload = payloadLikeMessagix
	presentE2EE(cli, as)
	p := cli.GetClientPayload()
	if string(p.FbUserAgent) != as.UserAgent || p.UserAgent.GetManufacturer() != as.Platform || p.UserAgent.GetDevice() != "Chrome" {
		t.Errorf("payload %v", p)
	}
	if cli.MessengerConfig.UserAgent != as.UserAgent {
		t.Errorf("requests say %q", cli.MessengerConfig.UserAgent)
	}

	plain := &whatsmeow.Client{MessengerConfig: &whatsmeow.MessengerConfig{UserAgent: useragent.UserAgent}}
	plain.GetClientPayload = payloadLikeMessagix
	presentE2EE(plain, browser.Default())
	if string(plain.GetClientPayload().FbUserAgent) != useragent.UserAgent || plain.MessengerConfig.UserAgent != useragent.UserAgent {
		t.Error("the libraries' own browser was changed")
	}
}

func TestDownloadsSayTheyreTheSessionsBrowser(t *testing.T) {
	as := namedChrome(t)
	agents := make(chan http.Header, 1)
	fakeCDN(t, func(w http.ResponseWriter, r *http.Request) {
		agents <- r.Header.Clone()
		w.Write([]byte("photo bytes"))
	})
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.as = as
	h.m.mu.Unlock()
	ref := ids.FileRef{ID: 1, Network: proto.Messenger, Source: &fbSource{URL: "https://scontent.xx.fbcdn.net/p.jpg", Mime: "image/jpeg"}}
	var buf writeBuf
	if err := h.m.Fetch(context.Background(), ref, &buf); err != nil {
		t.Fatal(err)
	}
	got := <-agents
	if got.Get("User-Agent") != as.UserAgent || got.Get("Sec-Ch-Ua") != as.Brands || got.Get("Sec-Ch-Ua-Platform") != `"`+as.Platform+`"` {
		t.Errorf("downloaded as %v", got)
	}
}
