// SPDX-License-Identifier: AGPL-3.0-or-later

package cookies

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestAllThreeFormatsAreRead(t *testing.T) {
	cases := map[string]string{
		"header":         " c_user=100; xs=SECRETxs%3Avalue ; datr=SECRETdatr; fr=x ",
		"header prefix":  "Cookie: c_user=100; xs=SECRETxs%3Avalue; datr=SECRETdatr",
		"array":          `[{"domain":".facebook.com","name":"c_user","value":"100","path":"/"},{"domain":".facebook.com","name":"xs","value":"SECRETxs%3Avalue"},{"domain":".messenger.com","name":"datr","value":"SECRETdatr","httpOnly":true}]`,
		"wrapped array":  `{"url":"https://www.facebook.com","cookies":[{"name":"c_user","value":"100"},{"name":"xs","value":"SECRETxs%3Avalue"},{"name":"datr","value":"SECRETdatr"}]}`,
		"object":         `{"c_user": "100", "xs": "SECRETxs%3Avalue", "datr": "SECRETdatr"}`,
		"numeric object": `{"c_user": 100, "xs": "SECRETxs%3Avalue", "datr": "SECRETdatr"}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := Parse(proto.Messenger, input)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if s.Get("c_user") != "100" || s.Get("xs") != "SECRETxs%3Avalue" || s.Get("datr") != "SECRETdatr" {
				t.Errorf("values = %v", s.Values())
			}
		})
	}
}

func TestInstagramNeedsItsOwnCookies(t *testing.T) {
	_, err := Parse(proto.Instagram, "c_user=1; xs=2; datr=3")
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.BadCookies {
		t.Fatalf("err = %v", err)
	}
	for _, name := range []string{"sessionid", "ds_user_id", "csrftoken"} {
		if !strings.Contains(pe.Message, name) {
			t.Errorf("the error doesn't name %s: %s", name, pe.Message)
		}
	}
	if _, err := Parse(proto.Instagram, "sessionid=a; ds_user_id=2; csrftoken=c"); err != nil {
		t.Errorf("good instagram cookies: %v", err)
	}
}

func TestCookiesFromOtherSitesInAnExportAreIgnored(t *testing.T) {
	input := `[{"domain":"example.com","name":"sessionid","value":"WRONG"},{"domain":".instagram.com","name":"sessionid","value":"right"},{"domain":"www.instagram.com","name":"ds_user_id","value":"2"},{"domain":"instagram.com","name":"csrftoken","value":"c"}]`
	s, err := Parse(proto.Instagram, input)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get("sessionid") != "right" {
		t.Errorf("sessionid = %q", s.Get("sessionid"))
	}
}

func TestMissingOrEmptyRequiredCookiesAreNamed(t *testing.T) {
	_, err := Parse(proto.Messenger, "c_user=1; xs=; other=SECRETother")
	if err == nil || !strings.Contains(err.Error(), "xs") || !strings.Contains(err.Error(), "datr") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "c_user") {
		t.Errorf("named a cookie that's there: %v", err)
	}
}

func TestErrorsNeverQuoteWhatWasPasted(t *testing.T) {
	bad := []string{
		"SECRETnoequals",
		"c_user=1; SECRETnoequals",
		"c_user=1; xs=SECRET\x01ctl; datr=3",
		"c_user=1; xs=SECRET value; datr=3",
		`SECRET{"name":"c_user"}`,
		`[{"name":"c_user","value":"SECRET1"},{"value":"SECRET2"}]`,
		`[{"name":"c_user","value":{"SECRET":1}}]`,
		`{"SECRET name":"SECRETvalue","xs":"2","datr":"3"}`,
		`{"c_user":"SECRET1"} trailing SECRET`,
		"",
		strings.Repeat("SECRET", MaxInput),
		"c_user=SECRET1; xs=SECRET2",
	}
	for _, input := range bad {
		_, err := Parse(proto.Messenger, input)
		if err == nil {
			t.Errorf("accepted %.40q", input)
			continue
		}
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.BadCookies {
			t.Errorf("%.40q: not bad_cookies: %v", input, err)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%.40q: the error quotes the input: %v", input, err)
		}
	}
}

func TestASetNeverPrintsItsValues(t *testing.T) {
	s, err := Parse(proto.Messenger, "c_user=SECRET1; xs=SECRET2; datr=SECRET3")
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		if out := fmt.Sprintf(verb, s); strings.Contains(out, "SECRET") {
			t.Errorf("%s printed %s", verb, out)
		}
	}
	if out, _ := json.Marshal(s); strings.Contains(string(out), "SECRET") {
		t.Errorf("JSON: %s", out)
	}
	if out, _ := json.Marshal(struct{ C Set }{s}); strings.Contains(string(out), "SECRET") {
		t.Errorf("JSON field: %s", out)
	}
	if s.Header() != "c_user=SECRET1; xs=SECRET2; datr=SECRET3" {
		t.Errorf("Header = %q", s.Header())
	}
}
