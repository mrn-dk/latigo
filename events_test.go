package main

import (
	"path/filepath"
	"testing"
)

func TestEventLogAppendAndSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	l, err := OpenEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := l.Append(KindTurn, TurnPayload{Turn: i}); err != nil {
			t.Fatal(err)
		}
	}
	if l.seq != 3 {
		t.Fatalf("seq=%d want 3", l.seq)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := OpenEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if err := l2.ResumeSeq(); err != nil {
		t.Fatal(err)
	}
	if l2.seq != 3 {
		t.Fatalf("resumed seq=%d want 3", l2.seq)
	}
	ev, err := l2.Append(KindTurn, TurnPayload{Turn: 3})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 4 {
		t.Fatalf("next seq=%d want 4", ev.Seq)
	}
}

func TestLoadTranscriptRebuildsConversation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	l, _ := OpenEventLog(path)
	_, _ = l.Append(KindRunStart, RunStartPayload{RunID: "r1", Goal: "do the thing", Model: "gpt-x"})
	_, _ = l.Append(KindLLM, LLMPayload{
		Turn: 0, Model: "gpt-x", TotalTokens: 12,
		Message: Message{Role: "assistant", Content: "let me check", ToolCalls: []ToolCall{
			{ID: "c1", Type: "function", Function: FuncCall{Name: "shell", Arguments: `{"command":"ls"}`}},
		}},
	})
	_, _ = l.Append(KindTool, ToolPayload{CallID: "c1", IdempotencyKey: "k1", Name: "shell", Status: "intent"})
	_, _ = l.Append(KindTool, ToolPayload{CallID: "c1", IdempotencyKey: "k1", Name: "shell", Status: "ok", Stdout: "a.txt\n"})
	_, _ = l.Append(KindTurnEnd, TurnEndPayload{Turn: 0})
	_, _ = l.Append(KindLog, LogPayload{Level: "info", Message: "noop"})
	l.Close()

	lt, err := LoadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if lt.Goal != "do the thing" || lt.Model != "gpt-x" {
		t.Fatalf("run_start not loaded: %+v", lt)
	}
	if len(lt.Messages) != 2 {
		t.Fatalf("got %d messages: %+v", len(lt.Messages), lt.Messages)
	}
	if lt.Messages[0].Role != "assistant" || len(lt.Messages[0].ToolCalls) != 1 {
		t.Fatalf("assistant turn wrong: %+v", lt.Messages[0])
	}
	if lt.Messages[1].Role != "tool" || lt.Messages[1].ToolCallID != "c1" || lt.Messages[1].Content != "a.txt\n" {
		t.Fatalf("tool turn wrong: %+v", lt.Messages[1])
	}
}
