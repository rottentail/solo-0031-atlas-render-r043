package bundle_test

import (
	"image"
	"image/color"
	"image/png"
	"io"
)

func newSolidPNG(width, height int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func pngEncode(w io.Writer, img image.Image) error {
	return png.Encode(w, img)
}
