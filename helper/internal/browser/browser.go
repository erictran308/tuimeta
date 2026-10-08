// SPDX-License-Identifier: AGPL-3.0-or-later

// Package browser is the web browser the helper says it is to Facebook and
// Instagram. Left alone, it's the libraries' own: Chrome 141 on Linux, what
// every mautrix-meta client says. Named at login ("Chrome 150.0.7712.45"),
// it's that Chrome on this computer's system, so a session whose cookies
// came from that browser keeps looking like it rather than like a second
// device using them. Only Chrome can be named: the libraries' connections
// shake hands the way Chrome does, and another browser's name would
// contradict them.
package browser

import (
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/useragent"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Identity is a browser as requests describe it: the user agent and the
// client hints that go with it.
type Identity struct {
	// name is what the login named ("Chrome 150.0.7712.45"), "" for the
	// libraries' own.
	name string

	UserAgent string
	// Brands is sec-ch-ua, and FullVersionList sec-ch-ua-full-version-list.
	Brands          string
	FullVersionList string
	// Platform and PlatformVersion are sec-ch-ua-platform's and
	// sec-ch-ua-platform-version's values, unquoted.
	Platform        string
	PlatformVersion string
}

// Default is the libraries' own identity.
func Default() Identity {
	return Identity{
		UserAgent:       useragent.UserAgent,
		Brands:          useragent.SecCHUserAgent,
		FullVersionList: useragent.SecCHFullVersionList,
		Platform:        useragent.OSName,
		PlatformVersion: useragent.OSVersion,
	}
}

// ErrNotChrome is the answer to a login naming anything but Chrome and its
// full version.
var ErrNotChrome = proto.Err(proto.BadRequest,
	`The browser to log in as must be Chrome and its full version, like "Chrome 150.0.7712.45" (chrome://version shows it).`)

// named is "Chrome" and a full version, as chrome://version or the About
// page shows them (with or without "Google", and the "(Official Build)"
// notes after it), all on one line. Only the numbers are used: the name the
// session keeps and every header are built from them.
var named = regexp.MustCompile(`(?i)^(?:google[ \t]+)?chrome[ \t]+(\d{1,4})\.(\d{1,5})\.(\d{1,6})\.(\d{1,6})(?:[ \t]+\([^()\r\n]*\))*$`)

// goos and osVersion are this computer's; tests replace them.
var (
	goos      = runtime.GOOS
	osVersion = systemVersion
)

// Parse is the identity a login names: "" is the libraries' own, anything
// else must be Chrome and its full version.
func Parse(name string) (Identity, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Default(), nil
	}
	m := named.FindStringSubmatch(name)
	if m == nil {
		return Identity{}, ErrNotChrome
	}
	var parts [4]int
	for i := range parts {
		parts[i], _ = strconv.Atoi(m[i+1])
	}
	// Chrome 100 came out in 2022; a smaller major version is a typo, and so
	// is one with four digits.
	major := parts[0]
	if major < 100 || major > 999 {
		return Identity{}, ErrNotChrome
	}
	full := fmt.Sprintf("%d.%d.%d.%d", parts[0], parts[1], parts[2], parts[3])
	token, platform := system(goos)
	return Identity{
		name:            "Chrome " + full,
		UserAgent:       "Mozilla/5.0 (" + token + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + strconv.Itoa(major) + ".0.0.0 Safari/537.36",
		Brands:          brands(major, strconv.Itoa(major), false),
		FullVersionList: brands(major, full, true),
		Platform:        platform,
		PlatformVersion: triple(osVersion()),
	}, nil
}

// Name is what the login named, to be saved with the session and named
// again when it's resumed; "" for the libraries' own.
func (id Identity) Name() string { return id.name }

// IsDefault says the libraries' own identity is used as it is.
func (id Identity) IsDefault() bool { return id.name == "" }

// system is how Chrome names this computer's system: the token in its user
// agent, the same for every version of each system since Chrome stopped
// putting details there (Apple silicon Macs say Intel too), and its
// sec-ch-ua-platform.
func system(goos string) (token, platform string) {
	switch goos {
	case "darwin":
		return "Macintosh; Intel Mac OS X 10_15_7", "macOS"
	case "windows":
		return "Windows NT 10.0; Win64; x64", "Windows"
	}
	return "X11; Linux x86_64", "Linux"
}

