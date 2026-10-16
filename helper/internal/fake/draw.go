// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"math"
	"strings"
)

// The fake's pictures are drawn here, the same bytes every run.

type artKind int

const (
	avatarArt  artKind = iota // a coloured circle on transparency
	blocksArt                 // four coloured blocks, a group's photo
	photoArt                  // sky, sun and hills
	stickerArt                // a smiling face on transparency
	videoArt                  // a dark still with a play triangle
	cardArt                   // a link preview's banner
)

type art struct {
	kind artKind
	w, h int
	seed int
}

var palette = []color.RGBA{
	{0xe0, 0x6c, 0x75, 0xff}, // red
	{0x61, 0xaf, 0xef, 0xff}, // blue
	{0x98, 0xc3, 0x79, 0xff}, // green
	{0xc6, 0x78, 0xdd, 0xff}, // purple
	{0xe5, 0xc0, 0x7b, 0xff}, // yellow
	{0xd1, 0x9a, 0x66, 0xff}, // orange
	{0x56, 0xb6, 0xc2, 0xff}, // cyan
	{0xbe, 0x50, 0x46, 0xff}, // brick
}

func pick(seed int) color.RGBA { return palette[((seed%len(palette))+len(palette))%len(palette)] }

func mix(a, b color.RGBA, t float64) color.RGBA {
	t = math.Max(0, math.Min(1, t))
	f := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
	return color.RGBA{f(a.R, b.R), f(a.G, b.G), f(a.B, b.B), f(a.A, b.A)}
}

// over paints c over the pixel with coverage a (0..1), for smooth edges.
func over(img *image.RGBA, x, y int, c color.RGBA, a float64) {
	if a <= 0 || !image.Pt(x, y).In(img.Rect) {
		return
	}
	a = math.Min(a, 1) * float64(c.A) / 255
	dst := img.RGBAAt(x, y)
	blend := func(d, s uint8) uint8 { return uint8(math.Round(float64(d)*(1-a) + float64(s)*a)) }
	img.SetRGBA(x, y, color.RGBA{
		blend(dst.R, c.R), blend(dst.G, c.G), blend(dst.B, c.B),
		uint8(math.Round(float64(dst.A)*(1-a) + 255*a)),
	})
}

// disc paints a filled circle with a soft one-pixel edge.
func disc(img *image.RGBA, cx, cy, r float64, c color.RGBA) {
	b := img.Rect
	for y := max(b.Min.Y, int(cy-r-1)); y < min(b.Max.Y, int(cy+r+2)); y++ {
		for x := max(b.Min.X, int(cx-r-1)); x < min(b.Max.X, int(cx+r+2)); x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			over(img, x, y, c, r-d+0.5)
		}
	}
}

