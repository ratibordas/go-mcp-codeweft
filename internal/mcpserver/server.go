package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = "Use search_context before broad repository scanning. Use impact_analysis before changing public symbols or shared modules. Inspect files directly when Codeweft reports incomplete evidence."

func New(services Services) *mcp.Server {
	version := services.Version
	if version == "" {
		version = "dev"
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "codeweft", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	registerTools(server, services)
	return server
}

func Run(ctx context.Context, services Services) error {
	return New(services).Run(ctx, &mcp.StdioTransport{})
}
