package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// This file implements the plan/todo built-in (spec 06): a durable, structured
// task list the model maintains across turns. It is pure agent-state
// computation — no hostcall — so it is deterministic and free to replay. The
// plan is stored on Agent (a.plan), included in the checkpoint snapshot (see
// agentSnapshot in guest/agent.go) so it survives restore/compaction, and
// pinned into the LLM request fresh every turn (see Agent.messagesForLLM) so
// it is never a candidate for guest/compaction.go's transcript elision.

// Plan status values.
const (
	PlanPending    = "pending"
	PlanInProgress = "in_progress"
	PlanDone       = "done"
)

// PlanItem is one entry in the agent's durable plan/todo list.
type PlanItem struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"` // pending | in_progress | done
}

func validPlanStatus(s string) bool {
	switch s {
	case PlanPending, PlanInProgress, PlanDone, "":
		return true
	default:
		return false
	}
}

// registerPlanTools adds the plan built-in to the registry.
func (a *Agent) registerPlanTools() {
	r := a.tools

	r.Add(Tool{
		Name: "plan",
		Description: "Maintain a durable task plan/todo list. op=\"set\" replaces the whole plan; " +
			"op=\"update\" edits existing items by id (text and/or status, leaving other fields as-is); " +
			"op=\"get\" returns the current plan unchanged. status is one of pending, in_progress, done " +
			"(defaults to pending when omitted on set). The plan is pinned into context every turn and " +
			"survives checkpoint/restore and compaction.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"op":{"type":"string","enum":["set","update","get"]},` +
			`"items":{"type":"array","items":{"type":"object","properties":{` +
			`"id":{"type":"integer"},"text":{"type":"string"},` +
			`"status":{"type":"string","enum":["pending","in_progress","done"]}` +
			`},"required":["id"]}}` +
			`},"required":["op"]}`),
		Invoke: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Op    string     `json:"op"`
				Items []PlanItem `json:"items"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			switch in.Op {
			case "set":
				for _, it := range in.Items {
					if !validPlanStatus(it.Status) {
						return "", fmt.Errorf("plan: invalid status %q for item %d", it.Status, it.ID)
					}
				}
				a.plan = normalizePlan(in.Items)
				return renderPlan(a.plan), nil
			case "update":
				if err := a.updatePlan(in.Items); err != nil {
					return "", err
				}
				return renderPlan(a.plan), nil
			case "get":
				return renderPlan(a.plan), nil
			default:
				return "", fmt.Errorf("plan: unknown op %q (want \"set\", \"update\", or \"get\")", in.Op)
			}
		},
	})
}

// normalizePlan copies items, defaulting an empty status to pending.
func normalizePlan(items []PlanItem) []PlanItem {
	out := make([]PlanItem, len(items))
	for i, it := range items {
		if it.Status == "" {
			it.Status = PlanPending
		}
		out[i] = it
	}
	return out
}

// updatePlan applies a batch of by-id edits atomically: it validates and
// resolves every update against a working copy first, and only replaces
// a.plan once all of them succeed. Any unknown id or invalid status aborts
// the whole batch, leaving a.plan untouched.
func (a *Agent) updatePlan(updates []PlanItem) error {
	next := make([]PlanItem, len(a.plan))
	copy(next, a.plan)
	for _, upd := range updates {
		if upd.Status != "" && !validPlanStatus(upd.Status) {
			return fmt.Errorf("plan: invalid status %q for item %d", upd.Status, upd.ID)
		}
		idx := -1
		for i, it := range next {
			if it.ID == upd.ID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return fmt.Errorf("plan: update: no item with id %d", upd.ID)
		}
		if upd.Text != "" {
			next[idx].Text = upd.Text
		}
		if upd.Status != "" {
			next[idx].Status = upd.Status
		}
	}
	a.plan = next
	return nil
}

// renderPlan formats items as a checklist, used both for the tool's own text
// result and for the pinned per-turn context reminder.
func renderPlan(items []PlanItem) string {
	if len(items) == 0 {
		return "(empty plan)"
	}
	var b strings.Builder
	for _, it := range items {
		mark := "[ ]"
		switch it.Status {
		case PlanInProgress:
			mark = "[~]"
		case PlanDone:
			mark = "[x]"
		}
		fmt.Fprintf(&b, "%s %d. %s\n", mark, it.ID, it.Text)
	}
	return b.String()
}
