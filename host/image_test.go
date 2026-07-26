package host

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"testing"

	"github.com/mrn-dk/latigo/abi"
)

func TestCapImageUnderLimitUnchanged(t *testing.T) {
	img := abi.ImageData{MediaType: "image/png", Data: []byte{1, 2, 3, 4}}
	got, err := CapImage(img, 1<<20)
	if err != nil {
		t.Fatalf("CapImage: %v", err)
	}
	if got.MediaType != img.MediaType || string(got.Data) != string(img.Data) {
		t.Errorf("CapImage under the limit modified the image: got %+v, want unchanged %+v", got, img)
	}
}

func TestCapImageNoLimitUnchanged(t *testing.T) {
	img := abi.ImageData{MediaType: "image/png", Data: make([]byte, 10_000)}
	got, err := CapImage(img, 0)
	if err != nil {
		t.Fatalf("CapImage: %v", err)
	}
	if len(got.Data) != len(img.Data) {
		t.Errorf("CapImage with maxBytes<=0 should be a no-op, got %d bytes", len(got.Data))
	}
}

func TestCapImageDownscalesOversizedImage(t *testing.T) {
	src := generateNoisePNG(t, 400, 400)
	if len(src) < 5000 {
		t.Fatalf("test fixture too small (%d bytes) to exercise capping", len(src))
	}
	const cap = 3000
	got, err := CapImage(abi.ImageData{MediaType: "image/png", Data: src}, cap)
	if err != nil {
		t.Fatalf("CapImage: %v", err)
	}
	if len(got.Data) > cap {
		t.Errorf("capped image is %d bytes, want <= %d", len(got.Data), cap)
	}
	if got.MediaType != "image/jpeg" {
		t.Errorf("capped image media type = %q, want image/jpeg (re-encoded)", got.MediaType)
	}
}

func TestCapImageRejectsUndecodable(t *testing.T) {
	junk := make([]byte, 5000)
	for i := range junk {
		junk[i] = byte(i)
	}
	if _, err := CapImage(abi.ImageData{MediaType: "image/png", Data: junk}, 100); err == nil {
		t.Error("CapImage on undecodable bytes should return an error (reject), got nil")
	}
}

// generateNoisePNG renders a deterministic w*h pseudo-random-noise PNG. Noise
// (rather than a flat color) keeps it from trivially deflating to near-zero
// bytes, which would defeat the "this image is actually oversized" premise
// of the downscale test.
func generateNoisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(rng.Intn(256)),
				G: uint8(rng.Intn(256)),
				B: uint8(rng.Intn(256)),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return buf.Bytes()
}
