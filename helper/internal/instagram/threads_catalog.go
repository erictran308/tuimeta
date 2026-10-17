// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// How Threads' website asks for what the helper reads: each GraphQL query
// is a persisted document (its doc id), with the variables it declares,
// among them the website's feature flags, whose values the website's code
// works out from the gates and experiments its page shipped. What the
// website did on 2026-10-17 is built in. Threads changes its queries from
// time to time; when it stops taking one as sent (a variable it now needs,
// a doc id it no longer knows), the definitions are read again from the
// website's own JavaScript, as a browser loading the page would get it, and
// kept in <data-dir>/threads/catalog.json.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
)

// The queries the helper sends. Replies of a reply are the reply's own
// first and later replies, asked for with these two as for a post.
const (
	threadsFeedQuery    = "BarcelonaFeedPaginationDirectQuery"
	threadsRepliesQuery = "BarcelonaPostPageDownwardQuery"
	threadsMoreQuery    = "BarcelonaPostPageDirectRepliesRefetchQuery"
)

// A feature flag's variable is the provider's name between these.
const (
	flagPrefix = "__relay_internal__pv__"
	flagSuffix = "relayprovider"
)

// catalogFile is where a rediscovered catalog is kept.
const catalogFile = "catalog.json"

// maxEmpty is the most bundles remembered as holding nothing wanted.
const maxEmpty = 5000

// threadsQuery is one persisted query of the website.
type threadsQuery struct {
	Name string `json:"name"`
	Doc  string `json:"doc"`
	// Kind is what the website's code says it is: only a "query" is ever
	// sent (threadsQueryOf).
	Kind string `json:"kind"`
	// Vars are the variables it declares, but for its feature flags, which
	// are Flags, by provider.
	Vars  []string `json:"vars"`
	Flags []string `json:"flags"`
}

// threadsCatalog is what the helper knows of the website's queries.
type threadsCatalog struct {
	Version int                     `json:"version"`
	Queries map[string]threadsQuery `json:"queries"`
	// Providers are the feature flags' code, by provider: the body of its
	// get(), which threadsFlag reads.
	Providers map[string]string `json:"providers"`
	// Empty are the bundles a rediscovery read that held none of it, by a
	// hash of their address (which names one content), so a later one
	// skips them.
	Empty []string `json:"empty,omitempty"`
}

func words(s string) []string { return strings.Fields(s) }

// threadsFlags are the feature flags the three queries declared on
// 2026-10-17; the reply queries add one more.
var threadsFlags = words(`BarcelonaIsLoggedIn BarcelonaShouldFetchPostAuthorFullName BarcelonaHasDearAlgoConsumption
	BarcelonaHasMetaAiContentAttachments BarcelonaHasMWRLocationConsumption
	BarcelonaMessagesHasLiveChatMessaging BarcelonaHasEventBadge BarcelonaHasBestOfThreads
	BarcelonaMessagingHasMetaAIBot BarcelonaGenAIRepliesEnabled BarcelonaIsSearchDiscoveryEnabled
	BarcelonaHasCommunities BarcelonaHasGameScoreShare BarcelonaHasPublicViewCountCard
	BarcelonaHasCommunityEmojiUpdateCard BarcelonaHasCommunityEntityCard BarcelonaHasScorecardCommunity
	BarcelonaHasSportTeamAllegianceCard BarcelonaHasStitchedAttachment BarcelonaHasNewspaperLinkStyle
	BarcelonaHasMessaging BarcelonaOptionalCookiesEnabled BarcelonaShouldFulfillLightboxQuery
	BarcelonaCanSeeSponsoredContent BarcelonaHasUnseenReplies BarcelonaHasWebFavicons BarcelonaIsCrawler
	BarcelonaHasDearAlgoWebProduction BarcelonaHasCommunityTopContributors
	BarcelonaHasCustomLikesConsumption BarcelonaHasViewerReplied BarcelonaHasPrivateRepliesDeprecation
	BarcelonaHasGhostPostEmojiActivation BarcelonaHasGhostPostProduction
	BarcelonaHasPodcastTranscriptConsumption BarcelonaHasPodcastV2Consumption
	BarcelonaHasNewsletterLinkPreview BarcelonaHasPublicViewCountShareCard
	BarcelonaShouldShowFediverseM075Features BarcelonaIsInternalUser`)

