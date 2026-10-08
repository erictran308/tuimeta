// SPDX-License-Identifier: AGPL-3.0-or-later

package browser

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
	"go.mau.fi/mautrix-meta/pkg/messagix/useragent"
	"go.mau.fi/util/exhttp"
)

// on makes Parse see the given system for one test.
func on(t *testing.T, system, version string) {
	t.Helper()
	oldOS, oldVersion := goos, osVersion
	goos, osVersion = system, func() string { return version }
	t.Cleanup(func() { goos, osVersion = oldOS, oldVersion })
}

func mustParse(t *testing.T, name string) Identity {
	t.Helper()
	id, err := Parse(name)
	if err != nil {
		t.Fatalf("Parse(%q): %v", name, err)
	}
	return id
}

func TestNamingNothingKeepsTheLibrariesOwnBrowser(t *testing.T) {
	for _, name := range []string{"", "  "} {
		id := mustParse(t, name)
		if !id.IsDefault() || id.Name() != "" || id.UserAgent != useragent.UserAgent {
			t.Errorf("%q: %+v", name, id)
		}
	}
}

func TestChromeOnAMacIsDescribedAsChromeDescribesItself(t *testing.T) {
	on(t, "darwin", "26.0")
	id := mustParse(t, "Chrome 150.0.7712.45")
	if id.Name() != "Chrome 150.0.7712.45" || id.IsDefault() {
		t.Errorf("name %q", id.Name())
	}
	want := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
	if id.UserAgent != want {
		t.Errorf("user agent %q", id.UserAgent)
	}
	if id.Platform != "macOS" || id.PlatformVersion != "26.0.0" {
		t.Errorf("platform %q %q", id.Platform, id.PlatformVersion)
	}
}

func TestWindowsAndLinuxGetTheirOwnSystemToken(t *testing.T) {
	on(t, "windows", "")
	if id := mustParse(t, "Chrome 150.0.7712.45"); id.Platform != "Windows" ||
		id.UserAgent != "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36" ||
		id.PlatformVersion != "" {
		t.Errorf("windows: %+v", id)
	}
	on(t, "linux", "6.8.0-45-generic\n")
	if id := mustParse(t, "Chrome 150.0.7712.45"); id.Platform != "Linux" ||
		id.UserAgent != "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36" ||
		id.PlatformVersion != "6.8.0" {
		t.Errorf("linux: %+v", id)
	}
}

func TestTheBrandListsAreTheOnesChromeSent(t *testing.T) {
	// What these versions of Chrome really sent in sec-ch-ua.
	for major, want := range map[int]string{
		120: `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`,
		124: `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		131: `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		141: `"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"`,
	} {
		if got := brands(major, strconv.Itoa(major), false); got != want {
			t.Errorf("%d: %s", major, got)
		}
	}
	on(t, "darwin", "26.0.1")
	id := mustParse(t, "Chrome 141.0.7390.122")
	if id.FullVersionList != `"Google Chrome";v="141.0.7390.122", "Not?A_Brand";v="8.0.0.0", "Chromium";v="141.0.7390.122"` {
		t.Errorf("full version list %s", id.FullVersionList)
	}
}

func TestChromesOwnWayOfNamingItIsAccepted(t *testing.T) {
	for _, name := range []string{
		"chrome 150.0.7712.45",
		"Google Chrome 150.0.7712.45",
		"Google Chrome\t150.0.7712.45 (Official Build) (arm64)",
		"  Chrome 150.0.7712.45  ",
	} {
		if id := mustParse(t, name); id.Name() != "Chrome 150.0.7712.45" {
			t.Errorf("%q named %q", name, id.Name())
		}
	}
}

