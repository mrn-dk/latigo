package guest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrn-dk/latigo/abi"
)

func invokePlan(t *testing.T, a *Agent, args map[string]any) (string, bool) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, _, isErr := a.tools.Invoke(context.Background(), "plan", json.RawMessage(b))
	return out, isErr
}

func TestPlanSetGetUpdate(t *testing.T) {
	a := newTestAgent(t)

	out, isErr := invokePlan(t, a, map[string]any{
		"op": "set",
		"items": []map[string]any{
			{"id": 1, "text": "read the failing test", "status": "done"},
			{"id": 2, "text": "fix the handler", "status": "in_progress"},
			{"id": 3, "text": "run the suite"}, // status omitted -> defaults to pending
		},
	})
	if isErr {
		t.Fatalf("plan set errored: %s", out)
	}
	if len(a.plan) != 3 {
		t.Fatalf("a.plan has %d items, want 3", len(a.plan))
	}
	if a.plan[2].Status != PlanPending {
		t.Errorf("item 3 status = %q, want default %q", a.plan[2].Status, PlanPending)
	}

	getOut, isErr := invokePlan(t, a, map[string]any{"op": "get"})
	if isErr {
		t.Fatalf("plan get errored: %s", getOut)
	}
	if !strings.Contains(getOut, "fix the handler") || !strings.Contains(getOut, "run the suite") {
		t.Errorf("plan get output missing items:\n%s", getOut)
	}

	updOut, isErr := invokePlan(t, a, map[string]any{
		"op": "update",
		"items": []map[string]any{
			{"id": 2, "status": "done"},
			{"id": 3, "status": "in_progress"},
		},
	})
	if isErr {
		t.Fatalf("plan update errored: %s", updOut)
	}
	if a.plan[1].Status != PlanDone {
		t.Errorf("item 2 status = %q, want done", a.plan[1].Status)
	}
	if a.plan[2].Status != PlanInProgress {
		t.Errorf("item 3 status = %q, want in_progress", a.plan[2].Status)
	}
	// Text should be untouched by an update that only sets status.
	if a.plan[1].Text != "fix the handler" {
		t.Errorf("item 2 text = %q, want unchanged", a.plan[1].Text)
	}
}

func TestPlanUpdateUnknownIDErrorsAndDoesNotPartiallyApply(t *testing.T) {
	a := newTestAgent(t)
	a.plan = []PlanItem{{ID: 1, Text: "a", Status: PlanPending}}

	_, isErr := invokePlan(t, a, map[string]any{
		"op": "update",
		"items": []map[string]any{
			{"id": 1, "status": "done"},
			{"id": 99, "status": "done"}, // unknown id
		},
	})
	if !isErr {
		t.Fatal("expected an error for an unknown plan item id")
	}
	// Atomic: item 1's update must not have been applied either.
	if a.plan[0].Status != PlanPending {
		t.Errorf("item 1 status = %q, want unchanged (pending) after a failed batch update", a.plan[0].Status)
	}
}

func TestPlanUnknownOpErrors(t *testing.T) {
	_, isErr := invokePlan(t, newTestAgent(t), map[string]any{"op": "bogus"})
	if !isErr {
		t.Fatal("expected an error for an unknown op")
	}
}

func TestPlanSurvivesCheckpointRestore(t *testing.T) {
	client := NewClient(&fakeTransport{})
	a := NewAgent(Config{Goal: "g", MaxTurns: 1}, client)
	a.plan = []PlanItem{
		{ID: 1, Text: "step one", Status: PlanDone},
		{ID: 2, Text: "step two", Status: PlanInProgress},
	}

	cp := a.checkpointState(2)

	restored := NewAgent(Config{Goal: "g", MaxTurns: 1}, client)
	turn, ok := restored.restore(cp)
	if !ok || turn != 2 {
		t.Fatalf("restore returned turn=%d ok=%v, want 2,true", turn, ok)
	}
	if len(restored.plan) != 2 {
		t.Fatalf("restored plan has %d items, want 2", len(restored.plan))
	}
	if restored.plan[0] != a.plan[0] || restored.plan[1] != a.plan[1] {
		t.Errorf("restored plan = %+v, want %+v", restored.plan, a.plan)
	}
}

// TestOldCheckpointBlobWithoutPlanFieldDecodes verifies a checkpoint blob
// written before the "plan" field existed (spec 06) still decodes cleanly:
// restore must not error and must leave the plan empty rather than failing.
func TestOldCheckpointBlobWithoutPlanFieldDecodes(t *testing.T) {
	client := NewClient(&fakeTransport{})
	a := NewAgent(Config{Goal: "g", MaxTurns: 1}, client)

	oldBlob, _ := json.Marshal(map[string]any{
		"turn":     5,
		"messages": []abi.LLMMessage{{Role: "user", Content: "hi"}},
		"files":    map[string][]byte{},
		"done":     false,
		"summary":  "",
		// no "plan" key at all
	})

	turn, ok := a.restore(oldBlob)
	if !ok || turn != 5 {
		t.Fatalf("restore returned turn=%d ok=%v, want 5,true", turn, ok)
	}
	if len(a.plan) != 0 {
		t.Errorf("plan = %+v, want empty when the blob predates the plan field", a.plan)
	}
}

func TestPlanPinnedInLLMMessagesEachTurn(t *testing.T) {
	a := newTestAgent(t)
	a.messages = []abi.LLMMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "goal"},
	}

	// No plan set yet: messagesForLLM must be exactly a.messages (no reminder
	// tacked on when there's nothing to pin).
	if got := a.messagesForLLM(); len(got) != len(a.messages) {
		t.Fatalf("messagesForLLM with no plan = %d messages, want %d (unchanged)", len(got), len(a.messages))
	}

	a.plan = []PlanItem{{ID: 1, Text: "do the thing", Status: PlanInProgress}}
	got := a.messagesForLLM()
	if len(got) != len(a.messages)+1 {
		t.Fatalf("messagesForLLM with a plan = %d messages, want %d (+1 pinned reminder)", len(got), len(a.messages)+1)
	}
	last := got[len(got)-1]
	if !strings.Contains(last.Content, "do the thing") {
		t.Errorf("pinned plan message missing plan text: %q", last.Content)
	}
	// The pinned reminder must not have mutated the persisted transcript.
	if len(a.messages) != 2 {
		t.Errorf("a.messages mutated by messagesForLLM: %+v", a.messages)
	}
}

func TestPlanNotElidedByCompaction(t *testing.T) {
	a := newTestAgent(t)
	a.plan = []PlanItem{{ID: 1, Text: "survive compaction", Status: PlanPending}}
	a.messages = longTranscript(20) // large transcript that TestDefaultCompactWindow shows gets compacted hard

	compacted := a.Compact(a, a.messages)
	if len(compacted) >= len(a.messages) {
		t.Fatalf("expected compaction to actually shrink the transcript (sanity check on the test setup)")
	}
	a.messages = compacted

	// Even though the transcript was aggressively compacted, the plan itself
	// (agent state, not a transcript message) is untouched...
	if len(a.plan) != 1 || a.plan[0].Text != "survive compaction" {
		t.Fatalf("a.plan = %+v, want untouched by compaction", a.plan)
	}
	// ...and it still reaches the model every turn via the pinned reminder,
	// regardless of how much the transcript around it was compacted.
	msgs := a.messagesForLLM()
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.Content, "survive compaction") {
		t.Errorf("pinned plan missing from post-compaction messagesForLLM: %q", last.Content)
	}
}