// brands is Chrome's brand list for a major version: Chromium, Google Chrome
// and a made-up brand that keeps servers from relying on the list's shape.
// Chrome derives the made-up brand's name, version and the list's order from
// its major version (Chromium's GenerateBrandVersionList), so this is the
// list that Chrome sends. With full, each brand has a full version
// (sec-ch-ua-full-version-list), else only the major one (sec-ch-ua).
func brands(major int, version string, full bool) string {
	chars := [...]string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	versions := [...]string{"8", "99", "24"}
	orders := [...][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	madeUp := "Not" + chars[major%len(chars)] + "A" + chars[(major+1)%len(chars)] + "Brand"
	madeUpVersion := versions[major%len(versions)]
	if full {
		madeUpVersion += ".0.0.0"
	}
	order := orders[major%len(orders)]
	var list [3]string
	list[order[0]] = brand(madeUp, madeUpVersion)
	list[order[1]] = brand("Chromium", version)
	list[order[2]] = brand("Google Chrome", version)
	return strings.Join(list[:], ", ")
}

func brand(name, version string) string { return `"` + name + `";v="` + version + `"` }

// triple is a system version as Chrome reports it: three numbers ("26.0" is
// "26.0.0", a Linux kernel's "6.8.0-45-generic" is "6.8.0"), or "" if there's
// no number to report.
func triple(v string) string {
	var nums []string
	for part := range strings.SplitSeq(v, ".") {
		end := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' })
		if part == "" || end == 0 {
			break
		}
		if end > 0 {
			nums = append(nums, part[:end])
			break
		}
		nums = append(nums, part)
		if len(nums) == 3 {
			break
		}
	}
	if len(nums) == 0 {
		return ""
	}
	for len(nums) < 3 {
		nums = append(nums, "0")
	}
	for i, n := range nums[:3] {
		k, _ := strconv.Atoi(n)
		nums[i] = strconv.Itoa(k)
	}
	return strings.Join(nums[:3], ".")
}

// Rewrite puts this identity in place of the libraries' own wherever h has
// it. A request that says it's something else (the phone apps some calls
// pretend to be) is left as it is, and so is a client hint h doesn't have.
func (id Identity) Rewrite(h http.Header) {
	if id.IsDefault() || h.Get("User-Agent") != useragent.UserAgent {
		return
	}
	h.Set("User-Agent", id.UserAgent)
	replace(h, "Sec-Ch-Ua", id.Brands)
	replace(h, "Sec-Ch-Ua-Full-Version-List", id.FullVersionList)
	replace(h, "Sec-Ch-Ua-Platform", `"`+id.Platform+`"`)
	replace(h, "Sec-Ch-Ua-Platform-Version", `"`+id.PlatformVersion+`"`)
}

func replace(h http.Header, key, value string) {
	if _, ok := h[key]; ok {
		h.Set(key, value)
	}
}

// Wrap is rt with every request rewritten.
func (id Identity) Wrap(rt http.RoundTripper) http.RoundTripper {
	if id.IsDefault() {
		return rt
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &transport{id: id, next: rt}
}

type transport struct {
	id   Identity
	next http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") != useragent.UserAgent {
		return t.next.RoundTrip(req)
	}
	// A RoundTripper mustn't change the request it's given.
	req = req.Clone(req.Context())
	t.id.Rewrite(req.Header)
	return t.next.RoundTrip(req)
}

// Use makes a library's HTTP client, and the one its websockets dial with,
// present this identity. It's called on a new client, before its first
// request.
func (id Identity) Use(c *httpclient.HTTPClient) {
	if id.IsDefault() || c == nil {
		return
	}
	if c.HTTP != nil {
		c.HTTP.Transport = id.Wrap(c.HTTP.Transport)
	}
	// The dial options hold the websockets' own client, which every socket
	// the library opens shares.
	if d := c.GetWebsocketDialer(); d != nil && d.HTTPClient != nil {
		d.HTTPClient.Transport = id.Wrap(d.HTTPClient.Transport)
	}
}
