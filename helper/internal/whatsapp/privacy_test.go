// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waTypes "go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestTheConnectionCantSetAPresence(t *testing.T) {
	forbidden := []string{"SendPresence", "SubscribePresence", "SetForceActiveDeliveryReceipts", "SetPassive", "SendAppState"}
	for _, typ := range []reflect.Type{reflect.TypeFor[waAPI](), reflect.TypeFor[*waConn]()} {
		for i := range typ.NumMethod() {
			name := typ.Method(i).Name
			for _, f := range forbidden {
				if name == f {
					t.Errorf("%v has %s", typ, name)
				}
			}
			if strings.Contains(name, "Presence") && name != "SendChatPresence" {
				t.Errorf("%v has %s", typ, name)
			}
		}
	}
}

func TestReceiptsAndTypingRefuseAnyOtherRequest(t *testing.T) {
	// The live connection checks before whatsmeow is even looked at.
	conn := &waConn{}
	for _, purpose := range []string{"send", "history", "search", "react", "edit", "open_dm", "mute", ""} {
		if err := conn.MarkRead(context.Background(), purpose, []waTypes.MessageID{"x"}, time.Now(), alicePN, waTypes.EmptyJID); err != errUnasked {
			t.Errorf("a read receipt for %q: %v", purpose, err)
		}
		if err := conn.SendChatPresence(context.Background(), purpose, alicePN, waTypes.ChatPresenceComposing); err != errUnasked {
			t.Errorf("typing for %q: %v", purpose, err)
		}
	}
}

func TestAReadIsToldToEveryoneOnlyWhenTheSettingSaysSo(t *testing.T) {
	for setting, want := range map[waTypes.PrivacySetting]waTypes.ReceiptType{
		waTypes.PrivacySettingAll: waTypes.ReceiptTypeRead, waTypes.PrivacySettingNone: waTypes.ReceiptTypeReadSelf,
		waTypes.PrivacySettingContacts: waTypes.ReceiptTypeReadSelf, "": waTypes.ReceiptTypeReadSelf,
	} {
		if got := receiptKind(&waTypes.PrivacySettings{ReadReceipts: setting}); got != want {
			t.Errorf("%q: %q", setting, got)
		}
	}
	if receiptKind(nil) != waTypes.ReceiptTypeReadSelf {
		t.Error("no setting told everyone")
	}
	// Each connection asks WhatsApp afresh: whatsmeow's copy may be from
	// before a change made while this device was offline.
	h := newHarness(t)
	conn := newConn(h.wa.real)
	conn.settled.Store(true)
	conn.reconnected()
	if conn.settled.Load() {
		t.Error("a new connection trusts the setting it had")
	}
	// Without the setting, nothing goes.
	if err := conn.MarkRead(context.Background(), "mark_read", []waTypes.MessageID{"x"}, time.Now(), alicePN, waTypes.EmptyJID); err == nil {
		t.Error("a read receipt went without the setting")
	}
}

func TestWhatsmeowIsConfiguredToKeepQuiet(t *testing.T) {
	h := newHarness(t)
	dev := h.w.st.container.NewDevice()
	cli := whatsmeow.NewClient(dev, waLog.Noop)
	configure(cli)
	if cli.UseRetryMessageStore || !cli.SynchronousAck || !cli.EnableAutoReconnect {
		t.Errorf("client %+v", cli)
	}
	describeDevice()
	if !strings.HasPrefix(pairName(), "Chrome (") || pairName() != "Chrome ("+systemName()+")" {
		t.Errorf("pair name %q", pairName())
	}
}

func TestTheLogNeverHoldsWhatPeopleWroteTheirNamesOrNumbers(t *testing.T) {
	var buf bytes.Buffer
	hlog.SetOutput(&buf)
	t.Cleanup(func() { hlog.SetOutput(io.Discard) })
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "the secret plan"), aliceLID))
	h.wa.sendErr = context.DeadlineExceeded
	c := h.chat(aliceLID.String())
	_ = h.w.Send(context.Background(), h.outgoing(c, "my reply", nil))
	h.wa.sendErr = nil
	h.w.Fetch(context.Background(), mustFile(t, h), &bytes.Buffer{})
	for _, secret := range []string{"secret plan", "my reply", "Alice", "447700900101", "100000000000002", "Robin"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("the log has %q:\n%s", secret, buf.String())
		}
	}
}

// mustFile is a file of the harness's, for a fetch that fails.
func mustFile(t *testing.T, h *harness) ids.FileRef {
	t.Helper()
	h.w.mu.Lock()
	img := h.w.avatar(alicePN.String(), "")
	h.w.mu.Unlock()
	got, ok := h.deps.Files.Get(img.FileID)
	if !ok {
		t.Fatal("no file")
	}
	return got
}

