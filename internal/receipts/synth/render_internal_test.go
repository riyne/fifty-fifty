package synth

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"
)

// inkCentroidY is the mean row of the dark pixels in columns [x0, x1).
func inkCentroidY(g image.Image, x0, x1 int) float64 {
	var sum, n float64
	b := g.Bounds()
	gray := g.(*image.Gray)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := x0; x < x1; x++ {
			if gray.GrayAt(x, y).Y < 128 {
				sum += float64(y)
				n++
			}
		}
	}
	return sum / n
}

// Positive skew is documented as clockwise: the right edge of the page goes down. A
// single horizontal rule is the cleanest thing to measure, because on a whole receipt
// the layout alone puts more ink at different heights on the two sides.
func TestRender_PositiveSkewIsClockwise(t *testing.T) {
	r := Receipt{printed: []string{strings.Repeat("-", Width)}}
	for _, tc := range []struct {
		deg   float64
		wantD float64 // right centroid minus left centroid, in pixels
	}{{4, 1}, {-4, -1}, {0, 0}} {
		b, err := r.Render(RenderOptions{SkewDegrees: tc.deg})
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		w := img.Bounds().Dx()
		d := inkCentroidY(img, 3*w/4, w) - inkCentroidY(img, 0, w/4)
		switch {
		case tc.wantD > 0 && d < 15, tc.wantD < 0 && d > -15, tc.wantD == 0 && (d > 1 || d < -1):
			t.Errorf("skew %v: right minus left = %.1f px", tc.deg, d)
		}
	}
}

// Noise must saturate at black and white. Without the clamp the conversion to a byte
// wraps around, and a white pixel pushed past 255 comes out almost black.
func TestAddNoise_Clamps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start uint8
		wrong func(v uint8) bool // what wraparound would produce
	}{
		{"white", 255, func(v uint8) bool { return v < 50 }},
		{"black", 0, func(v uint8) bool { return v > 205 }},
	} {
		img := image.NewGray(image.Rect(0, 0, 200, 200))
		for i := range img.Pix {
			img.Pix[i] = tc.start
		}
		addNoise(img, 100, 1)
		bad := 0
		for _, v := range img.Pix {
			if tc.wrong(v) {
				bad++
			}
		}
		// A clamped normal with sigma 100 puts about 2% of pixels beyond 2 sigma on the
		// wrong side; wraparound would put about 20% there.
		if share := float64(bad) / float64(len(img.Pix)); share > 0.05 {
			t.Errorf("%s: %.1f%% of pixels flipped to the other end", tc.name, 100*share)
		}
	}
}
