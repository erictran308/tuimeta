// SPDX-License-Identifier: AGPL-3.0-or-later

package ids

import (
	"math"
	"sync"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// FileRef is what a file id stands for: enough for its backend to fetch it.
type FileRef struct {
	ID      int32
	Network proto.Network
	// Key names the content, stable across runs: two refs with one key are
	// the same bytes (an attachment's id, a picture's URL path, not a URL
	// with an expiring signature). Downloads are kept on disk under it.
	Key string
	// Size is the size in bytes if known, else 0.
	Size int64
	// Mime and Name, if known, give the saved file its extension.
	Mime, Name string
	// Source is the backend's own: a URL, a media key, whatever it needs.
	Source any
}

// Files gives everything downloadable a file id for the run.
type Files struct {
	mu    sync.Mutex
	next  int32
	byKey map[fileKey]int32
	byID  map[int32]FileRef
}

type fileKey struct {
	network proto.Network
	key     string
}

func NewFiles() *Files {
	return &Files{next: 1, byKey: map[fileKey]int32{}, byID: map[int32]FileRef{}}
}

// Register gives ref an id, or returns the id its key already has, updating
// what's known about it (a fresh URL, a size). The name and type it was
// first registered with stay: another message carrying the same file can't
// rename what's saved, or what it opens as.
func (f *Files) Register(ref FileRef) int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fileKey{ref.Network, ref.Key}
	if id, ok := f.byKey[k]; ok {
		old := f.byID[id]
		ref.ID = id
		if ref.Size == 0 {
			ref.Size = old.Size
		}
		if old.Mime != "" {
			ref.Mime = old.Mime
		}
		if old.Name != "" {
			ref.Name = old.Name
		}
		if ref.Source == nil {
			ref.Source = old.Source
		}
		f.byID[id] = ref
		return id
	}
	if f.next == math.MaxInt32 {
		return 0
	}
	ref.ID = f.next
	f.next++
	f.byKey[k] = ref.ID
	f.byID[ref.ID] = ref
	return ref.ID
}

// Get is what file id stands for.
func (f *Files) Get(id int32) (FileRef, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref, ok := f.byID[id]
	return ref, ok
}

// Forget drops the id of the network's file with key, once what it came with
// is gone (its message deleted): Get no longer finds it, and the key
// registered again gets a new id. It returns the id it dropped.
func (f *Files) Forget(n proto.Network, key string) (int32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fileKey{n, key}
	id, ok := f.byKey[k]
	if ok {
		delete(f.byKey, k)
		delete(f.byID, id)
	}
	return id, ok
}

// ForgetNetwork drops the ids of all the network's files (logout), so an id
// from the account before can't be fetched with the next one's session.
func (f *Files) ForgetNetwork(n proto.Network) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, id := range f.byKey {
		if k.network == n {
			delete(f.byKey, k)
			delete(f.byID, id)
		}
	}
}