func (a art) image() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, a.w, a.h))
	w, h := float64(a.w), float64(a.h)
	switch a.kind {
	case avatarArt:
		c := pick(a.seed)
		disc(img, w/2, h/2, w/2-1, mix(c, color.RGBA{0, 0, 0, 0xff}, 0.25))
		disc(img, w/2, h/2, w/2-6, c)
		disc(img, w*0.4, h*0.38, w*0.12, mix(c, color.RGBA{0xff, 0xff, 0xff, 0xff}, 0.35))
	case blocksArt:
		for y := range a.h {
			for x := range a.w {
				q := 0
				if x >= a.w/2 {
					q++
				}
				if y >= a.h/2 {
					q += 2
				}
				img.SetRGBA(x, y, pick(a.seed+q*3))
			}
		}
	case photoArt, cardArt:
		top, bottom := pick(a.seed+1), pick(a.seed+4)
		top = mix(top, color.RGBA{0x20, 0x24, 0x40, 0xff}, 0.3)
		for y := range a.h {
			c := mix(top, bottom, float64(y)/h)
			for x := range a.w {
				img.SetRGBA(x, y, c)
			}
		}
		disc(img, w*(0.25+0.5*float64(a.seed%3)/2), h*0.35, math.Min(w, h)*0.12, color.RGBA{0xff, 0xe9, 0xa8, 0xff})
		hill := mix(pick(a.seed+2), color.RGBA{0x10, 0x14, 0x18, 0xff}, 0.45)
		for x := range a.w {
			fx := float64(x) / w
			ridge := h*0.62 - h*0.16*math.Sin(fx*math.Pi*float64(2+a.seed%3)+float64(a.seed))
			for y := int(ridge); y < a.h; y++ {
				over(img, x, y, hill, 1)
			}
			over(img, x, int(ridge)-1, hill, ridge-math.Floor(ridge))
		}
		if a.kind == cardArt {
			band := color.RGBA{0xff, 0xff, 0xff, 0x40}
			for y := a.h * 3 / 4; y < a.h*3/4+a.h/16; y++ {
				for x := range a.w {
					over(img, x, y, band, 1)
				}
			}
		}
	case stickerArt:
		yellow := color.RGBA{0xff, 0xcc, 0x33, 0xff}
		dark := color.RGBA{0x3a, 0x2a, 0x10, 0xff}
		disc(img, w/2, h/2, w/2-4, mix(yellow, dark, 0.2))
		disc(img, w/2, h/2, w/2-10, yellow)
		disc(img, w*0.36, h*0.4, w*0.06, dark)
		disc(img, w*0.64, h*0.4, w*0.06, dark)
		for t := 0.15; t <= 0.85; t += 0.004 {
			x := w/2 + math.Cos(t*math.Pi)*w*0.22
			y := h*0.55 + math.Sin(t*math.Pi)*h*0.16
			disc(img, x, y, w*0.025, dark)
		}
	case videoArt:
		a2 := pick(a.seed)
		for y := range a.h {
			c := mix(color.RGBA{0x18, 0x1a, 0x22, 0xff}, mix(a2, color.RGBA{0, 0, 0, 0xff}, 0.5), float64(y)/h)
			for x := range a.w {
				img.SetRGBA(x, y, c)
			}
		}
		disc(img, w/2, h/2, math.Min(w, h)*0.18, color.RGBA{0xff, 0xff, 0xff, 0x50})
		s := math.Min(w, h) * 0.1
		for y := range a.h {
			for x := range a.w {
				dx, dy := float64(x)-(w/2-s*0.6), math.Abs(float64(y)-h/2)
				if dx >= 0 && dx <= s*1.6 && dy <= (s*1.6-dx)*0.62 {
					over(img, x, y, color.RGBA{0xff, 0xff, 0xff, 0xff}, 1)
				}
			}
		}
	}
	return img
}

func (a art) png() []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, a.image()); err != nil {
		panic("fake: png.Encode failed")
	}
	return b.Bytes()
}

// clip is a fake video: a ball crossing a dusk sky, as an animated GIF of
// clipFrames frames that loops, the same bytes every run.
type clip struct {
	w, h, seed int
}

const (
	clipFrames = 20
	clipDelay  = 10 // hundredths of a second a frame shows: 2 s in all
)

// frame is frame i, in RGBA.
func (c clip) frame(i int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, c.w, c.h))
	w, h := float64(c.w), float64(c.h)
	top := mix(pick(c.seed+3), color.RGBA{0x10, 0x12, 0x30, 0xff}, 0.5)
	bottom := pick(c.seed + 5)
	for y := range c.h {
		row := mix(top, bottom, float64(y)/h)
		for x := range c.w {
			img.SetRGBA(x, y, row)
		}
	}
	t := float64(i) / clipFrames
	x := w*0.15 + w*0.7*t
	y := h*0.7 - h*0.45*math.Sin(t*math.Pi)
	disc(img, x, y, math.Min(w, h)*0.09, color.RGBA{0xff, 0xf1, 0xc4, 0xff})
	ground := mix(pick(c.seed+2), color.RGBA{0, 0, 0, 0xff}, 0.55)
	for y := int(h * 0.82); y < c.h; y++ {
		for x := range c.w {
			img.SetRGBA(x, y, ground)
		}
	}
	return img
}