// builtinCatalog is the website's queries as of 2026-10-17.
func builtinCatalog() *threadsCatalog {
	replyFlags := append(slices.Clone(threadsFlags), "BarcelonaHasPermalinkIndentation")
	return &threadsCatalog{
		Version: 1,
		Queries: map[string]threadsQuery{
			threadsFeedQuery: {Name: threadsFeedQuery, Doc: "28783769534619828", Kind: "query",
				Vars: words("after before data first last sort_by variant"), Flags: slices.Clone(threadsFlags)},
			threadsRepliesQuery: {Name: threadsRepliesQuery, Doc: "28686245017651778", Kind: "query",
				Vars: words("after filterType first postID sortOrder"), Flags: replyFlags},
			threadsMoreQuery: {Name: threadsMoreQuery, Doc: "30205808475684005", Kind: "query",
				Vars: words("after filterType first postID sortOrder"), Flags: slices.Clone(replyFlags)},
		},
		Providers: map[string]string{
			"BarcelonaCanSeeSponsoredContent":          `return!1`,
			"BarcelonaGenAIRepliesEnabled":             `return r("qex")._("5477")===!0`,
			"BarcelonaHasBestOfThreads":                `return!0`,
			"BarcelonaHasCommunities":                  `return r("gkx")("6542")===!0`,
			"BarcelonaHasCommunityEmojiUpdateCard":     `return r("gkx")("22366")===!0`,
			"BarcelonaHasCommunityEntityCard":          `return r("gkx")("23446")===!0`,
			"BarcelonaHasCommunityTopContributors":     `return r("gkx")("17942")===!0`,
			"BarcelonaHasCustomLikesConsumption":       `return r("gkx")("12807")===!0`,
			"BarcelonaHasDearAlgoConsumption":          `return r("gkx")("4511")`,
			"BarcelonaHasDearAlgoWebProduction":        `return r("gkx")("14866")`,
			"BarcelonaHasEventBadge":                   `return r("gkx")("3960")`,
			"BarcelonaHasGameScoreShare":               `return r("gkx")("18268")===!0`,
			"BarcelonaHasGhostPostEmojiActivation":     `return r("gkx")("1636")===!0`,
			"BarcelonaHasGhostPostProduction":          `return r("qex")._("3294")===!0`,
			"BarcelonaHasMWRLocationConsumption":       `return r("gkx")("27626")===!0`,
			"BarcelonaHasMessaging":                    `return r("gkx")("899")===!0`,
			"BarcelonaHasMetaAiContentAttachments":     `return r("gkx")("22584")===!0`,
			"BarcelonaHasNewsletterLinkPreview":        `return r("gkx")("27455")===!0`,
			"BarcelonaHasNewspaperLinkStyle":           `return r("gkx")("15632")===!0`,
			"BarcelonaHasPermalinkIndentation":         `return r("qex")._("695")===!0`,
			"BarcelonaHasPodcastTranscriptConsumption": `return r("gkx")("13916")===!0`,
			"BarcelonaHasPodcastV2Consumption":         `return r("gkx")("13916")===!0`,
			"BarcelonaHasPrivateRepliesDeprecation":    `return r("gkx")("14002")===!0&&r("gkx")("899")===!0`,
			"BarcelonaHasPublicViewCountCard":          `return r("gkx")("21940")===!0`,
			"BarcelonaHasPublicViewCountShareCard":     `return r("gkx")("1708")===!0`,
			"BarcelonaHasScorecardCommunity":           `return r("gkx")("22721")===!0`,
			"BarcelonaHasSportTeamAllegianceCard":      `var e;return(e=r("qex")._("518"))!=null?e:!1`,
			"BarcelonaHasStitchedAttachment":           `return r("gkx")("17978")===!0`,
			"BarcelonaHasUnseenReplies":                `return r("qex")._("4226")===!0`,
			"BarcelonaHasViewerReplied":                `return r("gkx")("7316")`,
			"BarcelonaHasWebFavicons":                  `return r("qex")._("1489")===!0||r("gkx")("4906")===!0`,
			"BarcelonaIsCrawler":                       `return r("gkx")("22947")`,
			"BarcelonaIsInternalUser":                  `return r("gkx")("8271")`,
			"BarcelonaIsLoggedIn":                      `return r("gkx")("7479")`,
			"BarcelonaIsSearchDiscoveryEnabled":        `return r("gkx")("10813")`,
			"BarcelonaMessagesHasLiveChatMessaging":    `return r("gkx")("18140")`,
			"BarcelonaMessagingHasMetaAIBot":           `return r("gkx")("14171")===!0`,
			"BarcelonaOptionalCookiesEnabled":          `return r("gkx")("8583")`,
			"BarcelonaShouldFetchPostAuthorFullName":   `return r("BarcelonaIsLoggedIn.relayprovider").get()&&r("gkx")("27036")===!0&&r("qex")._("3519")!==!1`,
			"BarcelonaShouldFulfillLightboxQuery":      `return r("gkx")("6301")`,
			"BarcelonaShouldShowFediverseM075Features": `return r("MetaConfig")._("92")`,
		},
	}
}

