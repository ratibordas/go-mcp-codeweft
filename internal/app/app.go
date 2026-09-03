package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"sync"

	"github.com/ratibordas/go-mcp-codeweft/internal/config"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
	"github.com/ratibordas/go-mcp-codeweft/internal/ollama"
	"github.com/ratibordas/go-mcp-codeweft/internal/project"
	"github.com/ratibordas/go-mcp-codeweft/internal/retrieval"
	"github.com/ratibordas/go-mcp-codeweft/internal/store"
)

type App struct {
	ProjectID   string
	Store       *store.Store
	Index       *indexer.Indexer
	Retrieval   *retrieval.Service
	InitialMode indexer.Mode

	start  sync.Once
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func Open(ctx context.Context, cfg config.Config) (*App, error) {
	projectID, err := ProjectID(cfg.ProjectRoot)
	if err != nil {
		return nil, err
	}
	dsn, err := clickHouseDSN(cfg.ClickHouse)
	if err != nil {
		return nil, err
	}
	database, err := store.New(dsn)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*App, error) {
		_ = database.Close()
		return nil, err
	}
	if err := database.Migrate(ctx); err != nil {
		return closeOnError(err)
	}
	models := ollama.New(cfg.Ollama, nil)
	idx := indexer.New(indexer.Config{
		ProjectID: projectID, Root: cfg.ProjectRoot, Index: cfg.Index,
		Store: indexer.NewStoreAdapter(database), Embedder: models,
	})
	if err := idx.Initialize(ctx); err != nil {
		return closeOnError(err)
	}
	retriever := retrieval.New(retrieval.Config{
		ProjectID: projectID, Root: cfg.ProjectRoot, Freshener: idx, Store: database,
		Embedder: models, Generator: models, GraphDepth: cfg.Retrieval.GraphDepth, MaxTokens: cfg.Retrieval.MaxTokens,
	})
	return &App{ProjectID: projectID, Store: database, Index: idx, Retrieval: retriever, InitialMode: initialMode(idx.Manifest())}, nil
}

func (a *App) StartInitial(ctx context.Context) {
	if a == nil || a.Index == nil {
		return
	}
	a.start.Do(func() {
		ctx, a.cancel = context.WithCancel(ctx)
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			if _, err := a.Index.Sync(ctx, a.InitialMode, nil); err != nil && ctx.Err() == nil {
				slog.Error("initial indexing failed", "error", err)
			}
		}()
	})
}

func (a *App) Purge(ctx context.Context) error {
	if a == nil || a.Store == nil {
		return fmt.Errorf("application is not open")
	}
	return a.Store.Purge(ctx, a.ProjectID)
}

func (a *App) Close() error {
	if a == nil || a.Store == nil {
		return nil
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	return a.Store.Close()
}

func ProjectID(root string) (string, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("canonicalize project root: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("canonicalize project root: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(root))), nil
}

func initialMode(manifest map[string]core.FileState) indexer.Mode {
	if len(manifest) == 0 {
		return indexer.Full
	}
	for _, file := range manifest {
		if file.ParserVersion != project.ParserVersion {
			return indexer.Full
		}
	}
	return indexer.Delta
}

func clickHouseDSN(cfg config.ClickHouse) (string, error) {
	parsed, err := url.Parse(cfg.DSN)
	if err != nil {
		return "", fmt.Errorf("parse ClickHouse DSN: %w", err)
	}
	if cfg.User != "" || cfg.Password != "" {
		if cfg.Password == "" {
			parsed.User = url.User(cfg.User)
		} else {
			parsed.User = url.UserPassword(cfg.User, cfg.Password)
		}
	}
	return parsed.String(), nil
}
