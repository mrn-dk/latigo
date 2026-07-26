package host

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif" // register GIF decoding for image.Decode
	"image/jpeg"
	_ "image/png" // register PNG decoding for image.Decode

	"github.com/mrn-dk/latigo/abi"
)

// DefaultMaxImageBytes is the reference cap applied to host-attached images
// (see cmd/latigo-local's -image flag) before they ever become part of a
// guest message.
//
// Why cap here and not later: the agent loop resends the *entire* transcript
// on every turn (guest/agent.go's a.messages), and the host's write-ahead
// durability records every llm.call request verbatim in the event log
// (host.Host.Dispatch, before any host-side processing runs). So once an
// oversized image is part of a.messages, it gets re-logged in full on every
// subsequent turn for the rest of the run — there is no way to shrink it out
// of the log after the fact without breaking replay. Capping at the point of
// attachment, before the bytes ever reach the guest, is the only place that
// actually bounds the log.
const DefaultMaxImageBytes = 2 << 20 // 2 MiB

// CapImage returns img unchanged when it already fits within maxBytes
// (maxBytes<=0 means unlimited, or img has no inline Data to shrink).
// Otherwise it decodes the image (PNG/JPEG/GIF, the formats importing this
// package registers), downscales it in a bounded number of steps, and
// re-encodes as JPEG at a shrinking quality/size until it fits.
//
// It returns an error — "reject" in the spec's downscale-or-reject language —
// when the bytes cannot be decoded as an image, or cannot be brought under
// the cap within the attempt budget; callers should treat that as "drop this
// image" rather than forwarding an oversized payload.
//
// This is implemented with only the standard library (image, image/png,
// image/jpeg, image/gif): the module intentionally carries no third-party
// image-processing dependency.
func CapImage(img abi.ImageData, maxBytes int) (abi.ImageData, error) {
	if maxBytes <= 0 || len(img.Data) <= maxBytes {
		return img, nil
	}
	src, _, err := image.Decode(bytes.NewReader(img.Data))
	if err != nil {
		return abi.ImageData{}, fmt.Errorf("cap image: decode: %w", err)
	}

	const maxAttempts = 6
	quality := 85
	scale := 1.0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resized := src
		if scale < 1.0 {
			resized = downscale(src, scale)
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: quality}); err != nil {
			return abi.ImageData{}, fmt.Errorf("cap image: encode: %w", err)
		}
		if buf.Len() <= maxBytes {
			return abi.ImageData{MediaType: "image/jpeg", Data: buf.Bytes()}, nil
		}
		scale *= 0.6
		if quality > 40 {
			quality -= 15
		}
	}
	return abi.ImageData{}, fmt.Errorf("cap image: could not shrink under %d bytes in %d attempts", maxBytes, maxAttempts)
}

// downscale nearest-neighbour resizes src by scale (0 < scale < 1). Plain
// stdlib image.Image sampling — no golang.org/x/image/draw dependency.
func downscale(src image.Image, scale float64) image.Image {
	b := src.Bounds()
	w := int(float64(b.Dx()) * scale)
	h := int(float64(b.Dy()) * scale)
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}
