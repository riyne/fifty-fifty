package receipts_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

var lim = receipts.DefaultImageLimits

func receiptPNG(t testing.TB, ref int) []byte {
	t.Helper()
	b, err := synth.Generate(ref).Render(synth.RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func receiptJPEG(t testing.TB, ref int) []byte {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(receiptPNG(t, ref)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func solidPNG(t testing.TB, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHardenImage_AcceptsAndReencodes(t *testing.T) {
	for name, in := range map[string][]byte{"png": receiptPNG(t, 3), "jpeg": receiptJPEG(t, 3)} {
		out, err := receipts.HardenImage(in, lim)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "image/" + name
		if out.ContentType != want {
			t.Errorf("%s: content type %q", name, out.ContentType)
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(out.Data))
		if err != nil {
			t.Fatalf("%s: output does not decode: %v", name, err)
		}
		if cfg.Width != out.Width || cfg.Height != out.Height || out.Width < 400 {
			t.Errorf("%s: reported %dx%d, decoded %dx%d", name, out.Width, out.Height, cfg.Width, cfg.Height)
		}
		_ = format
		if bytes.Equal(out.Data, in) && name == "jpeg" {
			t.Errorf("%s: output is the input; nothing was re-encoded", name)
		}
	}
}

// The point of re-encoding: whatever was not a pixel does not survive.
func TestHardenImage_StripsEverythingThatIsNotAPixel(t *testing.T) {
	const secret = "GPS-LAT-49.2827-SERIAL-ABC123"

	// JPEG: an EXIF segment (APP1) and a comment (COM) right after SOI, and a "zip"
	// after the end of the image.
	jp := receiptJPEG(t, 4)
	seg := func(marker byte, payload string) []byte {
		b := []byte{0xff, marker, 0, 0}
		binary.BigEndian.PutUint16(b[2:], uint16(len(payload)+2))
		return append(b, payload...)
	}
	dirty := append([]byte{}, jp[:2]...)
	dirty = append(dirty, seg(0xe1, "Exif\x00\x00"+secret)...)
	dirty = append(dirty, seg(0xfe, "comment "+secret)...)
	dirty = append(dirty, jp[2:]...)
	dirty = append(dirty, "PK\x03\x04"+secret...)
	if strings.Count(string(dirty), secret) != 3 {
		t.Fatal("test setup: secret not planted three times")
	}
	out, err := receipts.HardenImage(dirty, lim)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Data, []byte(secret)) || bytes.Contains(out.Data, []byte("PK\x03\x04")) || bytes.Contains(out.Data, []byte("Exif")) {
		t.Error("JPEG: metadata or trailing data survived")
	}

	// PNG: a tEXt chunk before the data, and bytes after IEND.
	pg := receiptPNG(t, 4)
	chunk := func(typ, data string) []byte {
		b := make([]byte, 8, 12+len(data))
		binary.BigEndian.PutUint32(b, uint32(len(data)))
		copy(b[4:], typ)
		b = append(b, data...)
		return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE([]byte(typ+data)))
	}
	dirtyPNG := append([]byte{}, pg[:33]...) // signature + IHDR (8 + 25)
	dirtyPNG = append(dirtyPNG, chunk("tEXt", "Comment\x00"+secret)...)
	dirtyPNG = append(dirtyPNG, pg[33:]...)
	dirtyPNG = append(dirtyPNG, "PK\x03\x04"+secret...)
	if strings.Count(string(dirtyPNG), secret) != 2 {
		t.Fatal("test setup: secret not planted twice")
	}
	out, err = receipts.HardenImage(dirtyPNG, lim)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Data, []byte(secret)) || bytes.Contains(out.Data, []byte("PK\x03\x04")) || bytes.Contains(out.Data, []byte("tEXt")) {
		t.Error("PNG: metadata or trailing data survived")
	}
}

func TestHardenImage_RefusesWhatIsNotAJpegOrPng(t *testing.T) {
	var g bytes.Buffer
	_ = gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 200, 200), color.Palette{color.White, color.Black}), nil)
	for name, data := range map[string][]byte{
		"empty":                nil,
		"text":                 []byte("hello, this is not an image at all"),
		"gif":                  g.Bytes(),
		"webp":                 []byte("RIFF\x24\x00\x00\x00WEBPVP8 " + strings.Repeat("\x00", 64)),
		"heic":                 []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic" + strings.Repeat("\x00", 64)),
		"svg":                  []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="500" height="500"><script>alert(1)</script></svg>`),
		"pdf":                  []byte("%PDF-1.4\n" + strings.Repeat("x", 200)),
		"html":                 []byte("<html><script>alert(1)</script></html>"),
		"png magic only":       []byte("\x89PNG\r\n\x1a\n"),
		"jpeg magic only":      []byte("\xff\xd8\xff"),
		"png magic then junk":  append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x41}, 100)...),
		"jpeg magic then junk": append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{0x41}, 100)...),
	} {
		_, err := receipts.HardenImage(data, lim)
		if !errors.Is(err, receipts.ErrImage) {
			t.Errorf("%s: err=%v, want ErrImage", name, err)
		}
		if name == "empty" && (err == nil || !strings.Contains(err.Error(), "empty")) {
			t.Errorf("empty upload: %v, want a message that says it is empty", err)
		}
	}
}

func TestHardenImage_RefusesTruncatedAndCorruptImages(t *testing.T) {
	for name, full := range map[string][]byte{"png": receiptPNG(t, 5), "jpeg": receiptJPEG(t, 5)} {
		for _, cut := range []int{len(full) / 2, len(full) - 20, len(full) - 1} {
			if _, err := receipts.HardenImage(full[:cut], lim); !errors.Is(err, receipts.ErrImage) {
				t.Errorf("%s cut at %d of %d: err=%v", name, cut, len(full), err)
			}
		}
		// Damaging the middle of the compressed data: a PNG notices (checksums); a JPEG
		// usually decodes to garbage, so only the PNG is required to refuse.
		bad := append([]byte{}, full...)
		for i := len(bad) / 2; i < len(bad)/2+64; i++ {
			bad[i] ^= 0xff
		}
		if _, err := receipts.HardenImage(bad, lim); name == "png" && !errors.Is(err, receipts.ErrImage) {
			t.Errorf("png with damaged data: err=%v", err)
		}
	}
}

func TestHardenImage_SizeLimits(t *testing.T) {
	good := receiptPNG(t, 6)
	tight := lim
	tight.MaxUploadBytes = len(good)
	if _, err := receipts.HardenImage(good, tight); err != nil {
		t.Errorf("a file of exactly the limit was refused: %v", err)
	}
	tight.MaxUploadBytes = len(good) - 1
	if _, err := receipts.HardenImage(good, tight); !errors.Is(err, receipts.ErrTooLarge) {
		t.Errorf("a file one byte over: err=%v, want ErrTooLarge", err)
	}

	// The limit on what is stored applies to the re-encoded bytes.
	out, _ := receipts.HardenImage(good, lim)
	tight = lim
	tight.MaxStoredBytes = len(out.Data)
	if _, err := receipts.HardenImage(good, tight); err != nil {
		t.Errorf("stored size exactly at the limit was refused: %v", err)
	}
	tight.MaxStoredBytes = len(out.Data) - 1
	if _, err := receipts.HardenImage(good, tight); !errors.Is(err, receipts.ErrTooLarge) {
		t.Errorf("stored size one byte over: err=%v", err)
	}
}

func TestHardenImage_DimensionLimitsAreExact(t *testing.T) {
	l := receipts.ImageLimits{MaxUploadBytes: 1 << 20, MaxStoredBytes: 1 << 20, MaxSide: 300, MinSide: 100, MaxPixels: 200 * 200}
	for _, tc := range []struct {
		w, h int
		ok   bool
	}{
		{200, 200, true},  // exactly the pixel limit
		{201, 199, true},  // 39,999 pixels
		{201, 200, false}, // 40,200 pixels
		{100, 100, true},  // exactly the minimum side
		{99, 200, false},  // one under on the width
		{200, 99, false},  // one under on the height
		{300, 100, true},  // exactly the maximum side, within the pixel limit
		{301, 100, false}, // one over on the side
		{100, 301, false},
		{1, 1, false},
	} {
		_, err := receipts.HardenImage(solidPNG(t, tc.w, tc.h, color.White), l)
		if (err == nil) != tc.ok {
			t.Errorf("%dx%d: err=%v, want ok=%v", tc.w, tc.h, err, tc.ok)
		}
	}
}

// A tiny file that declares a huge picture is refused from its header, before any
// memory is allocated for pixels.
func TestHardenImage_DimensionBombsAreRefusedBeforeDecoding(t *testing.T) {
	// PNG: signature, then an IHDR claiming 30000 x 30000 (900 megapixels), then IEND.
	chunk := func(typ string, data []byte) []byte {
		b := make([]byte, 8, 12+len(data))
		binary.BigEndian.PutUint32(b, uint32(len(data)))
		copy(b[4:], typ)
		b = append(b, data...)
		return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 30000)
	binary.BigEndian.PutUint32(ihdr[4:], 30000)
	ihdr[8], ihdr[9] = 8, 6 // 8 bits, RGBA
	bombPNG := append([]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", ihdr)...)
	bombPNG = append(bombPNG, chunk("IEND", nil)...)

	// JPEG: SOI, then a baseline frame header (SOF0) claiming 20000 x 20000, one component.
	sof := []byte{0xff, 0xc0, 0, 11, 8, 0x4e, 0x20, 0x4e, 0x20, 1, 1, 0x11, 0}
	bombJPEG := append([]byte("\xff\xd8"), sof...)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for name, data := range map[string][]byte{"png": bombPNG, "jpeg": bombJPEG} {
		start := time.Now()
		if _, err := receipts.HardenImage(data, lim); !errors.Is(err, receipts.ErrImage) {
			t.Errorf("%s: err=%v", name, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: took %v", name, d)
		}
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("refusing two bombs allocated %d bytes", grew)
	}
}

func TestHardenImage_FlattensTransparencyOntoWhite(t *testing.T) {
	// Black text-colored pixels on a fully transparent background.
	img := image.NewNRGBA(image.Rect(0, 0, 120, 120))
	for y := 40; y < 60; y++ {
		for x := 20; x < 100; x++ {
			img.SetNRGBA(x, y, color.NRGBA{A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out, err := receipts.HardenImage(buf.Bytes(), lim)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := png.Decode(bytes.NewReader(out.Data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, a := dec.At(5, 5).RGBA() // was transparent
	if r>>8 != 255 || g>>8 != 255 || b>>8 != 255 || a>>8 != 255 {
		t.Errorf("transparent pixel became %v %v %v %v, want opaque white", r>>8, g>>8, b>>8, a>>8)
	}
	if r, _, _, _ := dec.At(50, 50).RGBA(); r != 0 {
		t.Errorf("opaque black pixel became %v", r)
	}
}

func TestHardenImage_IsDeterministic(t *testing.T) {
	in := receiptJPEG(t, 7)
	a, _ := receipts.HardenImage(in, lim)
	b, _ := receipts.HardenImage(in, lim)
	if !bytes.Equal(a.Data, b.Data) {
		t.Error("two runs on the same input differ")
	}
	// Hardening twice is stable in content type and size, which is what makes the
	// stored object's key a function of the image.
	c, err := receipts.HardenImage(a.Data, lim)
	if err != nil || c.Width != a.Width || c.Height != a.Height || c.ContentType != a.ContentType {
		t.Errorf("second pass: %+v %v", c, err)
	}
}

// FuzzHardenImage feeds arbitrary bytes. The properties: it never panics or hangs, and
// when it accepts, the output is a decodable image of the reported size and format
// within the limits. `go test` runs the seed corpus; `go test -fuzz=FuzzHardenImage`
// searches further.
func FuzzHardenImage(f *testing.F) {
	f.Add(receiptPNG(f, 1))
	f.Add(receiptJPEG(f, 1))
	f.Add([]byte("\x89PNG\r\n\x1a\n"))
	f.Add([]byte("\xff\xd8\xff\xe0\x00\x10JFIF"))
	f.Add([]byte{})
	small := receipts.ImageLimits{MaxUploadBytes: 1 << 20, MaxStoredBytes: 1 << 20, MaxSide: 600, MinSide: 10, MaxPixels: 200_000}
	f.Add(solidPNG(f, 40, 40, color.Black))
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := receipts.HardenImage(data, small)
		if err != nil {
			if !errors.Is(err, receipts.ErrImage) && !errors.Is(err, receipts.ErrTooLarge) {
				t.Fatalf("unexpected error type: %v", err)
			}
			return
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(out.Data))
		if err != nil {
			t.Fatalf("accepted input produced undecodable output: %v", err)
		}
		if "image/"+format != out.ContentType || cfg.Width != out.Width || cfg.Height != out.Height {
			t.Fatalf("report %+v does not match decoded %s %dx%d", out, format, cfg.Width, cfg.Height)
		}
		if out.Width*out.Height > small.MaxPixels || out.Width < small.MinSide || out.Height < small.MinSide || len(out.Data) > small.MaxStoredBytes {
			t.Fatalf("output outside the limits: %+v", out)
		}
	})
}