// gif is the whole clip. Each frame takes the colours of a small palette
// made from the scene's own, so the file stays small.
func (c clip) gif() []byte {
	pal := c.palette()
	anim := &gif.GIF{LoopCount: 0}
	for i := range clipFrames {
		src := c.frame(i)
		dst := image.NewPaletted(src.Rect, pal)
		draw.Draw(dst, src.Rect, src, image.Point{}, draw.Src)
		anim.Image = append(anim.Image, dst)
		anim.Delay = append(anim.Delay, clipDelay)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, anim); err != nil {
		panic("fake: gif.EncodeAll failed")
	}
	return b.Bytes()
}

// still is the clip's first frame, as a PNG: its cover.
func (c clip) still() []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, c.frame(0)); err != nil {
		panic("fake: png.Encode failed")
	}
	return b.Bytes()
}

// palette is the sky's gradient in steps, the ball and the ground.
func (c clip) palette() color.Palette {
	top := mix(pick(c.seed+3), color.RGBA{0x10, 0x12, 0x30, 0xff}, 0.5)
	bottom := pick(c.seed + 5)
	pal := color.Palette{}
	for i := range 48 {
		pal = append(pal, mix(top, bottom, float64(i)/47))
	}
	ball := color.RGBA{0xff, 0xf1, 0xc4, 0xff}
	for i := range 8 {
		pal = append(pal, mix(top, ball, float64(i+1)/8), mix(bottom, ball, float64(i+1)/8))
	}
	return append(pal, ball, mix(pick(c.seed+2), color.RGBA{0, 0, 0, 0xff}, 0.55))
}

// Tone is the fake's sound: a 440 Hz sine about 24 dB below full scale, so
// it's quiet, in 16-bit mono at toneRate, faded in and out over toneFade so
// it doesn't click.
const (
	toneRate = 22050
	toneHz   = 440
	toneFade = 0.05 // seconds
)

var toneLevel = 32767 * math.Pow(10, -24.0/20)

// tone is seconds of the tone as a WAV file.
func tone(seconds int) []byte {
	n := toneRate * seconds
	samples := make([]int16, n)
	fade := toneFade * toneRate
	for i := range samples {
		gain := math.Min(1, math.Min(float64(i), float64(n-1-i))/fade)
		samples[i] = int16(math.Round(toneLevel * gain * math.Sin(2*math.Pi*toneHz*float64(i)/toneRate)))
	}
	var b bytes.Buffer
	data := uint32(2 * n)
	w := func(v any) {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			panic("fake: binary.Write failed")
		}
	}
	b.WriteString("RIFF")
	w(36 + data)
	b.WriteString("WAVEfmt ")
	w(uint32(16))           // the fmt chunk's size
	w(uint16(1))            // PCM
	w(uint16(1))            // one channel
	w(uint32(toneRate))     // samples a second
	w(uint32(toneRate * 2)) // bytes a second
	w(uint16(2))            // bytes a frame
	w(uint16(16))           // bits a sample
	b.WriteString("data")
	w(data)
	w(samples)
	return b.Bytes()
}

// pdf is a one-page PDF saying title (ASCII), with a correct xref table.
func pdf(title string) []byte {
	stream := fmt.Sprintf("BT /F1 18 Tf 36 96 Td (%s) Tj 0 -28 Td /F1 11 Tf (Made up by tuimeta-helper --fake.) Tj ET", title)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 420 160] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return []byte(b.String())
}

// mp4Box is a stand-in for a video or a voice message: an ISO media file
// header with nothing playable after it. The fake has no real recordings.
func mp4Box(brand string, size int) []byte {
	var b bytes.Buffer
	b.Write([]byte{0, 0, 0, 0x18})
	b.WriteString("ftyp")
	b.WriteString(brand)
	b.Write([]byte{0, 0, 0, 0})
	b.WriteString("isom" + brand)
	free := max(size-b.Len(), 8)
	b.Write([]byte{byte(free >> 24), byte(free >> 16), byte(free >> 8), byte(free)})
	b.WriteString("free")
	for i := 8; i < free; i++ {
		b.WriteByte(byte(i * 31))
	}
	return b.Bytes()
}
