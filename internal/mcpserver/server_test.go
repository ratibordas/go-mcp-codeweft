package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
)

func TestServerRegistersFourTypedTools(t *testing.T) {
	services := &fakeServices{}
	serverSession, clientSession := connectTestServer(t, services, nil)
	defer serverSession.Close()
	defer clientSession.Close()
	listed, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(listed.Tools))
	for index, tool := range listed.Tools {
		names[index] = tool.Name
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Fatalf("tool %q has no inferred schema", tool.Name)
		}
	}
	sort.Strings(names)
	if !slices.Equal(names, []string{"impact_analysis", "index_status", "refresh_index", "search_context"}) {
		t.Fatalf("tools = %v", names)
	}
}

func TestSearchReturnsStructuredOutputAndImpactValidatesInput(t *testing.T) {
	services := &fakeServices{contextResult: core.ContextResult{Summary: "Found [C1]", Freshness: core.Freshness{Generation: 4, State: "ready"}}}
	serverSession, clientSession := connectTestServer(t, services, nil)
	defer serverSession.Close()
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_context", Arguments: map[string]any{"question": "where"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !json.Valid(encoded) || !slices.ContainsFunc(result.Content, func(content mcp.Content) bool {
		text, ok := content.(*mcp.TextContent)
		return ok && text.Text != ""
	}) {
		t.Fatalf("result = %+v", result)
	}
	invalid, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "impact_analysis", Arguments: map[string]any{
		"symbol": "A", "path": "a.go", "direction": "both", "depth": 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !invalid.IsError {
		t.Fatalf("invalid impact result = %+v", invalid)
	}
}

func TestRefreshForwardsProgress(t *testing.T) {
	services := &fakeServices{}
	progress := make(chan *mcp.ProgressNotificationParams, 1)
	options := &mcp.ClientOptions{ProgressNotificationHandler: func(_ context.Context, request *mcp.ProgressNotificationClientRequest) {
		progress <- request.Params
	}}
	serverSession, clientSession := connectTestServer(t, services, options)
	defer serverSession.Close()
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "refresh_index", Arguments: map[string]any{"mode": "full"}, Meta: mcp.Meta{"progressToken": "p1"},
	})
	if err != nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	update := <-progress
	if update.ProgressToken != "p1" || update.Progress != 2 || update.Total != 3 || services.mode != indexer.Full {
		t.Fatalf("progress=%+v mode=%q", update, services.mode)
	}
}

func connectTestServer(t *testing.T, services *fakeServices, options *mcp.ClientOptions) (*mcp.ServerSession, *mcp.ClientSession) {
	t.Helper()
	server := New(Services{Retrieval: services, Index: services, Version: "test"})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, options)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		serverSession.Close()
		t.Fatal(err)
	}
	return serverSession, clientSession
}

type fakeServices struct {
	contextResult core.ContextResult
	mode          indexer.Mode
}

func (f *fakeServices) SearchContext(context.Context, core.SearchRequest, core.ProgressSink) (core.ContextResult, error) {
	return f.contextResult, nil
}

func (*fakeServices) Impact(context.Context, core.ImpactRequest, core.ProgressSink) (core.ImpactResult, error) {
	return core.ImpactResult{}, nil
}

func (f *fakeServices) Sync(ctx context.Context, mode indexer.Mode, sink core.ProgressSink) (core.SyncResult, error) {
	f.mode = mode
	if sink != nil {
		sink(ctx, core.Progress{Phase: "parse", Completed: 2, Total: 3})
	}
	return core.SyncResult{Generation: 5}, nil
}

func (*fakeServices) Status() core.IndexStatus {
	return core.IndexStatus{State: "ready", ActiveGeneration: 5}
}
