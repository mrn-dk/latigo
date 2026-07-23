package host

import (
	"encoding/json"
	"os"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
)

// CompactLog rewrites the event log at path for bounded replay: it keeps the
// run_start, the guest's initial tool.list and state.restore calls, everything
// after the most recent checkpoint, and the run_end — dropping the hostcalls of
// every turn folded into that checkpoint. The retained state.restore is
// rewritten to hand the guest the checkpoint snapshot, so on replay the guest
// resumes from it instead of re-running the compacted turns.
//
// It returns the number of events removed. If the log has no checkpoint, it is
// left unchanged and 0 is returned.
//
// This preserves Latigo's core guarantee — replay reconstructs state from
// recorded results, never by re-executing side effects — while bounding the log
// to the tail since the last checkpoint.
func CompactLog(path string) (int, error) {
	evs, err := ReadEvents(path)
	if err != nil {
		return 0, err
	}

	// Find the most recent checkpoint (highest Seq) and its state.
	var (
		boundary uint64
		cpState  json.RawMessage
		haveCP   bool
	)
	for _, ev := range evs {
		if ev.Kind == events.KindCheckpoint {
			var cp events.Checkpoint
			if err := json.Unmarshal(ev.Payload, &cp); err != nil {
				return 0, err
			}
			if !haveCP || ev.Seq >= boundary {
				boundary, cpState, haveCP = ev.Seq, cp.State, true
			}
		}
	}
	if !haveCP {
		return 0, nil // nothing to compact
	}

	restoreResp := encodeResponse(abi.Response{Result: mustMarshal(abi.StateRestoreResponse{
		Found: true, State: cpState,
	})})

	var kept []events.Event
	var seenToolList, seenRestore bool
	for _, ev := range evs {
		switch ev.Kind {
		case events.KindRunStart, events.KindRunEnd:
			kept = append(kept, ev)
			continue
		}
		// The guest always re-issues tool.list then state.restore at startup, so
		// those two initial hostcalls must survive even though they precede the
		// checkpoint. The restore is rewritten to carry the snapshot.
		if ev.Kind == events.KindHostcall {
			var hc events.Hostcall
			if err := json.Unmarshal(ev.Payload, &hc); err != nil {
				return 0, err
			}
			if hc.Op == abi.OpToolList && !seenToolList && ev.Seq <= boundary {
				seenToolList = true
				kept = append(kept, ev)
				continue
			}
			if hc.Op == abi.OpStateRestore && !seenRestore && ev.Seq <= boundary {
				seenRestore = true
				hc.Response = restoreResp
				ev.Payload = mustMarshal(hc)
				kept = append(kept, ev)
				continue
			}
		}
		// Everything strictly after the checkpoint is part of the live tail.
		if ev.Seq > boundary {
			kept = append(kept, ev)
		}
	}

	removed := len(evs) - len(kept)
	if err := rewriteLog(path, kept); err != nil {
		return 0, err
	}
	return removed, nil
}

// rewriteLog atomically replaces the log at path with evs, renumbering Seq from
// 1 in order.
func rewriteLog(path string, evs []events.Event) error {
	tmp := path + ".compact.tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for i := range evs {
		evs[i].Seq = uint64(i + 1)
		b, err := json.Marshal(evs[i])
		if err != nil {
			f.Close()
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
