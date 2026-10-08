// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestALoginSavesTheBrowserItNamedAndResumesAsIt(t *testing.T) {
	h := newHarness(t)
	h.api.inbox = mailbox(t, "", false)
	set, err := cookies.Parse(proto.Instagram, cookieText)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.b.LoginCookies(t.Context(), set, namedChrome(t)); err != nil {
		t.Fatalf("login: %v", err)
	}
	if h.api.as.Name() != chrome {
		t.Errorf("connected as %q", h.api.as.Name())
	}
	data, _ := os.ReadFile(filepath.Join(h.dir, "instagram", sessionFile))
	if !strings.Contains(string(data), `"browser":"`+chrome+`"`) {
		t.Errorf("saved %s", data)
	}
	dir := h.dir
	h.b.Close()

	h2 := newHarnessIn(t, dir)
	h2.api.inbox = mailbox(t, "", false)
	h2.b.Start(t.Context())
	waitFor(t, "ready", func() bool { return len(h2.rec.events("account")) >= 2 })
	if h2.api.as.Name() != chrome {
		t.Errorf("resumed as %q", h2.api.as.Name())
	}
}

func TestTheInstagramClientCantReplaceItsWrappedClients(t *testing.T) {
	// instameow rebuilds its HTTP clients, unwrapped, only when it's given a
	// way to get a new proxy: with one, a session would quietly go back to
	// the libraries' browser after a network error.
	cli := dialReal(libCookies(map[string]string{"sessionid": "s", "ds_user_id": "1", "csrftoken": "c"}), namedChrome(t), nil)
	if cli.(realAPI).GetHTTP().GetNewProxy != nil {
		t.Error("instameow can replace its clients")
	}
}

func TestASavedSessionNamingAnUnknownBrowserIsntResumed(t *testing.T) {
	h := newHarness(t)
	if err := h.deps.Session.SaveJSON(sessionFile, savedSession{Version: 1, Cookies: map[string]string{"sessionid": "s", "ds_user_id": "1", "csrftoken": "c"}, Browser: "Netscape 4.0"}); err != nil {
		t.Fatal(err)
	}
	h.b.Start(t.Context())
	if got := accountStates(h.rec); strings.Join(got, ",") != "logged_out" {
		t.Errorf("account states = %v", got)
	}
	if len(h.api.names()) != 0 {
		t.Errorf("requests: %v", h.api.names())
	}
}

func TestDownloadsSayTheyreTheSessionsBrowserUntilLogout(t *testing.T) {
	h := newHarness(t)
	h.api.inbox = mailbox(t, "", false)
	set, _ := cookies.Parse(proto.Instagram, cookieText)
	as := namedChrome(t)
	if err := h.b.LoginCookies(t.Context(), set, as); err != nil {
		t.Fatal(err)
	}
	stub := h.stubCDN()
	good := cdn + "/v/t51/photo_n.jpg?oh=sig&oe=1"
	var got http.Header
	stub.responses[good] = func(req *http.Request) *http.Response {
		got = req.Header.Clone()
		return respond(200, "JPEGDATA")(req)
	}
	var buf bytes.Buffer
	if err := h.b.Fetch(t.Context(), ids.FileRef{Network: proto.Instagram, Mime: "image/jpeg", Source: &mediaSource{url: good}}, &buf); err != nil {
		t.Fatal(err)
	}
	if got.Get("User-Agent") != as.UserAgent || got.Get("Sec-Ch-Ua") != as.Brands {
		t.Errorf("downloaded as %v", got)
	}
	if err := h.b.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	if !h.b.as.IsDefault() {
		t.Errorf("still %q after logout", h.b.as.Name())
	}
}
