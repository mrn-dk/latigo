package main

import (
	"bytes"
	"fmt"
	"os"
	"time"
)

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

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
