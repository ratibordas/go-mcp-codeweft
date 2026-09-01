package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load([]string{"--project", dir}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectRoot != wantRoot || cfg.Retrieval.MaxTokens != 3500 || cfg.Retrieval.GraphDepth != 2 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Ollama.GenerationModel != "qwen3.6:35b-a3b-q4_K_M" || cfg.Ollama.ContextTokens != 65536 {
		t.Fatalf("unexpected generation defaults: %+v", cfg.Ollama)
	}
	if cfg.Ollama.EmbeddingModel != "qwen3-embedding:0.6b" || cfg.Ollama.EmbeddingDimensions != 1024 {
		t.Fatalf("unexpected embedding defaults: %+v", cfg.Ollama)
	}
}

func TestLoadRejectsRelativeProject(t *testing.T) {
	_, err := Load([]string{"--project", "relative/project"}, func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("expected relative project root to fail")
	}
}

func TestLoadAppliesConfigAndEnvironmentPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("retrieval:\n  max_tokens: 5000\n  graph_depth: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lookup := func(key string) (string, bool) {
		values := map[string]string{
			"CODEWEFT_CLICKHOUSE_DSN": "env-dsn",
			"CODEWEFT_OLLAMA_URL":     "http://environment",
		}
		value, ok := values[key]
		return value, ok
	}
	cfg, err := Load([]string{"--project", dir, "--config", path}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retrieval.MaxTokens != 5000 || cfg.Retrieval.GraphDepth != 1 {
		t.Fatalf("YAML overrides not applied: %+v", cfg.Retrieval)
	}
	if cfg.ClickHouse.DSN != "env-dsn" || cfg.Ollama.BaseURL != "http://environment" {
		t.Fatal("environment connection settings not applied")
	}
}

func TestLoadRejectsConnectionSettingsInYAML(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{
		"clickhouse:\n  dsn: clickhouse://yaml\n",
		"ollama:\n  bearer_token: yaml-token\n",
	} {
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load([]string{"--project", dir, "--config", path}, func(string) (string, bool) { return "", false }); err == nil {
			t.Fatal("expected YAML connection settings to fail")
		}
	}
}

func TestLoadRejectsInvalidBoundsAndMissingProject(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		yaml string
		args []string
	}{
		{name: "max tokens below minimum", yaml: "retrieval:\n  max_tokens: 255\n", args: []string{"--project", dir}},
		{name: "max tokens above maximum", yaml: "retrieval:\n  max_tokens: 12001\n", args: []string{"--project", dir}},
		{name: "graph depth below minimum", yaml: "retrieval:\n  graph_depth: 0\n", args: []string{"--project", dir}},
		{name: "graph depth above maximum", yaml: "retrieval:\n  graph_depth: 3\n", args: []string{"--project", dir}},
		{name: "missing project", args: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if tc.yaml != "" {
				path := filepath.Join(dir, "config.yaml")
				if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(append([]string{}, tc.args...), "--config", path)
			}
			if _, err := Load(args, func(string) (string, bool) { return "", false }); err == nil {
				t.Fatal("expected invalid configuration to fail")
			}
		})
	}
}

func TestLoadCanonicalizesProjectRoot(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg, err := Load([]string{"--project", link}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectRoot != want {
		t.Fatalf("project root = %q, want %q", cfg.ProjectRoot, want)
	}
}
