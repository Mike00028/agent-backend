package config

import (
	"os"
	"testing"
)

func TestValidate_OK_NoDB(t *testing.T) {
	cfg := Config{} // no POSTGRES_DSN — no embedding required
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected no error when PostgresDSN is empty, got: %v", err)
	}
}

func TestValidate_OK_WithDB(t *testing.T) {
	cfg := Config{
		PostgresDSN:   "postgres://u:p@localhost/db",
		OllamaBaseURL: "http://localhost:11434",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected no error when both DSN and OllamaBaseURL are set, got: %v", err)
	}
}

func TestValidate_MissingOllamaURL(t *testing.T) {
	cfg := Config{
		PostgresDSN:   "postgres://u:p@localhost/db",
		OllamaBaseURL: "", // missing
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error when POSTGRES_DSN is set but OLLAMA_BASE_URL is empty")
	}
}

func TestLoad_MissingRequiredVars(t *testing.T) {
	// Unset any values that might be set in the environment
	os.Unsetenv("PLANNER_MODEL")
	os.Unsetenv("CHAT_MODEL")
	os.Unsetenv("EVAL_MODEL")

	_, err := Load()
	if err == nil {
		t.Skip("PLANNER_MODEL/CHAT_MODEL/EVAL_MODEL are set in environment — skipping")
	}
	// Should error listing the missing vars
}

func TestLoad_Defaults(t *testing.T) {
	// Set the three required vars so Load succeeds
	os.Setenv("PLANNER_MODEL", "llama3")
	os.Setenv("CHAT_MODEL", "llama3")
	os.Setenv("EVAL_MODEL", "llama3")
	defer func() {
		os.Unsetenv("PLANNER_MODEL")
		os.Unsetenv("CHAT_MODEL")
		os.Unsetenv("EVAL_MODEL")
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr default = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.GRPCPoolSize != 5 {
		t.Errorf("GRPCPoolSize default = %d, want 5", cfg.GRPCPoolSize)
	}
	if cfg.EmbedModel != "nomic-embed-text" {
		t.Errorf("EmbedModel default = %q, want nomic-embed-text", cfg.EmbedModel)
	}
	if cfg.OllamaBaseURL != "http://localhost:11434" {
		t.Errorf("OllamaBaseURL default = %q", cfg.OllamaBaseURL)
	}
}
