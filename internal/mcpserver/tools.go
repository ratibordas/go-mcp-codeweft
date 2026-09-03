package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
)

type RetrievalService interface {
	SearchContext(context.Context, core.SearchRequest, core.ProgressSink) (core.ContextResult, error)
	Impact(context.Context, core.ImpactRequest, core.ProgressSink) (core.ImpactResult, error)
}

type IndexService interface {
	Sync(context.Context, indexer.Mode, core.ProgressSink) (core.SyncResult, error)
	Status() core.IndexStatus
}

type Services struct {
	Retrieval RetrievalService
	Index     IndexService
	Version   string
}

type searchInput struct {
	Question  string   `json:"question" jsonschema:"question about the current project"`
	Paths     []string `json:"paths,omitempty" jsonschema:"optional project-relative path scopes"`
	MaxTokens int      `json:"max_tokens,omitempty" jsonschema:"maximum context tokens from 256 to 12000"`
}

type impactInput struct {
	Symbol    string `json:"symbol,omitempty" jsonschema:"exact code symbol"`
	Path      string `json:"path,omitempty" jsonschema:"project-relative code path"`
	Direction string `json:"direction" jsonschema:"upstream, downstream, or both"`
	Depth     int    `json:"depth" jsonschema:"graph depth from 1 to 2"`
}

type refreshInput struct {
	Mode string `json:"mode" jsonschema:"delta or full"`
}

type statusInput struct{}

func registerTools(server *mcp.Server, services Services) {
	mcp.AddTool(server, &mcp.Tool{Name: "search_context", Description: "Return current compact project context with exact cited evidence."},
		func(ctx context.Context, request *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, core.ContextResult, error) {
			if services.Retrieval == nil {
				return nil, core.ContextResult{}, errors.New("retrieval service is unavailable")
			}
			output, err := services.Retrieval.SearchContext(ctx, core.SearchRequest{Question: input.Question, Paths: input.Paths, MaxTokens: input.MaxTokens}, progressSink(request))
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "impact_analysis", Description: "Return deterministic upstream or downstream code impact."},
		func(ctx context.Context, request *mcp.CallToolRequest, input impactInput) (*mcp.CallToolResult, core.ImpactResult, error) {
			if services.Retrieval == nil {
				return nil, core.ImpactResult{}, errors.New("retrieval service is unavailable")
			}
			if (input.Symbol == "") == (input.Path == "") {
				return nil, core.ImpactResult{}, errors.New("exactly one of symbol and path is required")
			}
			if input.Direction != "upstream" && input.Direction != "downstream" && input.Direction != "both" {
				return nil, core.ImpactResult{}, fmt.Errorf("unknown graph direction %q", input.Direction)
			}
			if input.Depth < 1 || input.Depth > 2 {
				return nil, core.ImpactResult{}, errors.New("graph depth must be 1 or 2")
			}
			output, err := services.Retrieval.Impact(ctx, core.ImpactRequest{Symbol: input.Symbol, Path: input.Path, Direction: input.Direction, Depth: input.Depth}, progressSink(request))
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "refresh_index", Description: "Refresh the project index in delta or full mode."},
		func(ctx context.Context, request *mcp.CallToolRequest, input refreshInput) (*mcp.CallToolResult, core.SyncResult, error) {
			if services.Index == nil {
				return nil, core.SyncResult{}, errors.New("index service is unavailable")
			}
			mode := indexer.Mode(input.Mode)
			if mode != indexer.Delta && mode != indexer.Full {
				return nil, core.SyncResult{}, fmt.Errorf("unknown index mode %q", input.Mode)
			}
			output, err := services.Index.Sync(ctx, mode, progressSink(request))
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "index_status", Description: "Return index state, progress, throughput, and ETA."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ statusInput) (*mcp.CallToolResult, core.IndexStatus, error) {
			if services.Index == nil {
				return nil, core.IndexStatus{}, errors.New("index service is unavailable")
			}
			return nil, services.Index.Status(), nil
		})
}

func progressSink(request *mcp.CallToolRequest) core.ProgressSink {
	if request == nil || request.Params == nil {
		return nil
	}
	token := request.Params.GetProgressToken()
	if token == nil {
		return nil
	}
	return func(ctx context.Context, progress core.Progress) {
		_ = request.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token, Progress: float64(progress.Completed), Total: float64(progress.Total), Message: progress.Message(),
		})
	}
}
