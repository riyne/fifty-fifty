package receipts

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
)

// ErrImage means the upload is not an image we accept. Its message is fixed text and
// numbers, never a quote of the upload, so it can be shown to the caller and logged.
var ErrImage = errors.New("receipts: unsupported image")

// ErrTooLarge means the upload, or the image after re-encoding, is over the size limit.
var ErrTooLarge = errors.New("receipts: image too large")

// ImageLimits bound what HardenImage accepts. The pixel limit is the one that matters
// for memory: a decoder allocates width x height before it reads the pixels, so a tiny
// file can claim a huge picture. HardenImage reads the declared size from the header
// and refuses before decoding.
type ImageLimits struct {
	MaxUploadBytes int // size of the file as uploaded
	MaxStoredBytes int // size after re-encoding (a PNG can grow)
	MaxSide        int // longest side, pixels
	MinSide        int // shortest side, pixels: below this no text is readable
	MaxPixels      int // width x height
}

// DefaultImageLimits: a 12 megapixel phone photo is about 4 MB as a JPEG, a 25
// megapixel limit leaves room for 48 megapixel cameras that save at half size, and a
// decoded 25 megapixel image is on the order of 40-100 MB of memory, which is why
// callers also bound how many run at once.
var DefaultImageLimits = ImageLimits{
	MaxUploadBytes: 10 << 20,
	MaxStoredBytes: 20 << 20,
	MaxSide:        10000,
	MinSide:        100,
	MaxPixels:      25_000_000,
}

// Image is a hardened upload: re-encoded bytes and what is known about them.
type Image struct {
	Data        []byte
	ContentType string // "image/jpeg" or "image/png"
	Width       int
	Height      int
}

const jpegQuality = 90

// HardenImage turns untrusted upload bytes into an image we are willing to store and
// hand to OCR. It never trusts a declared content type or file name (there are none
// here): the format is decided from the bytes.
//
//  1. Size: at most MaxUploadBytes.
//  2. Sniff: the leading bytes must be PNG or JPEG. Everything else (GIF, WebP, HEIC,
//     SVG, PDF, text) is refused; no decoder for them is even linked in.
//  3. Dimensions: read from the header and checked against the limits before any
//     pixel is decoded.
//  4. Decode fully with the standard library's decoder; truncated or corrupt files fail.
//  5. Re-encode from the decoded pixels. This is what removes everything that was not a
//     pixel: EXIF (including GPS position and camera serial), ICC profiles, comments,
//     thumbnails, extra PNG chunks, and any bytes after the end of the image (a file
//     that is also a valid archive or script stops being one).
//
// Known limit: re-encoding drops the EXIF orientation tag without applying it, so a
// phone photo taken sideways and stored with an orientation tag comes out rotated. The
// client is expected to send upright pixels (docs/ARCHITECTURE.md section 10);
// transparency in a PNG is flattened onto white.
func HardenImage(data []byte, lim ImageLimits) (Image, error) {
	switch {
	case len(data) == 0:
		return Image{}, fmt.Errorf("%w: empty file", ErrImage)
	case len(data) > lim.MaxUploadBytes:
		return Image{}, fmt.Errorf("%w: file is larger than %d bytes", ErrTooLarge, lim.MaxUploadBytes)
	}

	var (
		cfg    image.Config
		err    error
		isPNG  = bytes.HasPrefix(data, pngMagic)
		isJPEG = bytes.HasPrefix(data, jpegMagic)
	)
	switch {
	case isPNG:
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
	case isJPEG:
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
	default:
		return Image{}, fmt.Errorf("%w: only JPEG and PNG are accepted", ErrImage)
	}
	if err != nil {
		return Image{}, fmt.Errorf("%w: unreadable image header", ErrImage)
	}
	w, h := cfg.Width, cfg.Height
	switch {
	case w < lim.MinSide || h < lim.MinSide:
		return Image{}, fmt.Errorf("%w: image is smaller than %d pixels on a side", ErrImage, lim.MinSide)
	case w > lim.MaxSide || h > lim.MaxSide:
		return Image{}, fmt.Errorf("%w: image is larger than %d pixels on a side", ErrImage, lim.MaxSide)
	case int64(w)*int64(h) > int64(lim.MaxPixels):
		return Image{}, fmt.Errorf("%w: image has more than %d pixels", ErrImage, lim.MaxPixels)
	}

	var img image.Image
	if isPNG {
		img, err = png.Decode(bytes.NewReader(data))
	} else {
		img, err = jpeg.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return Image{}, fmt.Errorf("%w: image data is corrupt or truncated", ErrImage)
	}
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		// The header promised one size and the decoder produced another.
		return Image{}, fmt.Errorf("%w: image size is inconsistent", ErrImage)
	}

	var out bytes.Buffer
	ct := "image/jpeg"
	if isPNG {
		ct = "image/png"
		err = (&png.Encoder{}).Encode(&out, flatten(img))
	} else {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return Image{}, fmt.Errorf("receipts: re-encode: %w", err)
	}
	if out.Len() > lim.MaxStoredBytes {
		return Image{}, fmt.Errorf("%w: image is larger than %d bytes after processing", ErrTooLarge, lim.MaxStoredBytes)
	}
	return Image{Data: out.Bytes(), ContentType: ct, Width: w, Height: h}, nil
}

var (
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
	jpegMagic = []byte("\xff\xd8\xff")
)

// flatten puts an image with transparency onto white, so OCR never sees black text on
// a "transparent" background that renders as black. Opaque images pass through.
func flatten(img image.Image) image.Image {
	if o, ok := img.(interface{ Opaque() bool }); ok && o.Opaque() {
		return img
	}
	dst := image.NewRGBA(img.Bounds())
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Over)
	return dst
}
