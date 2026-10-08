// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waAdv"
	wastore "go.mau.fi/whatsmeow/store"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/session"
)

// fresh is a backend on dir that hasn't started.
func fresh(t *testing.T, dir string) (*WhatsApp, *recorder) {
	t.Helper()
	rec := &recorder{}
	events := backend.NewEvents(rec.add)
	idStore, err := ids.Open(filepath.Join(dir, "ids.json"))
	if err != nil {
		t.Fatal(err)
	}
	msgs := ids.NewMessages()
	w := newWhatsApp(backend.Deps{
		Events: events, IDs: idStore, Messages: msgs, Files: ids.NewFiles(),
		Outbox: backend.NewOutbox(events, msgs), Session: session.New(dir, proto.WhatsApp),
	})
	w.now = func() time.Time { return base }
	t.Cleanup(w.Close)
	return w, rec
}

func TestNothingLinkedStartsLoggedOut(t *testing.T) {
	w, rec := fresh(t, t.TempDir())
	w.dial = func(int, *wastore.Device) error { t.Error("it tried to connect"); return nil }
	w.Start(context.Background())
	if accs := rec.accounts(); len(accs) != 1 || accs[0].State != proto.LoggedOut {
		t.Fatalf("accounts %+v", accs)
	}
}

func TestAKeptDeviceConnectsWithItsChatsReadBack(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(withAlt(text(alicePN, alicePN, "A1", -5, "hi"), aliceLID))
	ctx := context.Background()
	dev := h.w.st.container.NewDevice()
	jid := waTypes.JID{User: selfPN.User, Server: selfPN.Server, Device: 7}
	dev.ID = &jid
	dev.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := dev.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Session.SaveJSON(sessionFile, savedSession{Version: 1, Device: jid.String()}); err != nil {
		t.Fatal(err)
	}
	h.deps.IDs.Flush()
	h.w.Close()

	w, rec := fresh(t, h.dir)
	dialed := make(chan *wastore.Device, 1)
	w.dial = func(_ int, d *wastore.Device) error { dialed <- d; return nil }
	w.Start(ctx)
	select {
	case d := <-dialed:
		if d.ID == nil || *d.ID != jid {
			t.Fatalf("dialed %v", d.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the kept device didn't connect")
	}
	if accs := rec.accounts(); len(accs) != 1 || accs[0].State != proto.Connecting {
		t.Fatalf("accounts %+v", accs)
	}
	w.mu.Lock()
	n := len(w.chats)
	w.mu.Unlock()
	if n != 1 {
		t.Errorf("%d chats read back", n)
	}
}

func TestADeviceThePhoneUnlinkedSaysSoAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.event(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	h.w.Close()
	w, rec := fresh(t, h.dir)
	w.dial = func(int, *wastore.Device) error { t.Error("it tried to connect"); return nil }
	w.Start(context.Background())
	accs := rec.accounts()
	if len(accs) != 1 || accs[0].State != proto.Errored || !strings.Contains(accs[0].Error, "link it again") {
		t.Fatalf("accounts %+v", accs)
	}
}

func TestLinkingAgainNeverThrowsAwayADeviceThatStillWorks(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -1, "kept"))
	if err := h.deps.Session.SaveJSON(sessionFile, savedSession{Version: 1, Device: selfPN.String()}); err != nil {
		t.Fatal(err)
	}
	// Reconnecting: not ready, but the device is fine.
	h.w.mu.Lock()
	h.w.ready = false
	h.w.mu.Unlock()
	err := h.w.Link(context.Background(), "", 1)
	if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.BadRequest {
		t.Fatalf("got %v", err)
	}
	if h.chat(benLID.String()) == nil {
		t.Error("the kept chats went")
	}
	if len(h.rec.codes()) != 0 {
		t.Error("a code was shown")
	}
}

func TestThumbnailsThatStartAlikeAreDifferentFiles(t *testing.T) {
	h := newHarness(t)
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	header := make([]byte, 64)
	a := &media{Kind: proto.FileMedia, Thumb: append(append([]byte{}, header...), 'a')}
	b := &media{Kind: proto.FileMedia, Thumb: append(append([]byte{}, header...), 'b')}
	if h.w.mediaOf(a).Thumbnail.FileID == h.w.mediaOf(b).Thumbnail.FileID {
		t.Error("two thumbnails share a file")
	}
}
