// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// Instagram's web feed often sends a video only as DASH: a manifest
// (video_dash_manifest, MPD XML in a string) naming its picture and its
// sound as files of their own. Each representation is a whole file at its
// BaseURL, which is what yt-dlp downloads for Instagram's DASH formats too,
// so the picture and the sound are each one download; the player plays
// them together (PROTOCOL.md, Media's audio).

import (
	"encoding/xml"
	"strconv"
	"strings"
)

// maxManifest is the largest manifest read. Instagram's are a few KiB;
// one bigger is no video's, and isn't parsed.
const maxManifest = 256 << 10

// mpd is the part of a DASH manifest a video needs. Tags match whatever
// namespace the manifest declares (urn:mpeg:dash:schema:mpd:2011).
type mpd struct {
	Duration string `xml:"mediaPresentationDuration,attr"`
	Periods  []struct {
		Sets []mpdSet `xml:"AdaptationSet"`
	} `xml:"Period"`
}

type mpdSet struct {
	ContentType string   `xml:"contentType,attr"`
	MimeType    string   `xml:"mimeType,attr"`
	Reps        []mpdRep `xml:"Representation"`
}

type mpdRep struct {
	MimeType  string `xml:"mimeType,attr"`
	Width     string `xml:"width,attr"`
	Height    string `xml:"height,attr"`
	Bandwidth string `xml:"bandwidth,attr"`
	BaseURL   string `xml:"BaseURL"`
}

// dashStream is a representation picked: a whole file on the CDN.
type dashStream struct {
	url           string
	width, height int
	bandwidth     int64
}

// kind is "video", "audio" or something else: the representation's own
// mimeType, else its set's, else the set's contentType.
func (r mpdRep) kind(set mpdSet) string {
	for _, m := range []string{r.MimeType, set.MimeType} {
		if m = strings.TrimSpace(m); m != "" {
			top, _, _ := strings.Cut(strings.ToLower(m), "/")
			return top
		}
	}
	return strings.ToLower(strings.TrimSpace(set.ContentType))
}

// dashPick reads a manifest: picture is the video representation whose
// width is nearest videoWidth (the narrower of two as near), sound the
// audio one with the highest bandwidth, each only with a BaseURL on
// Instagram's CDN; seconds is the manifest's length, 0 if it doesn't say.
// ok is false for a manifest too big or that doesn't read, or with no
// picture on the CDN; sound.url is "" for a video without sound there.
func dashPick(manifest string) (picture, sound dashStream, seconds float64, ok bool) {
	if manifest == "" || len(manifest) > maxManifest {
		return dashStream{}, dashStream{}, 0, false
	}
	var doc mpd
	if xml.Unmarshal([]byte(manifest), &doc) != nil {
		return dashStream{}, dashStream{}, 0, false
	}
	dist := func(s dashStream) int {
		if s.width > videoWidth {
			return s.width - videoWidth
		}
		return videoWidth - s.width
	}
	var havePicture, haveSound bool
	for _, p := range doc.Periods {
		for _, set := range p.Sets {
			for _, r := range set.Reps {
				s := dashStream{
					url:       strings.TrimSpace(r.BaseURL),
					width:     number(r.Width),
					height:    number(r.Height),
					bandwidth: int64(number(r.Bandwidth)),
				}
				if !onCDNURL(s.url) {
					continue
				}
				switch r.kind(set) {
				case "video":
					if !havePicture || dist(s) < dist(picture) || (dist(s) == dist(picture) && s.width < picture.width) {
						picture, havePicture = s, true
					}
				case "audio":
					if !haveSound || s.bandwidth > sound.bandwidth {
						sound, haveSound = s, true
					}
				}
			}
		}
	}
	return picture, sound, isoSeconds(doc.Duration), havePicture
}

// number is a non-negative whole number attribute; 0 if it isn't one.
func number(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// isoSeconds reads an ISO 8601 length of hours, minutes and seconds, as
// manifests write it ("PT0H0M14.700S"); 0 for anything else.
func isoSeconds(d string) float64 {
	rest, ok := strings.CutPrefix(strings.TrimSpace(d), "PT")
	if !ok {
		return 0
	}
	total := 0.0
	for rest != "" {
		i := strings.IndexAny(rest, "HMS")
		if i <= 0 {
			return 0
		}
		v, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil || v < 0 || v > 1e7 {
			return 0
		}
		switch rest[i] {
		case 'H':
			total += v * 3600
		case 'M':
			total += v * 60
		case 'S':
			total += v
		}
		rest = rest[i+1:]
	}
	return total
}