func TestAnythingButChromeWithAFullVersionIsRefused(t *testing.T) {
	for _, name := range []string{
		"Chrome",
		"Chrome 150",
		"Chrome 150.0.0",
		"Safari 18.6",
		"Firefox 140.0.1.2",
		"Microsoft Edge 150.0.3456.78",
		"Chrome 99.0.4844.51",
		"Chrome 1500.0.1.2",
		"Chrome 150.0.7712.45; DROP",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
		"Chrome 150.0.7712.45\r\nX-Evil: 1",
		"Chrome\n150.0.7712.45",
		"Chrome 150.0.7712.45 (Official\r\nX-Evil: 1)",
	} {
		if _, err := Parse(name); !errors.Is(err, ErrNotChrome) {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestSystemVersionsBecomeThreeNumbers(t *testing.T) {
	for in, want := range map[string]string{
		"26.0":               "26.0.0",
		"15.6.1":             "15.6.1",
		"6.8.0-45-generic\n": "6.8.0",
		"6.8\n":              "6.8.0",
		"10.0.19045.1":       "10.0.19045",
		"":                   "",
		"unknown":            "",
		"07.01":              "7.1.0",
	} {
		if got := triple(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func libraryHeaders() http.Header {
	h := http.Header{}
	h.Set("user-agent", useragent.UserAgent)
	h.Set("sec-ch-ua", useragent.SecCHUserAgent)
	h.Set("sec-ch-ua-platform", useragent.SecCHPlatform)
	h.Set("sec-ch-ua-full-version-list", useragent.SecCHFullVersionList)
	h.Set("sec-ch-ua-platform-version", useragent.SecCHPlatformVersion)
	h.Set("sec-ch-ua-mobile", useragent.SecCHMobile)
	return h
}

func TestTheLibrariesHeadersAreReplacedWithTheNamedBrowsers(t *testing.T) {
	on(t, "darwin", "26.0.1")
	id := mustParse(t, "Chrome 150.0.7712.45")
	h := libraryHeaders()
	id.Rewrite(h)
	if h.Get("User-Agent") != id.UserAgent || h.Get("Sec-Ch-Ua") != id.Brands ||
		h.Get("Sec-Ch-Ua-Full-Version-List") != id.FullVersionList ||
		h.Get("Sec-Ch-Ua-Platform") != `"macOS"` || h.Get("Sec-Ch-Ua-Platform-Version") != `"26.0.1"` {
		t.Errorf("headers %v", h)
	}
	if h.Get("Sec-Ch-Ua-Mobile") != "?0" {
		t.Errorf("an unrelated hint changed: %v", h)
	}
}

func TestOnlyTheHintsARequestHadAreReplaced(t *testing.T) {
	on(t, "darwin", "26.0.1")
	id := mustParse(t, "Chrome 150.0.7712.45")
	h := http.Header{}
	h.Set("User-Agent", useragent.UserAgent)
	id.Rewrite(h)
	if len(h) != 1 || h.Get("User-Agent") != id.UserAgent {
		t.Errorf("headers %v", h)
	}
}

func TestRequestsPretendingToBeAPhoneAppAreLeftAlone(t *testing.T) {
	id := mustParse(t, "Chrome 150.0.7712.45")
	h := http.Header{}
	h.Set("User-Agent", useragent.MessengerLiteIOSUserAgent)
	h.Set("Sec-Ch-Ua", "x")
	id.Rewrite(h)
	if h.Get("User-Agent") != useragent.MessengerLiteIOSUserAgent || h.Get("Sec-Ch-Ua") != "x" {
		t.Errorf("headers %v", h)
	}
}

func TestTheDefaultChangesNothing(t *testing.T) {
	h := libraryHeaders()
	Default().Rewrite(h)
	if h.Get("User-Agent") != useragent.UserAgent || h.Get("Sec-Ch-Ua") != useragent.SecCHUserAgent {
		t.Errorf("headers %v", h)
	}
	rt := http.DefaultTransport
	if Default().Wrap(rt) != rt {
		t.Error("the default wrapped a transport")
	}
}

// seen records the headers each request arrived with.
func seen(t *testing.T) (*httptest.Server, chan http.Header) {
	t.Helper()
	got := make(chan http.Header, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		if r.Header.Get("Upgrade") == "websocket" {
			c, err := websocket.Accept(w, r, nil)
			if err == nil {
				c.Close(websocket.StatusNormalClosure, "")
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestAWrappedClientSendsTheNamedBrowserWithoutChangingTheRequest(t *testing.T) {
	on(t, "darwin", "26.0.1")
	id := mustParse(t, "Chrome 150.0.7712.45")
	srv, got := seen(t)
	client := &http.Client{Transport: id.Wrap(http.DefaultTransport)}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header = libraryHeaders()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := <-got; h.Get("User-Agent") != id.UserAgent || h.Get("Sec-Ch-Ua-Platform") != `"macOS"` {
		t.Errorf("arrived with %v", h)
	}
	if req.Header.Get("User-Agent") != useragent.UserAgent {
		t.Error("the caller's request was changed")
	}
}

// stubClient is what a library's HTTP client is made for, enough to make one.
type stubClient struct{}

func (stubClient) GetPlatform() types.Platform  { return types.Facebook }
func (stubClient) GetLogger() *zerolog.Logger   { l := zerolog.Nop(); return &l }
func (stubClient) GetCookies() *cookies.Cookies { return &cookies.Cookies{Platform: types.Facebook} }
func (stubClient) GetEndpoint(string) string    { return "" }
func (stubClient) IsAuthenticated() bool        { return false }

func TestALibraryClientAndItsWebsocketsSendTheNamedBrowser(t *testing.T) {
	on(t, "darwin", "26.0.1")
	id := mustParse(t, "Chrome 150.0.7712.45")
	srv, got := seen(t)
	c := httpclient.NewHTTPClient(stubClient{}, nil, exhttp.SensibleClientSettings)
	// A socket copies the dial options when it's made, which may be before
	// Use runs; the client inside them is the same one.
	opts := *c.GetWebsocketDialer()
	id.Use(c)

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("User-Agent", useragent.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := <-got; h.Get("User-Agent") != id.UserAgent {
		t.Errorf("http arrived with %v", h.Get("User-Agent"))
	}

	opts.HTTPHeader = http.Header{}
	opts.HTTPHeader.Set("User-Agent", useragent.UserAgent)
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), &opts)
	if err != nil {
		t.Fatal(err)
	}
	conn.CloseNow()
	if h := <-got; h.Get("User-Agent") != id.UserAgent {
		t.Errorf("websocket arrived with %v", h.Get("User-Agent"))
	}
}
