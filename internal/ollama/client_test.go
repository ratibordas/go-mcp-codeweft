package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ratibordas/go-mcp-codeweft/internal/config"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func TestEmbedUsesNativeEndpointAndBatchesInOrder(t *testing.T) {
	var inputs [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Fatalf("path = %q, want /api/embed", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "embed-model" {
			t.Fatalf("model = %q", body.Model)
		}
		inputs = append(inputs, body.Input)
		embeddings := make([][]float32, len(body.Input))
		for i, input := range body.Input {
			embeddings[i] = vector(float32(len(input)))
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings}); err != nil {
			t.Fatal(err)
		}
	}))
	defer srv.Close()

	texts := make([]string, 17)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	got, err := New(testConfig(srv.URL+"/v1"), srv.Client()).Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || len(inputs[0]) != 16 || len(inputs[1]) != 1 {
		t.Fatalf("batches = %#v", inputs)
	}
	if len(got) != len(texts) || got[0][0] != 1 || got[16][0] != 17 {
		t.Fatalf("embeddings not in input order")
	}
}

func TestEmbedRejectsWrongVectorDimension(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1}}})
	}))
	defer srv.Close()

	_, err := New(testConfig(srv.URL), srv.Client()).Embed(context.Background(), []string{"text"})
	if err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatalf("error = %v, want dimension validation", err)
	}
}

func TestEmbedWithNoInputsMakesNoRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()

	got, err := New(testConfig(srv.URL), srv.Client()).Embed(context.Background(), nil)
	if err != nil || len(got) != 0 || requests != 0 {
		t.Fatalf("embeddings = %#v, error = %v, requests = %d", got, err, requests)
	}
}

func TestEmbedRejectsMissingEmbeddingWithoutPartialResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vector(1)}})
	}))
	defer srv.Close()

	got, err := New(testConfig(srv.URL), srv.Client()).Embed(context.Background(), []string{"one", "two"})
	if err == nil || got != nil || !strings.Contains(err.Error(), "2 inputs") {
		t.Fatalf("embeddings = %#v, error = %v", got, err)
	}
}

func TestGenerateDisablesThinkingAndStreaming(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("path = %q, want /api/generate", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"response": `{"summary":"ok","citations":["C1"]}`})
	}))
	defer srv.Close()

	got, err := New(testConfig(srv.URL), srv.Client()).Generate(context.Background(), core.GenerateRequest{Prompt: "question"})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"summary":"ok","citations":["C1"]}` {
		t.Fatalf("response = %q", got)
	}
	if body["model"] != "generation-model" || body["prompt"] != "question" || body["think"] != false || body["stream"] != false {
		t.Fatalf("unsafe generation body: %#v", body)
	}
	options, ok := body["options"].(map[string]any)
	if !ok || options["num_ctx"] != float64(65536) || options["num_predict"] != float64(900) {
		t.Fatalf("options = %#v", body["options"])
	}
	if _, ok := body["format"]; ok {
		t.Fatalf("format supplied without schema: %#v", body["format"])
	}
}

func TestGenerateSendsSchemaFormat(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		format, ok := body["format"].(map[string]any)
		if !ok || format["type"] != "object" {
			t.Fatalf("format = %#v", body["format"])
		}
		json.NewEncoder(w).Encode(map[string]any{"response": "{}"})
	}))
	defer srv.Close()

	if _, err := New(testConfig(srv.URL), srv.Client()).Generate(context.Background(), core.GenerateRequest{Schema: schema}); err != nil {
		t.Fatal(err)
	}
}

func TestRequestsRespectHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"response": "{}"})
	}))
	defer srv.Close()

	httpClient := srv.Client()
	httpClient.Timeout = 10 * time.Millisecond
	_, err := New(testConfig(srv.URL), httpClient).Generate(context.Background(), core.GenerateRequest{})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error = %v, want client timeout", err)
	}
}

func TestWaitingForSerializedRequestRespectsContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		json.NewEncoder(w).Encode(map[string]any{"response": "{}"})
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL), srv.Client())
	done := make(chan error, 1)
	go func() { _, err := c.Generate(context.Background(), core.GenerateRequest{}); done <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.Embed(ctx, []string{"text"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestResponseLimitAndErrorDoNotExposeToken(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 2 {
			http.Error(w, "model unavailable: secret", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"response":"%s"}`, strings.Repeat("x", 16<<20))
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL), srv.Client())
	_, err := c.Generate(context.Background(), core.GenerateRequest{})
	if err == nil || !strings.Contains(err.Error(), "response body") {
		t.Fatalf("oversized response error = %v", err)
	}
	_, err = c.Generate(context.Background(), core.GenerateRequest{})
	if err == nil || !strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("status error = %v", err)
	}
}

func TestNon2xxErrorDoesNotExposeResponseContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "prompt and project evidence must stay private", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := New(testConfig(srv.URL), srv.Client()).Generate(context.Background(), core.GenerateRequest{Prompt: "private prompt"})
	if err == nil || !strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "private") {
		t.Fatalf("error = %v", err)
	}
}

func TestReadResponseHonorsExactLimit(t *testing.T) {
	data, err := readResponse(bytes.NewReader(make([]byte, maxResponseBytes)))
	if err != nil || len(data) != maxResponseBytes {
		t.Fatalf("len = %d, error = %v", len(data), err)
	}
	_, err = readResponse(bytes.NewReader(make([]byte, maxResponseBytes+1)))
	if err == nil {
		t.Fatal("expected oversized response error")
	}
}

func TestEmbedAndGenerateNeverOverlap(t *testing.T) {
	var mu sync.Mutex
	active, maximum := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		mu.Unlock()
		time.Sleep(25 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		if r.URL.Path == "/api/embed" {
			json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vector(1)}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"response": "{}"})
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL), srv.Client())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = c.Embed(context.Background(), []string{"text"}) }()
	go func() { defer wg.Done(); _, _ = c.Generate(context.Background(), core.GenerateRequest{}) }()
	wg.Wait()
	if maximum != 1 {
		t.Fatalf("maximum concurrent requests = %d, want 1", maximum)
	}
}

func TestHealthReportsConfiguredModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Fatalf("path = %q, want /api/tags", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "generation-model"}}})
	}))
	defer srv.Close()

	health := New(testConfig(srv.URL), srv.Client()).Health(context.Background())
	if !health.GenerationAvailable || health.EmbeddingAvailable || len(health.Warnings) != 1 {
		t.Fatalf("health = %#v", health)
	}
}

func testConfig(baseURL string) config.Ollama {
	return config.Ollama{
		BaseURL:             baseURL,
		BearerToken:         "secret",
		GenerationModel:     "generation-model",
		EmbeddingModel:      "embed-model",
		ContextTokens:       65536,
		MaxOutputTokens:     900,
		EmbeddingDimensions: 1024,
		EmbeddingBatch:      16,
	}
}

func vector(first float32) []float32 {
	v := make([]float32, 1024)
	v[0] = first
	return v
}
