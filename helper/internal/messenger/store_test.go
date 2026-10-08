// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestTheStoreIsPrivateAndKeepsADeviceAndMessages(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), storeFile)
	s, err := openStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	dev, isNew, err := s.device(ctx, "")
	if err != nil || !isNew || dev == nil {
		t.Fatalf("device: %v %v %v", dev, isNew, err)
	}
	if err := s.put(ctx, storedMessage{Chat: "1@msgr", Sender: "2@msgr", ID: "A", TS: time.UnixMilli(1000), App: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.put(ctx, storedMessage{Chat: "1@msgr", Sender: "3@msgr", ID: "B", TS: time.UnixMilli(500), FromMe: true, App: []byte{2}}); err != nil {
		t.Fatal(err)
	}
	all, err := s.all(ctx)
	if err != nil || len(all) != 2 || all[0].ID != "B" || !all[0].FromMe || all[1].ID != "A" {
		t.Fatalf("all = %+v, %v", all, err)
	}
	if err := s.remove(ctx, "1@msgr", "3@msgr", "B"); err != nil {
		t.Fatal(err)
	}
	if err := s.prune(ctx); err != nil {
		t.Fatal(err)
	}
	all, _ = s.all(ctx)
	if len(all) != 1 {
		t.Fatalf("after remove: %+v", all)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			info, err := os.Stat(path + suffix)
			if err != nil {
				continue // WAL files go away on a clean close
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v", suffix, info.Mode().Perm())
			}
		}
	}
	// Opened again, what was kept is there.
	s2, err := openStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if all, _ := s2.all(ctx); len(all) != 1 || all[0].ID != "A" {
		t.Fatalf("reopened: %+v", all)
	}
}

func TestPruneChatEnforcesThePerChatCapOnWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), storeFile)
	s, err := openStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Two chats, each given more than the cap; a put is always followed by
	// the per-chat prune, as receiveWA does.
	const over = MaxStoredPerChat + 50
	for _, chat := range []string{"1@msgr", "2@msgr"} {
		for i := range over {
			m := storedMessage{Chat: chat, Sender: "9@msgr", ID: fmt.Sprintf("m%d", i), TS: time.UnixMilli(int64(i + 1)), App: []byte{byte(i)}}
			if err := s.put(ctx, m); err != nil {
				t.Fatal(err)
			}
			if err := s.pruneChat(ctx, chat); err != nil {
				t.Fatal(err)
			}
		}
	}

	all, err := s.all(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2*MaxStoredPerChat {
		t.Fatalf("kept %d, want %d", len(all), 2*MaxStoredPerChat)
	}
	// The newest survive: the oldest kept id in a chat is the (over-cap)th.
	per := map[string]int{}
	oldestKept := fmt.Sprintf("m%d", over-MaxStoredPerChat)
	var sawOldest bool
	for _, m := range all {
		per[m.Chat]++
		if m.Chat == "1@msgr" && m.ID == oldestKept {
			sawOldest = true
		}
		if m.ID == "m0" {
			t.Fatalf("the oldest message wasn't pruned: %+v", m)
		}
	}
	if per["1@msgr"] != MaxStoredPerChat || per["2@msgr"] != MaxStoredPerChat {
		t.Fatalf("per-chat counts %v", per)
	}
	if !sawOldest {
		t.Fatalf("expected %s to be the oldest surviving message", oldestKept)
	}
}
