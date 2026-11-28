package cardkit

import (
	"bytes"
	"image"
	"image/png"
)

// EncodePNG writes img as a PNG at BestSpeed: about a third of the default
// level's encode time for a few KiB more (#727 plan §2.5).
func EncodePNG(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
