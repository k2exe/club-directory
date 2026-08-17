package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
)

const maxPhotoBytes = 6 << 20   // 6 MB of image data
const maxUploadBody = 8 << 20   // whole request, leaving room for multipart overhead
const maxPhotoPixels = 25 << 20 // reject decompression bombs before decoding
const photoEdge = 512           // stored square edge, in pixels

// savePhoto decodes an uploaded image, crops it square, scales it down and
// re-encodes it as JPEG. Re-encoding is deliberate: it drops EXIF (including
// GPS coordinates a member almost certainly did not mean to publish) and
// guarantees the stored bytes are a real image rather than something
// disguised with an image extension.
func savePhoto(r io.Reader, dir, memberID string) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxPhotoBytes+1))
	if err != nil {
		return "", errors.New("that image could not be read")
	}
	if len(raw) > maxPhotoBytes {
		return "", errors.New("that image is larger than 6 MB")
	}
	// Check the declared dimensions before decoding: a small file can claim
	// enormous width and height and turn into gigabytes of pixels.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return "", errors.New("that file isn't a JPEG, PNG or GIF image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPhotoPixels {
		return "", errors.New("that image's dimensions are too large; keep it under 25 megapixels")
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", errors.New("that file isn't a JPEG, PNG or GIF image")
	}
	sq := cropSquare(img)
	out := resize(sq, photoEdge)

	name := fmt.Sprintf("%s-%s.jpg", memberID, randToken(6))
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := jpeg.Encode(f, out, &jpeg.Options{Quality: 86}); err != nil {
		os.Remove(path)
		return "", err
	}
	return name, nil
}

func cropSquare(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	side := w
	if h < side {
		side = h
	}
	x0 := b.Min.X + (w-side)/2
	y0 := b.Min.Y + (h-side)/3 // bias upward: faces sit above centre
	if y0+side > b.Max.Y {
		y0 = b.Max.Y - side
	}
	dst := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(dst, dst.Bounds(), src, image.Pt(x0, y0), draw.Src)
	return dst
}

// resize does a simple box-filter downscale. Adequate for thumbnails and
// avoids pulling in an imaging dependency.
func resize(src image.Image, edge int) image.Image {
	b := src.Bounds()
	if b.Dx() <= edge {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, edge, edge))
	scale := float64(b.Dx()) / float64(edge)
	for y := 0; y < edge; y++ {
		for x := 0; x < edge; x++ {
			x0 := b.Min.X + int(float64(x)*scale)
			y0 := b.Min.Y + int(float64(y)*scale)
			x1 := b.Min.X + int(float64(x+1)*scale)
			y1 := b.Min.Y + int(float64(y+1)*scale)
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if y1 <= y0 {
				y1 = y0 + 1
			}
			var rs, gs, bs, as, n uint64
			for yy := y0; yy < y1 && yy < b.Max.Y; yy++ {
				for xx := x0; xx < x1 && xx < b.Max.X; xx++ {
					r, g, bl, a := src.At(xx, yy).RGBA()
					rs += uint64(r >> 8)
					gs += uint64(g >> 8)
					bs += uint64(bl >> 8)
					as += uint64(a >> 8)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.Set(x, y, color.RGBA{uint8(rs / n), uint8(gs / n), uint8(bs / n), uint8(as / n)})
		}
	}
	return dst
}