// usable reports whether c has the three queries, each a query with a doc
// id: a saved catalog that hasn't isn't used.
func (c *threadsCatalog) usable() bool {
	if c == nil || c.Version != 1 {
		return false
	}
	for _, name := range []string{threadsFeedQuery, threadsRepliesQuery, threadsMoreQuery} {
		q, ok := c.Queries[name]
		if !ok || q.Kind != "query" || !threadsDigit.MatchString(q.Doc) {
			return false
		}
	}
	return true
}

// errNotAQuery refuses an operation that isn't a query the website defines:
// a mutation would change something on Threads, and the helper only reads.
var errNotAQuery = errors.New("threads: not a query")

// query is name's definition, if it's a query: the only kind sent.
func (c *threadsCatalog) query(name string) (threadsQuery, error) {
	q, ok := c.Queries[name]
	if !ok || q.Kind != "query" || strings.Contains(strings.ToLower(name), "mutation") ||
		!strings.HasSuffix(name, "Query") || !threadsDigit.MatchString(q.Doc) {
		return threadsQuery{}, errNotAQuery
	}
	return q, nil
}

// variables are what q is sent with: given's values for the variables q
// declares (null for those not given, as the website sends them), and each
// feature flag's value as the website's code works it out from page. A
// variable q doesn't declare isn't sent.
func (c *threadsCatalog) variables(q threadsQuery, given map[string]any, page *threadsPage) map[string]any {
	out := make(map[string]any, len(q.Vars)+len(q.Flags))
	for _, v := range q.Vars {
		out[v] = given[v]
	}
	for _, f := range q.Flags {
		out[flagPrefix+f+flagSuffix] = c.flag(f, page)
	}
	return out
}

// flag is a feature flag's value: what its provider's get() returns on
// page, false when that can't be worked out (an unknown provider, or code
// of a shape it hasn't had), which is a feature turned off.
func (c *threadsCatalog) flag(provider string, page *threadsPage) bool {
	v, err := c.evalProvider(provider, page, 0)
	return err == nil && truthy(v)
}

// maxProviderDepth bounds providers that read other providers.
const maxProviderDepth = 4

func (c *threadsCatalog) evalProvider(provider string, page *threadsPage, depth int) (any, error) {
	code, ok := c.Providers[provider]
	if !ok || depth > maxProviderDepth {
		return nil, errBadFlag
	}
	ex := flagExpr{src: code, page: page, cat: c, depth: depth}
	return ex.run()
}

var errBadFlag = errors.New("threads: a flag's code can't be read")

// flagExpr reads the code of a provider's get(): the shapes the website's
// providers have, and nothing else. Gates (gkx) read as the page shipped
// them, false when it didn't, as the website's own code has them;
// experiments (qex) and settings (MetaConfig) as shipped, null when not.
type flagExpr struct {
	src   string
	at    int
	page  *threadsPage
	cat   *threadsCatalog
	depth int
}

// orFalse is `var e;return(e=X)!=null?e:!1`: X, or false if it's null.
var orFalse = regexp.MustCompile(`^var e;return\(e=(.+)\)!=null\?e:!1$`)

func (ex *flagExpr) run() (any, error) {
	src := strings.TrimSpace(ex.src)
	nullIsFalse := false
	if m := orFalse.FindStringSubmatch(src); m != nil {
		src, nullIsFalse = m[1], true
	} else if rest, ok := strings.CutPrefix(src, "return"); ok {
		src = rest
	} else {
		return nil, errBadFlag
	}
	ex.src, ex.at = strings.TrimSpace(src), 0
	v, err := ex.or()
	if err != nil {
		return nil, err
	}
	if ex.at != len(ex.src) {
		return nil, errBadFlag
	}
	if nullIsFalse && v == nil {
		return false, nil
	}
	return v, nil
}

func (ex *flagExpr) eat(tok string) bool {
	if strings.HasPrefix(ex.src[ex.at:], tok) {
		ex.at += len(tok)
		return true
	}
	return false
}

