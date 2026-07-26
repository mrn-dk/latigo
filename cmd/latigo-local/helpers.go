package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/host"
)

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// loadImages reads each -image path, sniffs its media type, and applies the
// host-side size cap (host.CapImage) before it ever becomes part of a guest
// message — see host.DefaultMaxImageBytes for why capping happens here
// rather than later. An unreadable or uncappable image fails the whole run
// rather than silently proceeding without it.
func loadImages(paths []string) ([]abi.ImageData, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make([]abi.ImageData, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read image %s: %w", p, err)
		}
		img := abi.ImageData{MediaType: sniffImageMediaType(p, data), Data: data}
		capped, err := host.CapImage(img, host.DefaultMaxImageBytes)
		if err != nil {
			return nil, fmt.Errorf("attach image %s: %w", p, err)
		}
		out = append(out, capped)
	}
	return out, nil
}

// sniffImageMediaType infers a media type from p's extension, falling back to
// a magic-byte sniff of data.
func sniffImageMediaType(p string, data []byte) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case bytes.HasPrefix(data, []byte("RIFF")) && len(data) > 12 && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// prefixWriter returns an io.Writer that prefixes each line written to stdout.
func prefixWriter(prefix string) *lineWriter {
	return &lineWriter{prefix: prefix, out: os.Stdout}
}

type lineWriter struct {
	prefix string
	out    *os.File
	buf    bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// no full line yet; put back the partial
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		fmt.Fprint(w.out, w.prefix+line)
	}
	return len(p), nil
}
