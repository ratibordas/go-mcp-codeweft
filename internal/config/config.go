package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ProjectRoot string     `yaml:"-"`
	ClickHouse  ClickHouse `yaml:"clickhouse"`
	Ollama      Ollama     `yaml:"ollama"`
	Index       Index      `yaml:"index"`
	Retrieval   Retrieval  `yaml:"retrieval"`
}

type ClickHouse struct {
	DSN      string `yaml:"dsn"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

type Ollama struct {
	BaseURL              string `yaml:"base_url"`
	BearerToken          string `yaml:"bearer_token"`
	GenerationModel      string `yaml:"generation_model"`
	EmbeddingModel       string `yaml:"embedding_model"`
	ContextTokens        int    `yaml:"context_tokens"`
	SynthesisInputTokens int    `yaml:"synthesis_input_tokens"`
	MaxOutputTokens      int    `yaml:"max_output_tokens"`
	EmbeddingDimensions  int    `yaml:"embedding_dimensions"`
	EmbeddingBatch       int    `yaml:"embedding_batch"`
}

type Index struct {
	MaxFileBytes     int64    `yaml:"max_file_bytes"`
	IncludeTests     bool     `yaml:"include_tests"`
	ExcludePaths     []string `yaml:"exclude_paths"`
	ExcludeDirNames  []string `yaml:"exclude_dir_names"`
	ExcludeFileNames []string `yaml:"exclude_file_names"`
}

type Retrieval struct {
	MaxTokens  int `yaml:"max_tokens"`
	GraphDepth int `yaml:"graph_depth"`
}

type yamlConfig struct {
	Index     Index     `yaml:"index"`
	Retrieval Retrieval `yaml:"retrieval"`
}

func Load(args []string, lookup func(string) (string, bool)) (Config, error) {
	flags := flag.NewFlagSet("codeweft", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", "", "project root")
	configPath := flags.String("config", "", "configuration file")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if *project == "" {
		return Config{}, errors.New("--project is required")
	}
	if !filepath.IsAbs(*project) {
		return Config{}, errors.New("project root must be absolute")
	}
	root, err := filepath.EvalSymlinks(*project)
	if err != nil {
		return Config{}, fmt.Errorf("canonicalize project root: %w", err)
	}

	cfg := defaults(root)
	path := *configPath
	if path == "" {
		path = filepath.Join(root, ".codeweft.yaml")
	} else if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return Config{}, fmt.Errorf("get working directory: %w", err)
		}
		path = filepath.Join(cwd, path)
	}
	if err := loadYAML(path, &cfg, *configPath != ""); err != nil {
		return Config{}, err
	}
	applyEnvironment(&cfg, lookup)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.ProjectRoot) {
		return errors.New("project root must be absolute")
	}
	if c.Retrieval.MaxTokens < 256 || c.Retrieval.MaxTokens > 12000 {
		return errors.New("retrieval max_tokens must be between 256 and 12000")
	}
	if c.Retrieval.GraphDepth < 1 || c.Retrieval.GraphDepth > 2 {
		return errors.New("graph depth must be 1 or 2")
	}
	if c.Ollama.EmbeddingDimensions != 1024 {
		return errors.New("embedding dimensions must be 1024")
	}
	return nil
}

func defaults(root string) Config {
	return Config{
		ProjectRoot: root,
		ClickHouse:  ClickHouse{DSN: "clickhouse://localhost:9000/codeweft", User: "default"},
		Ollama: Ollama{
			BaseURL: "http://localhost:11434", GenerationModel: "qwen3.6:35b-a3b-q4_K_M", EmbeddingModel: "qwen3-embedding:0.6b",
			ContextTokens: 65536, SynthesisInputTokens: 12000, MaxOutputTokens: 900, EmbeddingDimensions: 1024, EmbeddingBatch: 16,
		},
		Index:     Index{MaxFileBytes: 2097152},
		Retrieval: Retrieval{MaxTokens: 3500, GraphDepth: 2},
	}
}

func loadYAML(path string, cfg *Config, required bool) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()
	policy := yamlConfig{Index: cfg.Index, Retrieval: cfg.Retrieval}
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&policy); err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	cfg.Index = policy.Index
	cfg.Retrieval = policy.Retrieval
	return nil
}

func applyEnvironment(cfg *Config, lookup func(string) (string, bool)) {
	if value, ok := lookup("CODEWEFT_CLICKHOUSE_DSN"); ok {
		cfg.ClickHouse.DSN = value
	}
	if value, ok := lookup("CODEWEFT_CLICKHOUSE_USER"); ok {
		cfg.ClickHouse.User = value
	}
	if value, ok := lookup("CODEWEFT_CLICKHOUSE_PASSWORD"); ok {
		cfg.ClickHouse.Password = value
	}
	if value, ok := lookup("CODEWEFT_OLLAMA_URL"); ok {
		cfg.Ollama.BaseURL = value
	}
	if value, ok := lookup("CODEWEFT_OLLAMA_TOKEN"); ok {
		cfg.Ollama.BearerToken = value
	}
}
