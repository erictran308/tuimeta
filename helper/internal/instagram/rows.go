// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// Rows of the timeline that aren't media_or_ad can still hold posts:
// Instagram's "Suggested for you" (explore_story) rows, and units of
// several suggested reels. Their shape isn't known for sure, so a row is
// searched for media dicts a few levels down, rather than read as one
// shape that would drop them all the day it changes.

import (
	"bytes"
	"encoding/json"
	"slices"
)

const (
	// rowDepth is how deep in a row's value a post is looked for: the
	// value is 0, what an object or a list holds one more.
	rowDepth = 3
	// rowPosts is the most posts one row gives.
	rowPosts = 20
)

// preferred are the keys a post is looked for under first, in this order;
// the first that holds one is the row's post, and other keys aren't
// searched.
var preferred = []string{"media_or_ad", "media", "clips_item", "clip", "item"}

// postless are rows known to hold no post (people to follow, a tray of
// stories, tags, the "you're all caught up" mark): not searched.
var postless = []string{"end_of_feed_demarcator", "stories_netego", "suggested_hashtags", "suggested_users"}

// rowMedia is the media dicts in a row's value, in order, at most
// rowPosts.
func rowMedia(v json.RawMessage) []json.RawMessage {
	var found []json.RawMessage
	searchRow(v, 0, &found)
	return found
}

func searchRow(v json.RawMessage, depth int, found *[]json.RawMessage) {
	if depth > rowDepth || len(*found) >= rowPosts {
		return
	}
	v = bytes.TrimSpace(v)
	if len(v) == 0 {
		return
	}
	switch v[0] {
	case '[':
		var list []json.RawMessage
		if json.Unmarshal(v, &list) != nil {
			return
		}
		for _, e := range list {
			searchRow(e, depth+1, found)
		}
	case '{':
		var obj map[string]json.RawMessage
		if json.Unmarshal(v, &obj) != nil {
			return
		}
		if looksLikeMedia(obj) {
			// A carousel's parts are inside it: one post, not several.
			*found = append(*found, v)
			return
		}
		for _, k := range preferred {
			before := len(*found)
			if c, ok := obj[k]; ok {
				searchRow(c, depth+1, found)
			}
			if len(*found) > before {
				return
			}
		}
		for _, k := range sortedKeys(obj) {
			if !slices.Contains(preferred, k) {
				searchRow(obj[k], depth+1, found)
			}
		}
	}
}

// looksLikeMedia reports whether obj is a media dict: a media_type, a
// shortcode, and pictures or a video.
func looksLikeMedia(obj map[string]json.RawMessage) bool {
	if !present(obj["media_type"]) || !present(obj["code"]) {
		return false
	}
	for _, k := range []string{"image_versions2", "carousel_media", "video_versions", "video_dash_manifest"} {
		if present(obj[k]) {
			return true
		}
	}
	return false
}

func sortedKeys(obj map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// rowKey is what a row is called: its key, or of several, the first (in
// order) that holds an object or a list.
func rowKey(item map[string]json.RawMessage) string {
	keys := sortedKeys(item)
	for _, k := range keys {
		if v := bytes.TrimSpace(item[k]); len(v) > 0 && (v[0] == '{' || v[0] == '[') {
			return k
		}
	}
	if len(keys) > 0 {
		return keys[0]
	}
	return ""
}

// schemaName reports whether a row's key reads as one of Instagram's own
// field names (lower case, digits and _, at most 40), which the log may
// say; anything else is counted as "other".
func schemaName(k string) bool {
	if k == "" || len(k) > 40 || k[0] < 'a' || k[0] > 'z' {
		return false
	}
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// rowCount is how many rows of a kind a page had, and how many held posts.
type rowCount struct {
	rows, withPosts int
}
