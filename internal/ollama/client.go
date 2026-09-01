package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ratibordas/go-mcp-codeweft/internal/config"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

const (
	maxResponseBytes = 16 << 20
	embeddingBatch   = 16
	embeddingSize    = 1024
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
	sem     chan struct{}
	cfg     config.Ollama
}

func New(cfg config.Ollama, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   cfg.BearerToken,
		http:    httpClient,
		sem:     make(chan struct{}, 1),
		cfg:     cfg,
	}
}

func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embeddingBatch {
		end := min(start+embeddingBatch, len(texts))
		var response struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		if err := c.call(ctx, http.MethodPost, "/api/embed", struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}{c.cfg.EmbeddingModel, texts[start:end]}, &response); err != nil {
			return nil, err
		}
		if len(response.Embeddings) != end-start {
			return nil, fmt.Errorf("ollama embed response has %d embeddings for %d inputs", len(response.Embeddings), end-start)
		}
		for _, vector := range response.Embeddings {
			if len(vector) != embeddingSize {
				return nil, fmt.Errorf("ollama embedding dimension is %d, want %d", len(vector), embeddingSize)
			}
		}
		result = append(result, response.Embeddings...)
	}
	return result, nil
}

func (c *Client) Generate(ctx context.Context, req core.GenerateRequest) (string, error) {
	body := struct {
		Model   string          `json:"model"`
		Prompt  string          `json:"prompt"`
		Stream  bool            `json:"stream"`
		Think   bool            `json:"think"`
		Options generateOptions `json:"options"`
		Format  json.RawMessage `json:"format,omitempty"`
	}{
		Model:   c.cfg.GenerationModel,
		Prompt:  req.Prompt,
		Stream:  false,
		Think:   false,
		Options: generateOptions{Context: 65536, Predict: 900},
		Format:  req.Schema,
	}
	var response struct {
		Response string `json:"response"`
	}
	if err := c.call(ctx, http.MethodPost, "/api/generate", body, &response); err != nil {
		return "", err
	}
	return response.Response, nil
}

type generateOptions struct {
	Context int `json:"num_ctx"`
	Predict int `json:"num_predict"`
}

func (c *Client) Health(ctx context.Context) core.ModelHealth {
	var response struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/tags", nil, &response); err != nil {
		return core.ModelHealth{Warnings: []string{fmt.Sprintf("Ollama health check failed: %v", err)}}
	}
	health := core.ModelHealth{}
	for _, model := range response.Models {
		health.GenerationAvailable = health.GenerationAvailable || model.Name == c.cfg.GenerationModel
		health.EmbeddingAvailable = health.EmbeddingAvailable || model.Name == c.cfg.EmbeddingModel
	}
	if !health.GenerationAvailable {
		health.Warnings = append(health.Warnings, "Ollama generation model is unavailable")
	}
	if !health.EmbeddingAvailable {
		health.Warnings = append(health.Warnings, "Ollama embedding model is unavailable")
	}
	return health
}

func (c *Client) call(ctx context.Context, method, path string, requestBody, responseBody any) error {
	return c.locked(ctx, func() error {
		var body io.Reader
		if requestBody != nil {
			data, err := json.Marshal(requestBody)
			if err != nil {
				return fmt.Errorf("encode Ollama request: %w", err)
			}
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
		if err != nil {
			return fmt.Errorf("create Ollama request: %w", err)
		}
		if requestBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		response, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("send Ollama request: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes+1)); err != nil {
				return fmt.Errorf("read Ollama error response: %w", err)
			}
			return fmt.Errorf("Ollama request failed with status %d", response.StatusCode)
		}
		data, err := readResponse(response.Body)
		if err != nil {
			return fmt.Errorf("read Ollama response: %w", err)
		}
		if err := json.Unmarshal(data, responseBody); err != nil {
			return fmt.Errorf("decode Ollama response: %w", err)
		}
		return nil
	})
}

func (c *Client) locked(ctx context.Context, fn func() error) error {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readResponse(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxResponseBytes)
	}
	return data, nil
}
