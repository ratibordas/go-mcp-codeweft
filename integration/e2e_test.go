//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/app"
	"github.com/ratibordas/go-mcp-codeweft/internal/config"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
)

func TestChangedFileNeverReturnsOldEvidence(t *testing.T) {
	dsn := os.Getenv("CODEWEFT_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("CODEWEFT_TEST_CLICKHOUSE_DSN is not set")
	}
	model := fakeOllama(t)
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "codeweft@example.invalid")
	runGit(t, root, "config", "user.name", "Codeweft Test")
	write(t, root, "docs/api.md", "# API\nUse POST /v1/customers.\n")
	runGit(t, root, "add", "docs/api.md")
	runGit(t, root, "commit", "-m", "fixture")
	cfg := config.Config{
		ProjectRoot: root,
		ClickHouse:  config.ClickHouse{DSN: dsn},
		Ollama: config.Ollama{
			BaseURL: model.URL, GenerationModel: "generation", EmbeddingModel: "embedding",
			ContextTokens: 65536, SynthesisInputTokens: 12000, MaxOutputTokens: 900, EmbeddingDimensions: 1024, EmbeddingBatch: 16,
		},
		Index: config.Index{MaxFileBytes: 2 << 20}, Retrieval: config.Retrieval{MaxTokens: 3500, GraphDepth: 2},
	}
	application, err := app.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = application.Purge(context.Background())
		_ = application.Close()
	})
	if _, err := application.Index.Sync(context.Background(), indexer.Full, nil); err != nil {
		t.Fatal(err)
	}
	write(t, root, "docs/api.md", "# API\nUse POST /v2/customers.\n")
	result, err := application.Retrieval.SearchContext(context.Background(), core.SearchRequest{Question: "customer endpoint"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("/v1/customers")) || !bytes.Contains(encoded, []byte("/v2/customers")) {
		t.Fatalf("stale evidence: %s", encoded)
	}
	if len(result.Evidence) == 0 || result.Evidence[0].StartLine != 1 || result.Evidence[0].EndLine != 2 {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
}

func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	vector := make([]float32, 1024)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/embed":
			var body struct {
				Input []string `json:"input"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			embeddings := make([][]float32, len(body.Input))
			for index := range embeddings {
				embeddings[index] = vector
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"embeddings": embeddings})
		case "/api/generate":
			_ = json.NewEncoder(writer).Encode(map[string]string{"response": "not json"})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func write(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
