// seedgifs draws the dev server's GIF library: a handful of small animated
// GIFs the composer's picker can offer on a machine with no GIF provider and
// no network. They are generated rather than fetched so the repository owns
// them outright, and named so a search for "wave" or "pulse" finds one.
//
//	go run ./cmd/seedgifs            # writes into ./seedmedia/gifs
//	go run ./cmd/seedgifs -out DIR
package main

import (
	"flag"
	"image"
	"image/color"
	"image/gif"
	"log"
	"math"
	"os"
	"path/filepath"
)

const (
	size   = 200
	frames = 16
	// Hundredths of a second, the GIF's unit.
	delay = 6
)

// A motif is one GIF: a name, a palette pair, and how a pixel moves.
type motif struct {
	name       string
	start, end color.RGBA
	shade      func(x, y float64, t float64) float64
}

var motifs = []motif{
	{"pulse-glow", color.RGBA{0x2a, 0x1b, 0x5e, 0xff}, color.RGBA{0xff, 0x6b, 0x9d, 0xff}, func(x, y, t float64) float64 {
		r := math.Hypot(x-0.5, y-0.5)
		return smooth(0.5 + 0.5*math.Cos(2*math.Pi*(r*2.2-t)))
	}},
	{"ocean-wave", color.RGBA{0x05, 0x2b, 0x4a, 0xff}, color.RGBA{0x5f, 0xe1, 0xff, 0xff}, func(x, y, t float64) float64 {
		return smooth(0.5 + 0.5*math.Sin(2*math.Pi*(x*2+t)+math.Sin(2*math.Pi*(y+t))*1.5))
	}},
	{"spin-orbit", color.RGBA{0x1f, 0x10, 0x30, 0xff}, color.RGBA{0xff, 0xd1, 0x66, 0xff}, func(x, y, t float64) float64 {
		a := math.Atan2(y-0.5, x-0.5) / (2 * math.Pi)
		r := math.Hypot(x-0.5, y-0.5)
		return smooth(0.5 + 0.5*math.Cos(2*math.Pi*(a*3-t)) * (1 - math.Abs(r-0.32)*4))
	}},
	{"rain-fall", color.RGBA{0x12, 0x1a, 0x2e, 0xff}, color.RGBA{0xb8, 0xc6, 0xff, 0xff}, func(x, y, t float64) float64 {
		col := math.Floor(x * 10)
		phase := math.Mod(col*0.37, 1)
		return smooth(0.5 + 0.5*math.Cos(2*math.Pi*(y*3-t*2-phase)))
	}},
	{"heart-beat", color.RGBA{0x3a, 0x08, 0x1c, 0xff}, color.RGBA{0xff, 0x4d, 0x6d, 0xff}, func(x, y, t float64) float64 {
		beat := 0.85 + 0.15*math.Pow(math.Max(0, math.Sin(2*math.Pi*t)), 8)
		px := (x - 0.5) / beat * 3
		py := (0.55 - y) / beat * 3
		inside := math.Pow(px*px+py*py-1, 3) - px*px*py*py*py
		if inside < 0 {
			return 1
		}
		return 0.15
	}},
	{"confetti-drop", color.RGBA{0x0e, 0x0e, 0x12, 0xff}, color.RGBA{0x7c, 0xff, 0xb2, 0xff}, func(x, y, t float64) float64 {
		gx, gy := math.Floor(x*8), math.Floor(y*8)
		seed := math.Mod(gx*12.9898+gy*78.233, 1)
		on := math.Mod(seed+t, 1)
		if on < 0.5 && math.Mod(x*8, 1) > 0.3 && math.Mod(x*8, 1) < 0.7 && math.Mod(y*8, 1) > 0.3 && math.Mod(y*8, 1) < 0.7 {
			return 1
		}
		return 0.1
	}},
	{"sunrise-fade", color.RGBA{0x2b, 0x0a, 0x3d, 0xff}, color.RGBA{0xff, 0xa2, 0x3a, 0xff}, func(x, y, t float64) float64 {
		horizon := 0.65 - 0.25*(0.5+0.5*math.Sin(2*math.Pi*t))
		return smooth(1 - math.Min(1, math.Max(0, (y-horizon)*4)))
	}},
	{"bounce-ball", color.RGBA{0x10, 0x20, 0x20, 0xff}, color.RGBA{0xff, 0xff, 0xff, 0xff}, func(x, y, t float64) float64 {
		cy := 0.75 - 0.5*math.Abs(math.Sin(math.Pi*t))
		if math.Hypot(x-0.5, y-cy) < 0.12 {
			return 1
		}
		return 0.08
	}},
}

func smooth(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func main() {
	out := flag.String("out", "seedmedia/gifs", "directory to write into")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, m := range motifs {
		if err := write(filepath.Join(*out, m.name+".gif"), m); err != nil {
			log.Fatalf("%s: %v", m.name, err)
		}
	}
	log.Printf("wrote %d GIFs into %s", len(motifs), *out)
}

// palette is sixteen steps between the two colours; every frame shares it so
// the file stays small.
func palette(m motif) color.Palette {
	p := make(color.Palette, 16)
	for i := range p {
		f := float64(i) / 15
		p[i] = color.RGBA{
			uint8(float64(m.start.R)*(1-f) + float64(m.end.R)*f),
			uint8(float64(m.start.G)*(1-f) + float64(m.end.G)*f),
			uint8(float64(m.start.B)*(1-f) + float64(m.end.B)*f),
			0xff,
		}
	}
	return p
}

func write(path string, m motif) error {
	pal := palette(m)
	anim := &gif.GIF{LoopCount: 0}
	for f := 0; f < frames; f++ {
		t := float64(f) / frames
		img := image.NewPaletted(image.Rect(0, 0, size, size), pal)
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				v := m.shade(float64(x)/size, float64(y)/size, t)
				img.SetColorIndex(x, y, uint8(math.Round(v*15)))
			}
		}
		anim.Image = append(anim.Image, img)
		anim.Delay = append(anim.Delay, delay)
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return gif.EncodeAll(file, anim)
}
