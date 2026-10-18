// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// What a page of threads.com says about the session, read as the website's
// own code reads it: the tokens every GraphQL request carries, who is
// logged in, the gates and experiments the website's feature flags come
// from, and the JavaScript it loads (where its queries are defined, read
// only when Threads stops taking a query as it was: threads_catalog.go).

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/erictran308/tuimeta/helper/internal/backend"
)

// threadsPage is what one load of a page of threads.com gave.
type threadsPage struct {
	// What each GraphQL request carries (threadsForm).
	dtsg, lsd, appID   string
	rev, spinR, spinT  string
	spinB, hsi, haste  string
	cometReq, ccg, crn string
	dpr                string
	// actor is the account's id as the website's requests name it (av);
	// viewerID its Instagram user id; username its name on Threads.
	actor, viewerID, username string
	// gkx, qex and config are the gates, experiments and settings the page
	// shipped, which the queries' feature flags read (threadsProviders).
	gkx    map[string]bool
	qex    map[string]json.RawMessage
	config map[string]json.RawMessage
	// bundles are the page's JavaScript, in the order the page names it.
	bundles []string
}

var (
	pageDTSG     = regexp.MustCompile(`"DTSGInitialData",\[\],\{"token":"([^"]+)"`)
	pageLSD      = regexp.MustCompile(`"LSD",\[\],\{"token":"([^"]+)"`)
	pageAppID    = regexp.MustCompile(`"CurrentUserInitialData",\[\],\{[^}]*?"APP_ID":"(\d+)"`)
	pageNonFB    = regexp.MustCompile(`"NON_FACEBOOK_USER_ID":"(\d+)"`)
	pageActor    = regexp.MustCompile(`"RelayAPIConfigDefaults",\[\],\{[^}]*?"actorID":"(\d+)"`)
	pageRev      = regexp.MustCompile(`"server_revision":(\d+)`)
	pageSpinR    = regexp.MustCompile(`"__spin_r":(\d+)`)
	pageSpinT    = regexp.MustCompile(`"__spin_t":(\d+)`)
	pageSpinB    = regexp.MustCompile(`"__spin_b":"([A-Za-z0-9_]+)"`)
	pageHSI      = regexp.MustCompile(`"hsi":"(\d+)"`)
	pageHaste    = regexp.MustCompile(`"haste_session":"([A-Za-z0-9._:-]+)"`)
	pageComet    = regexp.MustCompile(`"comet_env":(\d+)`)
	pageCCG      = regexp.MustCompile(`"connectionClass":"([A-Z]+)"`)
	pageCRN      = regexp.MustCompile(`"canonicalRouteName":"([A-Za-z0-9._]+)"`)
	pageDPR      = regexp.MustCompile(`"SiteData",\[\],\{[^}]*?"pr":([0-9.]+)`)
	pageViewer   = regexp.MustCompile(`"viewer":\{"fbid":`)
	pageGate     = regexp.MustCompile(`"(\d+)":\{"result":(true|false),"hash":`)
	pageExp      = regexp.MustCompile(`"(\d+)":\{"r":(true|false|null|-?\d+(?:\.\d+)?|"[^"\\]{0,200}")`)
	pageConfig   = regexp.MustCompile(`"(\d+)":\{"value":(true|false|null|-?\d+(?:\.\d+)?|"[^"\\]{0,200}")\}`)
	pageBundle   = regexp.MustCompile(`https:(?:\\/\\/|//)static\.cdninstagram\.com(?:\\/|/)rsrc\.php(?:\\/|/)[A-Za-z0-9_\\/,.+-]{1,300}?\.js`)
	threadsDigit = regexp.MustCompile(`^\d{1,25}$`)
)

// maxBundles is the most JavaScript files a page's list keeps: the home
// page names about 420.
const maxBundles = 2000

// parseThreadsPage reads a page. ok is false when nobody is logged in on
// it (the page of a session that's over, or of no session): its viewer is
// "0", or it has none.
func parseThreadsPage(html string) (p threadsPage, ok bool) {
	first := func(re *regexp.Regexp) string {
		if m := re.FindStringSubmatch(html); m != nil {
			return m[1]
		}
		return ""
	}
	p = threadsPage{
		dtsg: first(pageDTSG), lsd: first(pageLSD), appID: first(pageAppID),
		rev: first(pageRev), spinR: first(pageSpinR), spinT: first(pageSpinT), spinB: first(pageSpinB),
		hsi: first(pageHSI), haste: first(pageHaste), cometReq: first(pageComet), ccg: first(pageCCG),
		crn: first(pageCRN), dpr: first(pageDPR),
		actor: first(pageActor),
		gkx:   map[string]bool{}, qex: map[string]json.RawMessage{}, config: map[string]json.RawMessage{},
	}
	if p.actor == "" || p.actor == "0" {
		p.actor = first(pageNonFB)
	}
	if p.dpr == "" {
		p.dpr = "2"
	}
	if loc := pageViewer.FindStringIndex(html); loc != nil {
		var v struct {
			ID       flexString `json:"id"`
			Username string     `json:"username"`
		}
		dec := json.NewDecoder(strings.NewReader(html[loc[0]+len(`"viewer":`):]))
		if dec.Decode(&v) == nil {
			p.viewerID, p.username = string(v.ID), v.Username
		}
	}
	for _, m := range pageGate.FindAllStringSubmatch(html, -1) {
		if _, seen := p.gkx[m[1]]; !seen {
			p.gkx[m[1]] = m[2] == "true"
		}
	}
	for _, m := range pageExp.FindAllStringSubmatch(html, -1) {
		if _, seen := p.qex[m[1]]; !seen {
			p.qex[m[1]] = json.RawMessage(m[2])
		}
	}
	for _, m := range pageConfig.FindAllStringSubmatch(html, -1) {
		if _, seen := p.config[m[1]]; !seen {
			p.config[m[1]] = json.RawMessage(m[2])
		}
	}
	seen := map[string]bool{}
	for _, raw := range pageBundle.FindAllString(html, -1) {
		u := strings.ReplaceAll(raw, `\/`, "/")
		if !seen[u] && len(p.bundles) < maxBundles {
			seen[u] = true
			p.bundles = append(p.bundles, u)
		}
	}
	if !backend.Username(p.username) {
		p.username = ""
	}
	if !threadsDigit.MatchString(p.viewerID) {
		p.viewerID = ""
	}
	ok = p.actor != "" && p.actor != "0" && p.viewerID != "" && p.dtsg != "" && p.lsd != ""
	return p, ok
}

// usable reports whether the page has what a GraphQL request needs.
func (p *threadsPage) usable() bool {
	return p != nil && p.dtsg != "" && p.lsd != "" && p.actor != ""
}

// jazoest is the number the website derives from fb_dtsg: "2" and the sum
// of its characters' codes.
func jazoest(dtsg string) string {
	sum := 0
	for _, r := range dtsg {
		sum += int(r)
	}
	return "2" + strconv.Itoa(sum)
}