func (ex *flagExpr) or() (any, error) {
	v, err := ex.and()
	for err == nil && ex.eat("||") {
		var w any
		if w, err = ex.and(); err == nil && !truthy(v) {
			v = w
		}
	}
	return v, err
}

func (ex *flagExpr) and() (any, error) {
	v, err := ex.eq()
	for err == nil && ex.eat("&&") {
		var w any
		if w, err = ex.eq(); err == nil && truthy(v) {
			v = w
		}
	}
	return v, err
}

func (ex *flagExpr) eq() (any, error) {
	v, err := ex.atom()
	for err == nil {
		var not bool
		switch {
		case ex.eat("==="):
		case ex.eat("!=="):
			not = true
		default:
			return v, nil
		}
		var w any
		if w, err = ex.atom(); err == nil {
			v = same(v, w) != not
		}
	}
	return v, err
}

var (
	flagGate     = regexp.MustCompile(`^r\("gkx"\)\("(\d+)"\)`)
	flagExp      = regexp.MustCompile(`^r\("qex"\)\._\("(\d+)"\)`)
	flagSetting  = regexp.MustCompile(`^r\("MetaConfig"\)\._\("(\d+)"\)`)
	flagProvider = regexp.MustCompile(`^r\("([A-Za-z0-9]+)\.relayprovider"\)\.get\(\)`)
)

func (ex *flagExpr) atom() (any, error) {
	rest := ex.src[ex.at:]
	switch {
	case ex.eat("!0"):
		return true, nil
	case ex.eat("!1"):
		return false, nil
	case ex.eat("null"):
		return nil, nil
	case ex.eat("("):
		v, err := ex.or()
		if err == nil && !ex.eat(")") {
			err = errBadFlag
		}
		return v, err
	}
	if m := flagGate.FindStringSubmatch(rest); m != nil {
		ex.at += len(m[0])
		return ex.page != nil && ex.page.gkx[m[1]], nil
	}
	if m := flagExp.FindStringSubmatch(rest); m != nil {
		ex.at += len(m[0])
		return ex.shipped(func(p *threadsPage) json.RawMessage { return p.qex[m[1]] }), nil
	}
	if m := flagSetting.FindStringSubmatch(rest); m != nil {
		ex.at += len(m[0])
		return ex.shipped(func(p *threadsPage) json.RawMessage { return p.config[m[1]] }), nil
	}
	if m := flagProvider.FindStringSubmatch(rest); m != nil {
		ex.at += len(m[0])
		return ex.cat.evalProvider(m[1], ex.page, ex.depth+1)
	}
	return nil, errBadFlag
}

// shipped is an experiment's or setting's value as the page shipped it.
func (ex *flagExpr) shipped(of func(*threadsPage) json.RawMessage) any {
	if ex.page == nil {
		return nil
	}
	var v any
	if raw := of(ex.page); raw == nil || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// truthy is JavaScript's.
func truthy(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	}
	return false
}

// same is JavaScript's ===.
func same(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case bool:
		bb, ok := b.(bool)
		return ok && a == bb
	case float64:
		bf, ok := b.(float64)
		return ok && a == bf
	case string:
		bs, ok := b.(string)
		return ok && a == bs
	}
	return false
}

// --- reading the definitions again -------------------------------------------

// discovery reads the website's bundles one at a time for what the wanted
// queries need: each one's doc id, kind and variables, and the code of the
// providers their flags (and those providers) read.
type discovery struct {
	want      []string
	docs      map[string]string
	kinds     map[string]string
	vars      map[string][]string
	providers map[string]string
}

func newDiscovery(want ...string) *discovery {
	return &discovery{want: want, docs: map[string]string{}, kinds: map[string]string{},
		vars: map[string][]string{}, providers: map[string]string{}}
}

var (
	discProvider = regexp.MustCompile(`__d\("([A-Za-z0-9]+)\.relayprovider",\[[^\]]*\],\(function\([a-z,]+\)\{"use strict";var e=\{get:function\(\)\{(.{0,400}?)\}\};[il]\.default=e\}\),\d+\);`)
	discArg      = regexp.MustCompile(`kind:"LocalArgument",name:"([A-Za-z0-9_]+)"`)
)

