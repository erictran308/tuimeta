// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"errors"
	"fmt"
	stdnet "net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	mtypes "go.mau.fi/mautrix-meta/pkg/messagix/types"

	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestLoginErrorsSaySomethingAPersonCanDo(t *testing.T) {
	token := "https://www.facebook.com/checkpoint/?next=SECRET"
	cases := []struct {
		err  error
		code proto.Code
	}{
		{httpclient.RedirectedError{Type: httpclient.ErrCheckpointRequired, URL: token}, proto.Checkpoint},
		{fmt.Errorf("load: %w", httpclient.RedirectedError{Type: httpclient.ErrChallengeRequired, URL: token}), proto.Checkpoint},
		{httpclient.RedirectedError{Type: httpclient.ErrConsentRequired, URL: token}, proto.Checkpoint},
		{httpclient.RedirectedError{Type: httpclient.ErrAccountSuspended, URL: token}, proto.Checkpoint},
		{fmt.Errorf("%w to %s", httpclient.ErrTokenInvalidatedRedirect, token), proto.BadCookies},
		{httpclient.ErrUserIDIsZero, proto.BadCookies},
		{&mtypes.ErrorResponse{ErrorCode: 1357053}, proto.BadCookies},
		{fmt.Errorf("x: %w", httpclient.ErrRateLimited), proto.NetworkError},
		{&stdnet.OpError{Op: "dial", Err: errors.New("refused " + token)}, proto.NetworkError},
		{fmt.Errorf("%w: %w", httpclient.ErrMaxRetriesReached, errors.New(token)), proto.NetworkError},
		{context.DeadlineExceeded, proto.NetworkError},
		{errors.New("something new " + token), proto.NetworkError},
	}
	for i, tc := range cases {
		err := loginError(tc.err)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != tc.code {
			t.Errorf("case %d: %v → %v", i, tc.err, err)
			continue
		}
		if strings.Contains(pe.Message, "SECRET") || pe.Message == "" || !strings.HasSuffix(pe.Message, ".") {
			t.Errorf("case %d: message %q", i, pe.Message)
		}
	}
	if loginError(httpclient.ErrCheckpointRequired) != errCheckpoint || !strings.Contains(errCheckpoint.Message, "confirm it's you") {
		t.Error("checkpoint sentence")
	}
	for _, err := range []error{whatsmeow.ErrNotConnected, whatsmeow.ErrMessageTimedOut, httpclient.ErrTokenInvalidated, errors.New("odd " + token)} {
		var pe *proto.Error
		if !errors.As(requestError(err), &pe) || strings.Contains(pe.Message, "SECRET") {
			t.Errorf("request error for %v: %v", err, requestError(err))
		}
	}
}

func TestStartWithoutASessionIsLoggedOut(t *testing.T) {
	h := newHarness(t)
	h.m.Start(context.Background())
	acc := h.rec.of("account")
	if len(acc) != 1 || acc[0].(proto.AccountEvent).State != proto.LoggedOut {
		t.Errorf("account %+v", acc)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAResumedSessionFacebookRefusesBecomesAnError(t *testing.T) {
	h := newHarness(t)
	if err := h.deps.Session.SaveJSON(sessionFile, savedSession{Version: 1, UserID: selfID, Cookies: map[string]string{"c_user": "100001", "xs": "old"}}); err != nil {
		t.Fatal(err)
	}
	var tries atomic.Int32
	h.m.dial = func(_ context.Context, _ context.Context, values map[string]string) error {
		if values["xs"] != "old" {
			t.Errorf("resumed with %v", len(values))
		}
		if tries.Add(1) == 1 {
			return errNetwork // the network first: tried again
		}
		return errCheckpoint
	}
	retryWait = time.Millisecond
	defer func() { retryWait = 2 * time.Second }()
	h.m.Start(context.Background())
	waitFor(t, "the error", func() bool { return len(h.rec.of("account")) >= 2 })
	acc := h.rec.of("account")
	first, last := acc[0].(proto.AccountEvent), acc[len(acc)-1].(proto.AccountEvent)
	if first.State != proto.Connecting || last.State != proto.Errored || last.Error != errCheckpoint.Message || tries.Load() != 2 {
		t.Errorf("account %+v (tries %d)", acc, tries.Load())
	}
}

func TestAFailedLoginSavesNothing(t *testing.T) {
	h := newHarness(t)
	h.m.dial = func(context.Context, context.Context, map[string]string) error { return errBadCookies }
	set, err := cookies.Parse(proto.Messenger, "c_user=1; xs=2; datr=3")
	if err != nil {
		t.Fatal(err)
	}
	err = h.m.LoginCookies(context.Background(), set)
	if err != errBadCookies {
		t.Errorf("err %v", err)
	}
	if ok, _ := h.deps.Session.LoadJSON(sessionFile, &savedSession{}); ok {
		t.Error("a failed login was saved")
	}
	acc := h.rec.of("account")
	if len(acc) != 2 || acc[0].(proto.AccountEvent).State != proto.Connecting || acc[1].(proto.AccountEvent).State != proto.LoggedOut {
		t.Errorf("account %+v", acc)
	}
}

func TestLogoutDisconnectsLocallyAndForgetsEverythingButKeepsTheWebSession(t *testing.T) {
	h := newHarness(t)
	if err := h.deps.Session.SaveJSON(sessionFile, savedSession{Version: 1, Cookies: map[string]string{"xs": "1"}}); err != nil {
		t.Fatal(err)
	}
	dir, _ := h.deps.Session.Dir()
	h.load()
	if err := h.m.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A local logout: the connection is dropped, but facebook.com's own
	// "Log out" is never called, so the browser's session stays valid.
	if !h.meta.closed {
		t.Error("the connection wasn't dropped")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the session folder is still there: %v", err)
	}
	acc := h.rec.of("account")
	if len(acc) != 1 || acc[0].(proto.AccountEvent).State != proto.LoggedOut {
		t.Errorf("account %+v", acc)
	}
	if _, err := h.m.connected(); err == nil || len(h.m.chats) != 0 {
		t.Error("still connected after logout")
	}
}

func TestCloseKeepsTheCookiesMessengerLastSetAndReturnsFast(t *testing.T) {
	h := newHarness(t)
	h.meta.cookies = map[string]string{"c_user": "100001", "xs": "refreshed"}
	start := time.Now()
	h.m.Close()
	if time.Since(start) > time.Second {
		t.Error("Close was slow")
	}
	var s savedSession
	if ok, err := h.deps.Session.LoadJSON(sessionFile, &s); !ok || err != nil || s.Cookies["xs"] != "refreshed" {
		t.Errorf("session %+v %v %v", s, ok, err)
	}
	if !h.meta.closed {
		t.Error("not disconnected")
	}
	if len(h.rec.of("account")) != 0 {
		t.Error("closing told tuimeta something")
	}
}

func TestLoadChatsBeforeConnectingSaysSo(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.ready = false
	h.m.mu.Unlock()
	if _, err := h.m.LoadChats(context.Background(), 10); err != errNotConnected {
		t.Errorf("err %v", err)
	}
}
