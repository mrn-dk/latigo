package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withEnv sets env vars for the duration of t and restores them after.
func withEnv(t *testing.T, env map[string]string) {
	t.Helper()
	saved := map[string]string{}
	for k, v := range env {
		saved[k] = os.Getenv(k)
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
}

func clearPromptEnv(t *testing.T) {
	for _, k := range []string{EnvSystemPrompt, EnvSystemPromptFile, EnvAppendSystemPrompt, EnvAppendSystemPromptFile, EnvGoal, EnvResume} {
		os.Unsetenv(k)
	}
}

func TestLoadConfigDefaultPrompt(t *testing.T) {
	clearPromptEnv(t)
	withEnv(t, map[string]string{EnvGoal: "g"})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SystemPrompt != "" {
		t.Fatalf("SystemPrompt=%q want empty (default)", cfg.SystemPrompt)
	}
	if cfg.promptSource() != "default" {
		t.Fatalf("source=%q want default", cfg.promptSource())
	}
}

func TestLoadConfigInlineOverride(t *testing.T) {
	clearPromptEnv(t)
	withEnv(t, map[string]string{EnvGoal: "g", EnvSystemPrompt: "custom prompt"})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SystemPrompt != "custom prompt" {
		t.Fatalf("SystemPrompt=%q", cfg.SystemPrompt)
	}
	if cfg.promptSource() != "override" {
		t.Fatalf("source=%q want override", cfg.promptSource())
	}
}

func TestLoadConfigFileOverride(t *testing.T) {
	clearPromptEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "prompt.txt")
	os.WriteFile(p, []byte("file prompt"), 0o644)
	withEnv(t, map[string]string{EnvGoal: "g", EnvSystemPromptFile: p})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SystemPrompt != "file prompt" {
		t.Fatalf("SystemPrompt=%q", cfg.SystemPrompt)
	}
	if cfg.promptSource() != "override" {
		t.Fatalf("source=%q want override", cfg.promptSource())
	}
}

func TestLoadConfigInlineOverrideBeatsFile(t *testing.T) {
	clearPromptEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "prompt.txt")
	os.WriteFile(p, []byte("file prompt"), 0o644)
	withEnv(t, map[string]string{
		EnvGoal:             "g",
		EnvSystemPrompt:     "inline wins",
		EnvSystemPromptFile: p,
	})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SystemPrompt != "inline wins" {
		t.Fatalf("SystemPrompt=%q want inline wins", cfg.SystemPrompt)
	}
}

func TestLoadConfigEmptyInlineFallsBackToFile(t *testing.T) {
	clearPromptEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "prompt.txt")
	os.WriteFile(p, []byte("file prompt"), 0o644)
	withEnv(t, map[string]string{
		EnvGoal:             "g",
		EnvSystemPrompt:     "", // empty inline is unset
		EnvSystemPromptFile: p,
	})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SystemPrompt != "file prompt" {
		t.Fatalf("SystemPrompt=%q want file prompt", cfg.SystemPrompt)
	}
}

func TestLoadConfigAppend(t *testing.T) {
	clearPromptEnv(t)
	withEnv(t, map[string]string{EnvGoal: "g", EnvAppendSystemPrompt: "extra rules"})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppendSystemPrompt != "extra rules" {
		t.Fatalf("AppendSystemPrompt=%q", cfg.AppendSystemPrompt)
	}
	if cfg.promptSource() != "default+append" {
		t.Fatalf("source=%q want default+append", cfg.promptSource())
	}
}

func TestLoadConfigOverrideAndAppend(t *testing.T) {
	clearPromptEnv(t)
	withEnv(t, map[string]string{
		EnvGoal:               "g",
		EnvSystemPrompt:       "custom",
		EnvAppendSystemPrompt: "extra",
	})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.promptSource() != "override+append" {
		t.Fatalf("source=%q want override+append", cfg.promptSource())
	}
}

func TestLoadConfigMissingOverrideFileFails(t *testing.T) {
	clearPromptEnv(t)
	withEnv(t, map[string]string{
		EnvGoal:             "g",
		EnvSystemPromptFile: "/no/such/path/prompt.txt",
	})
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing override file")
	}
	if !contains(err.Error(), "/no/such/path/prompt.txt") {
		t.Fatalf("error should name the path: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(sub) == 0) || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
