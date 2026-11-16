package persist_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func solidPNG(t *testing.T, width, height int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		t.Fatalf("encode png: %v", e)
	}
	return b.Bytes()
}