// scan reads one bundle; true if it held anything of what's wanted.
func (d *discovery) scan(js string) bool {
	found := false
	for _, q := range d.want {
		qq := regexp.QuoteMeta(q)
		params := regexp.MustCompile(`params:\{id:(?:"(\d+)"|[a-z]\("` + qq + `_threadsRelayOperation"\)),metadata:\{[^}]*\},name:"` + qq + `",operationKind:"(\w+)"`)
		if m := params.FindStringSubmatch(js); m != nil {
			found = true
			d.kinds[q] = m[2]
			if m[1] != "" {
				d.docs[q] = m[1]
			}
		}
		op := regexp.MustCompile(`__d\("` + qq + `_threadsRelayOperation",\[\],\(function\([a-z,]+\)\{[a-z]\.exports="(\d+)"\}\)`)
		if m := op.FindStringSubmatch(js); m != nil {
			found = true
			d.docs[q] = m[1]
		}
		// The full definition is the module that declares the most
		// variables: a parameters-only module names the query too.
		mark := `name:"` + q + `",operationKind:"`
		for from := 0; ; {
			i := strings.Index(js[from:], mark)
			if i < 0 {
				break
			}
			i += from
			start := max(strings.LastIndex(js[:i], "__d("), 0)
			var args []string
			for _, a := range discArg.FindAllStringSubmatch(js[start:i], -1) {
				if !slices.Contains(args, a[1]) {
					args = append(args, a[1])
				}
			}
			if len(args) > len(d.vars[q]) {
				d.vars[q] = args
				found = true
			}
			from = i + len(mark)
		}
	}
	for _, m := range discProvider.FindAllStringSubmatch(js, -1) {
		if _, ok := d.providers[m[1]]; !ok {
			d.providers[m[1]] = m[2]
			found = true
		}
	}
	return found
}

// queries are the wanted queries found so far, whole.
func (d *discovery) queries() map[string]threadsQuery {
	out := map[string]threadsQuery{}
	for _, q := range d.want {
		doc, kind, vars := d.docs[q], d.kinds[q], d.vars[q]
		if doc == "" || kind == "" || len(vars) == 0 {
			continue
		}
		def := threadsQuery{Name: q, Doc: doc, Kind: kind}
		for _, v := range vars {
			if f, ok := strings.CutPrefix(v, flagPrefix); ok && strings.HasSuffix(f, flagSuffix) {
				def.Flags = append(def.Flags, strings.TrimSuffix(f, flagSuffix))
			} else {
				def.Vars = append(def.Vars, v)
			}
		}
		out[q] = def
	}
	return out
}

// done reports whether every wanted query is whole and every provider they
// read, directly or through another, has its code.
func (d *discovery) done() bool {
	qs := d.queries()
	if len(qs) != len(d.want) {
		return false
	}
	var todo []string
	for _, q := range qs {
		todo = append(todo, q.Flags...)
	}
	seen := map[string]bool{}
	for len(todo) > 0 {
		p := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[p] {
			continue
		}
		seen[p] = true
		code, ok := d.providers[p]
		if !ok {
			return false
		}
		for _, m := range regexp.MustCompile(`r\("([A-Za-z0-9]+)\.relayprovider"\)`).FindAllStringSubmatch(code, -1) {
			todo = append(todo, m[1])
		}
	}
	return true
}

// merge is c with what was found in place of what it had: whole queries,
// and providers' code (a provider not found keeps the code it had).
func (d *discovery) merge(c *threadsCatalog) *threadsCatalog {
	out := &threadsCatalog{Version: 1, Queries: map[string]threadsQuery{}, Providers: map[string]string{},
		Empty: slices.Clone(c.Empty)}
	for k, v := range c.Queries {
		out.Queries[k] = v
	}
	for k, v := range d.queries() {
		out.Queries[k] = v
	}
	for k, v := range c.Providers {
		out.Providers[k] = v
	}
	for k, v := range d.providers {
		out.Providers[k] = v
	}
	return out
}

// bundleKey is how Empty names a bundle.
func bundleKey(u string) string {
	sum := sha256.Sum256([]byte(u))
	return hex.EncodeToString(sum[:12])
}

// isEmpty reports whether a rediscovery found nothing in bundle u.
func (c *threadsCatalog) isEmpty(u string) bool { return slices.Contains(c.Empty, bundleKey(u)) }

// markEmpty remembers that bundle u holds nothing wanted, forgetting the
// oldest past maxEmpty.
func (c *threadsCatalog) markEmpty(u string) {
	k := bundleKey(u)
	if slices.Contains(c.Empty, k) {
		return
	}
	c.Empty = append(c.Empty, k)
	if over := len(c.Empty) - maxEmpty; over > 0 {
		c.Empty = slices.Delete(c.Empty, 0, over)
	}
}
