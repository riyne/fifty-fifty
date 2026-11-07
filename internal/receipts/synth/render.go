package synth

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand/v2"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// RenderOptions describe how a receipt is drawn. The zero value is a clean, upright,
// noise-free image at the default size.
type RenderOptions struct {
	// FontPx is the font size in pixels. Zero means 28, which gives characters about
	// 17 px wide: comfortably above what an OCR engine wants, and a few hundred
	// kilobytes of PNG.
	FontPx float64
	// SkewDegrees rotates the whole receipt about its centre, as a photographed page
	// is never quite square. Positive is clockwise. At most 15.
	SkewDegrees float64
	// NoiseSigma is the standard deviation, in gray levels (0 to 255), of Gaussian
	// noise added to every pixel. At most 100.
	NoiseSigma float64
	// Seed seeds the noise.
	Seed uint64
}

func (o RenderOptions) validate() error {
	switch {
	case math.IsNaN(o.FontPx) || o.FontPx < 0 || o.FontPx > 100:
		return fmt.Errorf("synth: font size %v out of range", o.FontPx)
	case math.IsNaN(o.SkewDegrees) || math.Abs(o.SkewDegrees) > 15:
		return fmt.Errorf("synth: skew %v out of range", o.SkewDegrees)
	case math.IsNaN(o.NoiseSigma) || o.NoiseSigma < 0 || o.NoiseSigma > 100:
		return fmt.Errorf("synth: noise %v out of range", o.NoiseSigma)
	}
	return nil
}

// Render draws the receipt as black text on white and returns a PNG. Whether the text
// can be read back is not claimed here: it is measured by running OCR over the output
// (the redaction recall benchmark).
func (r Receipt) Render(o RenderOptions) ([]byte, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	if len(r.printed) == 0 {
		return nil, errors.New("synth: receipt has no text; build it with Generate")
	}
	px := o.FontPx
	if px == 0 {
		px = 28
	}
	f, err := opentype.Parse(gomono.TTF)
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer face.Close()

	// Go Mono advances 0.6 em per character.
	const margin = 40
	advance := int(math.Ceil(px * 0.6))
	lineH := int(math.Ceil(px * 1.35))
	w := 2*margin + Width*advance
	h := 2*margin + len(r.printed)*lineH

	page := image.NewGray(image.Rect(0, 0, w, h))
	for i := range page.Pix {
		page.Pix[i] = 255
	}
	d := font.Drawer{Dst: page, Src: image.NewUniform(color.Gray{Y: 0}), Face: face}
	for i, line := range r.printed {
		d.Dot = fixed.P(margin, margin+i*lineH+int(px))
		d.DrawString(line)
	}

	out := page
	if o.SkewDegrees != 0 {
		out = rotate(page, o.SkewDegrees)
	}
	if o.NoiseSigma > 0 {
		addNoise(out, o.NoiseSigma, o.Seed)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rotate turns src by deg degrees (clockwise) about its centre onto a white canvas
// large enough to hold every corner, sampling bilinearly.
func rotate(src *image.Gray, deg float64) *image.Gray {
	rad := deg * math.Pi / 180
	sin, cos := math.Sincos(rad)
	sw, sh := float64(src.Bounds().Dx()), float64(src.Bounds().Dy())
	dw := int(math.Ceil(sw*math.Abs(cos) + sh*math.Abs(sin)))
	dh := int(math.Ceil(sw*math.Abs(sin) + sh*math.Abs(cos)))
	dst := image.NewGray(image.Rect(0, 0, dw, dh))
	for i := range dst.Pix {
		dst.Pix[i] = 255
	}
	scx, scy := sw/2, sh/2
	dcx, dcy := float64(dw)/2, float64(dh)/2
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			// Inverse mapping: where in the source does this destination pixel come from?
			dx, dy := float64(x)+0.5-dcx, float64(y)+0.5-dcy
			sx := dx*cos + dy*sin + scx - 0.5
			sy := -dx*sin + dy*cos + scy - 0.5
			if sx < 0 || sy < 0 || sx > sw-1 || sy > sh-1 {
				continue
			}
			x0, y0 := int(sx), int(sy)
			fx, fy := sx-float64(x0), sy-float64(y0)
			p := func(xx, yy int) float64 {
				if xx >= int(sw) {
					xx = int(sw) - 1
				}
				if yy >= int(sh) {
					yy = int(sh) - 1
				}
				return float64(src.GrayAt(xx, yy).Y)
			}
			v := p(x0, y0)*(1-fx)*(1-fy) + p(x0+1, y0)*fx*(1-fy) + p(x0, y0+1)*(1-fx)*fy + p(x0+1, y0+1)*fx*fy
			dst.SetGray(x, y, color.Gray{Y: uint8(math.Round(v))})
		}
	}
	return dst
}

func addNoise(img *image.Gray, sigma float64, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 0x6e6f697365))
	for i, v := range img.Pix {
		n := float64(v) + rng.NormFloat64()*sigma
		img.Pix[i] = uint8(math.Max(0, math.Min(255, math.Round(n))))
	}
}
