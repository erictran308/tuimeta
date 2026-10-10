// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"bytes"
	"context"
	"os"
	"testing"

	"go.mau.fi/whatsmeow/proto/waArmadilloApplication"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// What a view-once photo or video could be fetched with, made up.
var (
	viewOnceKey  = []byte("VIEW-ONCE-MEDIA-KEY")
	viewOncePath = "/v/view-once-direct-path"
)

func viewOnceTransport() *waMediaTransport.WAMediaTransport {
	return &waMediaTransport.WAMediaTransport{
		Integral: &waMediaTransport.WAMediaTransport_Integral{
			FileSHA256: []byte("sha"), FileEncSHA256: []byte("enc"), MediaKey: viewOnceKey, DirectPath: gproto.String(viewOncePath),
		},
		Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{Mimetype: gproto.String("image/jpeg")},
	}
}

// viewOncePhoto is a consumer view-once photo, as WhatsApp-style clients send it.
func viewOncePhoto(t *testing.T) *waConsumerApplication.ConsumerApplication_Content {
	t.Helper()
	img := &waConsumerApplication.ConsumerApplication_ImageMessage{}
	if err := img.Set(&waMediaTransport.ImageTransport{Integral: &waMediaTransport.ImageTransport_Integral{Transport: viewOnceTransport()}}); err != nil {
		t.Fatal(err)
	}
	return &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_ViewOnceMessage{
		ViewOnceMessage: &waConsumerApplication.ConsumerApplication_ViewOnceMessage{
			ViewOnceContent: &waConsumerApplication.ConsumerApplication_ViewOnceMessage_ImageMessage{ImageMessage: img},
		},
	}}
}

// ravenVideo is Messenger's own view-once video, its payload as whatsmeow
// delivers it.
func ravenVideo(t *testing.T, id string, min int) *events.FBMessage {
	t.Helper()
	rm := &waArmadilloApplication.Armadillo_Content_RavenMessage{}
	if err := rm.SetVideoMessage(&waMediaTransport.VideoTransport{Integral: &waMediaTransport.VideoTransport_Integral{Transport: viewOnceTransport()}}); err != nil {
		t.Fatal(err)
	}
	arm := armadilloMsg(&waArmadilloApplication.Armadillo_Content{Content: &waArmadilloApplication.Armadillo_Content_RavenMessageMsgr{RavenMessageMsgr: rm}})
	sub := &waMsgApplication.MessageApplication_SubProtocolPayload_Armadillo{}
	if err := sub.Set(arm); err != nil {
		t.Fatal(err)
	}
	evt := waMsg(jid(aliceID), jid(aliceID), id, min, waText("x"), nil)
	evt.Message = arm
	evt.FBApplication = &waMsgApplication.MessageApplication{
		Payload: &waMsgApplication.MessageApplication_Payload{Content: &waMsgApplication.MessageApplication_Payload_SubProtocol{
			SubProtocol: &waMsgApplication.MessageApplication_SubProtocolPayload{SubProtocol: sub},
		}},
		Metadata: &waMsgApplication.MessageApplication_Metadata{},
	}
	return evt
}

// keepsViewOnceMedia reports whether the store's rows, or its files, still
// have what the view-once media could be fetched with.
func (h *harness) keepsViewOnceMedia(files bool) bool {
	h.t.Helper()
	for _, r := range h.rows() {
		if bytes.Contains(r.App, viewOnceKey) || bytes.Contains(r.App, []byte(viewOncePath)) {
			return true
		}
	}
	if !files {
		return false
	}
	path, _ := h.deps.Session.Path(storeFile)
	for _, suffix := range []string{"", "-wal"} {
		if data, err := os.ReadFile(path + suffix); err == nil && (bytes.Contains(data, viewOnceKey) || bytes.Contains(data, []byte(viewOncePath))) {
			return true
		}
	}
	return false
}

// viewOnce checks the two view-once messages were read back as such.
func (h *harness) viewOnceReadBack() {
	h.t.Helper()
	all := h.chat(aliceID).log.All()
	if len(all) != 2 || all[0].Media == nil || all[1].Media == nil {
		h.t.Fatalf("read back %+v", all)
	}
	if !all[0].Media.ViewOnce || all[0].Media.Kind != proto.Photo || !all[1].Media.ViewOnce || all[1].Media.Kind != proto.Video || all[1].Media.FileID != 0 {
		h.t.Errorf("read back as %+v and %+v", all[0].Media, all[1].Media)
	}
}

func TestViewOnceMediaIsKeptWithNothingToFetchItWith(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "V1", -2, viewOncePhoto(t), nil))
	h.m.receiveWA(ravenVideo(t, "V2", -1))
	if len(h.rows()) != 2 || h.keepsViewOnceMedia(false) {
		t.Errorf("the store keeps a view-once message's key or path")
	}
	h.restart(gen)
	h.viewOnceReadBack()
}

func TestAnOlderStoresViewOnceMediaIsTakenOutAtTheNextStart(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	// As an older build kept them: the whole payload.
	for _, evt := range []*events.FBMessage{waMsg(jid(aliceID), jid(aliceID), "V1", -2, viewOncePhoto(t), nil), ravenVideo(t, "V2", -1)} {
		app, _ := gproto.Marshal(evt.FBApplication)
		if err := h.kept().put(context.Background(), storedMessage{Chat: evt.Info.Chat.String(), Sender: evt.Info.Sender.String(), ID: evt.Info.ID, TS: evt.Info.Timestamp, App: app}); err != nil {
			t.Fatal(err)
		}
	}
	if !h.keepsViewOnceMedia(false) {
		t.Fatal("the test's rows don't have the media's key")
	}
	h.restart(gen)
	if h.keepsViewOnceMedia(true) {
		t.Error("the older rows still have a view-once message's key or path")
	}
	h.viewOnceReadBack()
}

func TestAQuotesCopyOfTheMessageIsKeptOnlyAsText(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	photo := &waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage{}
	if err := photo.Set(wrapConsumer(waPhoto(t, []byte("sha"), viewOnceKey, viewOncePath, nil))); err != nil {
		t.Fatal(err)
	}
	quote := quoting(t, "P0", jid(benID).String(), "")
	quote.QuotedMessage.Payload.GetSubProtocol().SubProtocol = photo
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "A1", -2, waText("nice photo"), quote))
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "A2", -1, waText("I said"), quoting(t, "T0", jid(aliceID).String(), "something kind")))
	if h.keepsViewOnceMedia(false) {
		t.Error("a quoted photo's key was kept")
	}
	h.restart(gen)
	all := h.chat(aliceID).log.All()
	if len(all) != 2 || all[1].ReplyTo == nil || all[1].ReplyTo.Text != "something kind" {
		t.Errorf("read back %+v", all)
	}
}
