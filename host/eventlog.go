package host

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/mrn-dk/latigo/events"
)

// EventLog is an append-only JSONL durable log. Every hostcall result is
// appended and flushed (write-ahead) before the guest observes it.
type EventLog struct {
	mu   sync.Mutex
	w    *os.File
	bw   *bufio.Writer
	seq  uint64
	harn string
}

// OpenEventLog opens (creating if needed) a JSONL log at path for appending.
func OpenEventLog(path, harnessVersion string) (*EventLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &EventLog{w: f, bw: bufio.NewWriter(f), harn: harnessVersion}, nil
}

// Append writes an event of the given kind with the payload, assigning the next
// sequence number, then flushes and fsyncs so the record is durable before the
// caller proceeds.
func (l *EventLog) Append(kind events.Kind, payload any) (events.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	ev, err := events.Marshal(l.seq, kind, l.harn, payload)
	if err != nil {
		return events.Event{}, err
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return events.Event{}, err
	}
	if _, err := l.bw.Write(append(b, '\n')); err != nil {
		return events.Event{}, err
	}
	if err := l.bw.Flush(); err != nil {
		return events.Event{}, err
	}
	if err := l.w.Sync(); err != nil {
		return events.Event{}, err
	}
	return ev, nil
}

// Close flushes and closes the log.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.bw.Flush(); err != nil {
		return err
	}
	return l.w.Close()
}

// ReadEvents reads all events from a JSONL log file.
func ReadEvents(path string) ([]events.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []events.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev events.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("event log: %w", err)
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return nil, err
	}
	return out, nil
}