func TestTheStoreIsPrivateAndKeepsAtMostThatManyMessagesAChat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, storeFile)
	st, err := openStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info.Mode(), err)
	}
	var many []*message
	for i := range MaxStoredPerChat + 5 {
		many = append(many, &message{ID: "M" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), Sender: "s", MS: int64(i)})
	}
	trimmed, err := st.putMessages(context.Background(), "chat", many...)
	if err != nil {
		t.Fatal(err)
	}
	if len(trimmed) != 5 || trimmed[0].MS >= 5 {
		t.Errorf("the trim says it took %d", len(trimmed))
	}
	kept, err := st.messages(context.Background(), "chat")
	if err != nil || len(kept) != MaxStoredPerChat || kept[0].MS != 5 {
		t.Fatalf("kept %d from %d: %v", len(kept), kept[0].MS, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil && info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", suffix, info.Mode())
		}
	}
}

func TestALinkInTheStoresPlaceIsRefusedNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links aren't refused on Windows")
	}
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.db")
	if err := os.WriteFile(elsewhere, []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, storeFile)
	if err := os.Symlink(elsewhere, planted); err != nil {
		t.Fatal(err)
	}
	if st, err := openStore(context.Background(), planted); err == nil {
		st.Close()
		t.Error("the store was opened through a link")
	}
	if data, _ := os.ReadFile(elsewhere); string(data) != "theirs" {
		t.Errorf("written through: %q", data)
	}
	if info, _ := os.Stat(elsewhere); info.Mode().Perm() != 0o644 {
		t.Errorf("the link's target was chmodded: %v", info.Mode())
	}
	// Nor one pointing at nothing yet: nothing is made where it points.
	dangling := filepath.Join(t.TempDir(), storeFile)
	nowhere := filepath.Join(dir, "nothing-yet")
	if err := os.Symlink(nowhere, dangling); err != nil {
		t.Fatal(err)
	}
	if st, err := openStore(context.Background(), dangling); err == nil {
		st.Close()
		t.Error("the store was opened through a dangling link")
	}
	if _, err := os.Lstat(nowhere); !os.IsNotExist(err) {
		t.Error("a file was made where the dangling link pointed")
	}
}

func TestDisappearingMessagesGoWhenTheirTimeIsUp(t *testing.T) {
	h := newHarness(t)
	h.load()
	evt := text(benLID, benLID, "B1", -1, "this goes")
	h.event(evt)
	c := h.chat(benLID.String())
	h.w.mu.Lock()
	c.msgs["B1"].Expires = at(1).UnixMilli()
	h.w.store(c, c.msgs["B1"])
	st := h.w.st
	h.w.mu.Unlock()
	h.w.sweep(st)
	if len(h.rec.deletions()) != 0 {
		t.Fatal("deleted before its time")
	}
	h.w.now = func() time.Time { return at(2) }
	h.w.sweep(st)
	if d := h.rec.deletions(); len(d) != 1 {
		t.Fatalf("deletions %+v", d)
	}
	kept, _ := h.w.st.messages(context.Background(), c.key)
	if len(kept) != 0 {
		t.Error("it's still in the store")
	}
}

func TestLogoutUnlinksTheDeviceAndForgetsEverything(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -1, "hi"))
	if err := h.w.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.wa.loggedOut {
		t.Error("the device wasn't unlinked")
	}
	if _, err := os.Stat(filepath.Join(h.dir, string(proto.WhatsApp), storeFile)); !os.IsNotExist(err) {
		t.Errorf("the store is still there: %v", err)
	}
	accs := h.rec.accounts()
	if len(accs) == 0 || accs[len(accs)-1].State != proto.LoggedOut {
		t.Errorf("accounts %+v", accs)
	}
}

func TestPicturesComeOnlyFromWhatsAppsHosts(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://pps.whatsapp.net/v/t61/pic.jpg?oe=1": true,
		"https://whatsapp.net/x":                      true,
		"http://pps.whatsapp.net/x":                   false,
		"https://pps.whatsapp.net.evil.com/x":         false,
		"https://evilwhatsapp.net/x":                  false,
		"https://user@pps.whatsapp.net/x":             false,
		"https://example.com/?u=whatsapp.net":         false,
	} {
		if allowedMediaURL(raw) != ok {
			t.Errorf("%s: want %v", raw, ok)
		}
	}
}

// TestNothingInThePackageCanSayYoureAroundOrHaveRead reads the backend's own
// code: no presence call at all, and read receipts and typing only from
// the one request each is for.
func TestNothingInThePackageCanSayYoureAroundOrHaveRead(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	count := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		for _, forbidden := range []string{"SendPresence(", "SubscribePresence(", "SetPassive(", "SetForceActiveDeliveryReceipts(true"} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s calls %s", f, forbidden)
			}
		}
		count["mark_read"] += strings.Count(src, `.MarkRead(ctx, "mark_read"`)
		count["typing"] += strings.Count(src, `.SendChatPresence(ctx, "typing"`)
		count["app state"] += strings.Count(src, "SendAppState(")
	}
	if count["mark_read"] != 1 || count["typing"] != 1 || count["app state"] != 1 {
		t.Errorf("calls %v: one read receipt (MarkRead), one typing (SetTyping), one app state (mute)", count)
	}
}
