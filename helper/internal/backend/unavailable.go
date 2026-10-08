// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"context"
	"io"

	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Unavailable stands in for a network this build can't speak: it stays
// logged out and answers every request with "unsupported".
type Unavailable struct {
	Net    proto.Network
	Events *Events
}

func (u *Unavailable) err() error {
	return proto.Err(proto.Unsupported, "This tuimeta-helper can't connect to "+u.Net.Title()+" yet.")
}

func (u *Unavailable) Network() proto.Network { return u.Net }
func (u *Unavailable) Start(context.Context) {
	u.Events.Account(u.Net, proto.LoggedOut, 0, "", "")
}
func (u *Unavailable) Close() {}
func (u *Unavailable) LoginCookies(context.Context, cookies.Set, browser.Identity) error {
	return u.err()
}
func (u *Unavailable) Logout(context.Context) error {
	u.Events.Account(u.Net, proto.LoggedOut, 0, "", "")
	return nil
}
func (u *Unavailable) LoadChats(context.Context, int) (bool, error) { return false, u.err() }
func (u *Unavailable) History(context.Context, ChatRef, history.Query) (history.Page, error) {
	return history.Page{}, u.err()
}
func (u *Unavailable) GetMessage(context.Context, MessageRef) (proto.Message, error) {
	return proto.Message{}, u.err()
}
func (u *Unavailable) Send(context.Context, *Outgoing) error              { return u.err() }
func (u *Unavailable) EditText(context.Context, MessageRef, string) error { return u.err() }
func (u *Unavailable) Delete(context.Context, MessageRef) error           { return u.err() }
func (u *Unavailable) React(context.Context, MessageRef, string) error    { return u.err() }
func (u *Unavailable) MarkRead(context.Context, MessageRef) error         { return u.err() }
func (u *Unavailable) SetTyping(context.Context, ChatRef, bool) error     { return u.err() }
func (u *Unavailable) Mute(context.Context, ChatRef, bool) error          { return u.err() }
func (u *Unavailable) Search(context.Context, string) ([]proto.SearchResult, error) {
	return nil, u.err()
}
func (u *Unavailable) OpenDM(context.Context, UserRef) (int64, error) { return 0, u.err() }
func (u *Unavailable) Fetch(context.Context, ids.FileRef, io.Writer) error {
	return u.err()
}
