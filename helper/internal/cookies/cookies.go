// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cookies reads the cookies a person pastes to log in: a Cookie
// header's value, or the JSON a browser extension exports. Values are a
// login: they never reach an error, a log or the wire. Set prints as
// "[3 cookies]" however it's formatted.
package cookies

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// MaxInput bounds what's pasted (a whole browser's export is still far less).
const MaxInput = 1 << 20

// Required names the cookies each network's login needs.
var Required = map[proto.Network][]string{
	proto.Messenger: {"c_user", "xs", "datr"},
	proto.Instagram: {"sessionid", "ds_user_id", "csrftoken"},
}

// domains are the sites whose cookies a network's login may use, for exports
// that say which site each cookie is from.
var domains = map[proto.Network][]string{
	proto.Messenger: {"facebook.com", "messenger.com"},
	proto.Instagram: {"instagram.com"},
}

// Cookie is one name and value.
type Cookie struct{ Name, Value string }

// Set is the cookies of one login, in the order they were given; the first
// of two with one name wins.
type Set struct{ list []Cookie }

func bad(msg string) error { return proto.Err(proto.BadCookies, msg) }

// Parse reads cookies for the network n and checks the required ones are
// there. Errors are *proto.Error with code bad_cookies and never quote what
// was pasted.
func Parse(n proto.Network, input string) (Set, error) {
	if _, ok := Required[n]; !ok {
		return Set{}, bad(n.Title() + " doesn't log in with cookies.")
	}
	if len(input) > MaxInput {
		return Set{}, bad("That's too long to be cookies; copy just the site's cookies.")
	}
	s, err := parse(n, strings.TrimSpace(input))
	if err != nil {
		return Set{}, err
	}
	if err := s.require(Required[n]); err != nil {
		return Set{}, err
	}
	return s, nil
}

func parse(n proto.Network, input string) (Set, error) {
	switch {
	case input == "":
		return Set{}, bad("Paste the cookies of a logged-in " + site(n) + " tab.")
	case input[0] == '[':
		return parseArray(n, []byte(input))
	case input[0] == '{':
		return parseObject(n, []byte(input))
	default:
		return parseHeader(input)
	}
}

func site(n proto.Network) string {
	if n == proto.Instagram {
		return "instagram.com"
	}
	return "facebook.com"
}

func parseHeader(input string) (Set, error) {
	if len(input) >= 7 && strings.EqualFold(input[:7], "cookie:") {
		input = input[7:]
	}
	var s Set
	for i, part := range strings.Split(input, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			return Set{}, bad(fmt.Sprintf("Cookie %d has no '=': paste the Cookie header's value, like name=value; name=value.", i+1))
		}
		if err := s.add(i+1, name, value); err != nil {
			return Set{}, err
		}
	}
	return s, nil
}

type exported struct {
	Name   *string         `json:"name"`
	Value  json.RawMessage `json:"value"`
	Domain string          `json:"domain"`
}

func parseArray(n proto.Network, input []byte) (Set, error) {
	var items []exported
	if err := strictJSON(input, &items); err != nil {
		return Set{}, bad("The cookies look like JSON but can't be read; export them again.")
	}
	var s Set
	for i, it := range items {
		if it.Name == nil {
			return Set{}, bad(fmt.Sprintf("Cookie %d in the JSON has no name.", i+1))
		}
		if it.Domain != "" && !fromSite(n, it.Domain) {
			continue
		}
		value, ok := jsonValue(it.Value)
		if !ok {
			return Set{}, bad(fmt.Sprintf("Cookie %d in the JSON has no text value.", i+1))
		}
		if err := s.add(i+1, *it.Name, value); err != nil {
			return Set{}, err
		}
	}
	return s, nil
}

func parseObject(n proto.Network, input []byte) (Set, error) {
	// Some extensions wrap the list: {"url": …, "cookies": [ … ]}.
	var wrapped struct {
		Cookies json.RawMessage `json:"cookies"`
	}
	if strictJSON(input, &wrapped) == nil && len(wrapped.Cookies) > 0 && wrapped.Cookies[0] == '[' {
		return parseArray(n, wrapped.Cookies)
	}
	var fields map[string]json.RawMessage
	if err := strictJSON(input, &fields); err != nil {
		return Set{}, bad("The cookies look like JSON but can't be read; export them again.")
	}
	// Sorted, so the numbers errors give the cookies don't change run to run.
	var s Set
	i := 0
	for _, name := range sortedKeys(fields) {
		i++
		value, ok := jsonValue(fields[name])
		if !ok {
			return Set{}, bad(fmt.Sprintf("Cookie %d in the JSON has no text value.", i))
		}
		if err := s.add(i, name, value); err != nil {
			return Set{}, err
		}
	}
	return s, nil
}

func strictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

// jsonValue accepts a string or a number (ds_user_id is sometimes exported
// as one).
func jsonValue(raw json.RawMessage) (string, bool) {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str, true
	}
	var num json.Number
	if json.Unmarshal(raw, &num) == nil {
		if _, err := strconv.ParseFloat(num.String(), 64); err == nil {
			return num.String(), true
		}
	}
	return "", false
}

func fromSite(n proto.Network, domain string) bool {
	domain = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "."))
	for _, d := range domains[n] {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

func (s *Set) add(i int, name, value string) error {
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if !token(name) {
		return bad(fmt.Sprintf("Cookie %d's name isn't a cookie name.", i))
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == ';' || r == ' ' || r == '\t' {
			return bad(fmt.Sprintf("Cookie %d's value has characters a cookie can't have.", i))
		}
	}
	if s.Get(name) != "" {
		return nil
	}
	s.list = append(s.list, Cookie{name, value})
	return nil
}

// token is RFC 7230's: what a cookie name may be made of.
func token(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f || strings.IndexByte(`()<>@,;:\"/[]?={}`, c) >= 0 {
			return false
		}
	}
	return true
}

func (s Set) require(names []string) error {
	var missing []string
	for _, n := range names {
		if s.Get(n) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return bad("Missing cookies: " + strings.Join(missing, ", ") + ". Copy them from a tab where you're logged in.")
	}
	return nil
}

// Get is the named cookie's value, or "".
func (s Set) Get(name string) string {
	for _, c := range s.list {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// Len is how many cookies there are.
func (s Set) Len() int { return len(s.list) }

// Values hands the cookies out by name, to give to a library or save.
func (s Set) Values() map[string]string {
	m := make(map[string]string, len(s.list))
	for _, c := range s.list {
		m[c.Name] = c.Value
	}
	return m
}

// Header is the cookies as a Cookie header's value.
func (s Set) Header() string {
	parts := make([]string, len(s.list))
	for i, c := range s.list {
		parts[i] = c.Name + "=" + c.Value
	}
	return strings.Join(parts, "; ")
}

// String hides the values.
func (s Set) String() string { return fmt.Sprintf("[%d cookies]", len(s.list)) }

// Format hides the values whatever the verb (%v, %+v, %#v, %s, %q).
func (s Set) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

// MarshalJSON hides the values too; save Values() explicitly.
func (s Set) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
