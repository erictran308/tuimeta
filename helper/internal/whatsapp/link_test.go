// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// try is a link in progress, for awaitScan.
func newTry() *linkTry {
	_, cancel := context.WithCancel(context.Background())
	return &linkTry{done: make(chan struct{}), cancel: cancel}
}

func codesOf(items ...whatsmeow.QRChannelItem) <-chan whatsmeow.QRChannelItem {
	ch := make(chan whatsmeow.QRChannelItem, len(items))
	for _, it := range items {
		ch <- it
	}
	close(ch)
	return ch
}

func code(c string, d time.Duration) whatsmeow.QRChannelItem {
	return whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: c, Timeout: d}
}

func noPair(context.Context, string) (string, error) {
	return "", errors.New("a QR link asked for a pairing code")
}

func TestEachQRCodeIsShownWithWhenItStopsWorking(t *testing.T) {
	h := newHarness(t)
	err := h.w.awaitScan(context.Background(), newTry(), "", codesOf(code("2@one", time.Minute), code("2@two", 20*time.Second), whatsmeow.QRChannelSuccess), noPair)
	if err != nil {
		t.Fatal(err)
	}
	codes := h.rec.codes()
	if len(codes) != 2 || codes[0].QR != "2@one" || codes[1].QR != "2@two" || codes[0].Pairing != "" {
		t.Fatalf("codes %+v", codes)
	}
	if codes[0].Expires != base.Add(time.Minute).Unix() || codes[1].Expires != base.Add(20*time.Second).Unix() {
		t.Errorf("expiry %d %d", codes[0].Expires, codes[1].Expires)
	}
}

func TestAPhoneLinkAsksForOnePairingCodeOnceTheConnectionIsUp(t *testing.T) {
	h := newHarness(t)
	asked := 0
	pair := func(_ context.Context, phone string) (string, error) {
		asked++
		if phone != "447700900100" {
			t.Errorf("phone %q", phone)
		}
		return "ABCD-EFGH", nil
	}
	err := h.w.awaitScan(context.Background(), newTry(), "447700900100", codesOf(code("2@one", time.Minute), code("2@two", time.Minute), whatsmeow.QRChannelSuccess), pair)
	if err != nil || asked != 1 {
		t.Fatalf("err %v, asked %d", err, asked)
	}
	codes := h.rec.codes()
	if len(codes) != 1 || codes[0].Pairing != "ABCD-EFGH" || codes[0].QR != "" || codes[0].Expires != base.Add(PairCodeLife).Unix() {
		t.Fatalf("codes %+v", codes)
	}
}

func TestLinkingEndsWithASentenceForEachWayItCanFail(t *testing.T) {
	h := newHarness(t)
	for item, want := range map[whatsmeow.QRChannelItem]proto.Code{
		whatsmeow.QRChannelTimeout:                      proto.Timeout,
		whatsmeow.QRChannelClientOutdated:               proto.Unsupported,
		whatsmeow.QRChannelScannedWithoutMultidevice:    proto.Unsupported,
		{Event: whatsmeow.QRChannelEventPasskeyRequest}: proto.Unsupported,
		whatsmeow.QRChannelErrUnexpectedEvent:           proto.NetworkError,
	} {
		err := h.w.awaitScan(context.Background(), newTry(), "", codesOf(item), noPair)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != want {
			t.Errorf("%s: %v", item.Event, err)
		}
	}
	refused := func(context.Context, string) (string, error) { return "", whatsmeow.ErrPhoneNumberIsNotInternational }
	err := h.w.awaitScan(context.Background(), newTry(), "0123456789", codesOf(code("2@x", time.Minute)), refused)
	if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.BadRequest {
		t.Errorf("a refused number: %v", err)
	}
}

func TestCancellingALinkEndsItAtOnce(t *testing.T) {
	h := newHarness(t)
	try := newTry()
	codes := make(chan whatsmeow.QRChannelItem)
	done := make(chan error, 1)
	go func() { done <- h.w.awaitScan(context.Background(), try, "", codes, noPair) }()
	codes <- code("2@one", time.Minute)
	try.end(errCancelled, false)
	select {
	case err := <-done:
		if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.Cancelled {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the link didn't end")
	}
}

func TestCancelLinkWithNothingWaitingDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.w.CancelLink(0)
	if len(h.rec.all()) != 0 {
		t.Errorf("events %+v", h.rec.all())
	}
}
