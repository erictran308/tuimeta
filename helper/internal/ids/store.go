// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ids hands out the numbers tuimeta knows chats, people, messages
// and files by, so it never sees a network's own ids.
package ids

import (
	"cmp"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/fsutil"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

type kind uint8

const (
	chatKind kind = iota
	userKind
)

type key struct {
	kind    kind
	network proto.Network
	netID   string
}

// Store gives chats and people ids that are unique across networks and
// stable across runs: the mapping from (network, the network's own id) is
// kept in a file. Chats and people share one counter, so no chat has a
// person's id, and an id is never given out twice, even after Forget.
type Store struct {
	mu    sync.Mutex
	path  string
	next  int64
	byKey map[key]int64
	byID  map[int64]key
	dirty bool
}

type entry struct {
	Network proto.Network `json:"network"`
	ID      string        `json:"id"`
	N       int64         `json:"n"`
}

type file struct {
	Version int     `json:"version"`
	Next    int64   `json:"next"`
	Chats   []entry `json:"chats"`
	Users   []entry `json:"users"`
}

// Open reads the mapping kept at path, or starts one. A file that can't be
// read is moved aside, and counting then starts far above anything it can
// have held, so an id tuimeta remembers is never given to something else.
func Open(path string) (*Store, error) {
	s := &Store{path: path, next: 1, byKey: map[key]int64{}, byID: map[int64]key{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	} else if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil || f.Version != 1 || f.Next < 1 {
		hlog.Warn("ids file unreadable; starting over", hlog.Kind(err))
		_ = os.Rename(path, path+".bad")
		s.next = time.Now().UnixMilli()
		s.dirty = true
		return s, nil
	}
	s.next = f.Next
	load := func(k kind, entries []entry) {
		for _, e := range entries {
			kk := key{k, e.Network, e.ID}
			if e.N < 1 || e.N >= s.next || !e.Network.Valid() || e.ID == "" {
				continue
			}
			if _, dup := s.byID[e.N]; dup {
				continue
			}
			if _, dup := s.byKey[kk]; dup {
				continue
			}
			s.byKey[kk] = e.N
			s.byID[e.N] = kk
		}
	}
	load(chatKind, f.Chats)
	load(userKind, f.Users)
	return s, nil
}

func (s *Store) get(k key) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.byKey[k]; ok {
		return n
	}
	n := s.next
	s.next++
	s.byKey[k] = n
	s.byID[n] = k
	s.dirty = true
	return n
}

// Chat is the id of the network's chat netID, made if it's new.
func (s *Store) Chat(n proto.Network, netID string) int64 { return s.get(key{chatKind, n, netID}) }

// User is the id of the network's person netID, made if it's new.
func (s *Store) User(n proto.Network, netID string) int64 { return s.get(key{userKind, n, netID}) }

func (s *Store) lookup(k kind, id int64) (proto.Network, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kk, ok := s.byID[id]
	if !ok || kk.kind != k {
		return "", "", false
	}
	return kk.network, kk.netID, true
}

// LookupChat is the network and network-side id of chat id.
func (s *Store) LookupChat(id int64) (proto.Network, string, bool) { return s.lookup(chatKind, id) }

// LookupUser is the network and network-side id of person id.
func (s *Store) LookupUser(id int64) (proto.Network, string, bool) { return s.lookup(userKind, id) }

// ChatsOf lists the ids of the network's chats.
func (s *Store) ChatsOf(n proto.Network) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for k, id := range s.byKey {
		if k.kind == chatKind && k.network == n {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// Forget drops everything known about the network's chats and people (on
// logout). Their ids aren't given out again.
func (s *Store) Forget(n proto.Network) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, id := range s.byKey {
		if k.network == n {
			delete(s.byKey, k)
			delete(s.byID, id)
			s.dirty = true
		}
	}
}

// Flush writes the mapping if it changed. The server calls it before each
// line it writes, so no id reaches tuimeta before it's kept.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	f := file{Version: 1, Next: s.next, Chats: []entry{}, Users: []entry{}}
	for k, n := range s.byKey {
		e := entry{Network: k.network, ID: k.netID, N: n}
		if k.kind == chatKind {
			f.Chats = append(f.Chats, e)
		} else {
			f.Users = append(f.Users, e)
		}
	}
	byN := func(a, b entry) int { return cmp.Compare(a.N, b.N) }
	slices.SortFunc(f.Chats, byN)
	slices.SortFunc(f.Users, byN)
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	// Synced, or a power cut could hand out the lost ids again for other
	// chats. The server writes lines in batches, so this runs once a batch.
	if err := fsutil.WriteAtomic(s.path, data, true); err != nil {
		return err
	}
	s.dirty = false
	return nil
}
