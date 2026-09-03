package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/ratibordas/go-mcp-codeweft/internal/app"
	"github.com/ratibordas/go-mcp-codeweft/internal/benchmark"
	"github.com/ratibordas/go-mcp-codeweft/internal/config"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
	"github.com/ratibordas/go-mcp-codeweft/internal/mcpserver"
)

const version = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		slog.Error("codeweft failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" {
		return nil
	}
	command := args[0]
	if command != "serve" && command != "index" && command != "search" && command != "status" && command != "benchmark" && command != "purge" {
		return fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	projectRoot := flags.String("project", "", "project root")
	configPath := flags.String("config", "", "configuration file")
	full := flags.Bool("full", false, "full index")
	question := flags.String("question", "", "search question")
	maxTokens := flags.Int("max-tokens", 0, "maximum context tokens")
	suite := flags.String("suite", "", "benchmark suite")
	yes := flags.Bool("yes", false, "confirm purge")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if command == "search" && *question == "" {
		return errors.New("--question is required")
	}
	if command == "benchmark" && *suite == "" {
		return errors.New("--suite is required")
	}
	if command == "purge" && !*yes {
		return errors.New("purge requires --yes")
	}
	configArgs := []string{"--project", *projectRoot}
	if *configPath != "" {
		configArgs = append(configArgs, "--config", *configPath)
	}
	cfg, err := config.Load(configArgs, os.LookupEnv)
	if err != nil {
		return err
	}
	var benchmarkSuite benchmark.Suite
	if command == "benchmark" {
		benchmarkSuite, err = benchmark.LoadSuite(*suite)
		if err != nil {
			return err
		}
	}
	application, err := app.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer application.Close()
	switch command {
	case "serve":
		application.StartInitial(ctx)
		return mcpserver.Run(ctx, mcpserver.Services{Retrieval: application.Retrieval, Index: application.Index, Version: version})
	case "index":
		mode := indexer.Delta
		if *full {
			mode = indexer.Full
		}
		result, err := application.Index.Sync(ctx, mode, cliProgress)
		if err != nil {
			return err
		}
		return printJSON(os.Stdout, result)
	case "search":
		result, err := application.Retrieval.SearchContext(ctx, core.SearchRequest{Question: *question, MaxTokens: *maxTokens}, cliProgress)
		if err != nil {
			return err
		}
		return printJSON(os.Stdout, result)
	case "status":
		return printJSON(os.Stdout, application.Index.Status())
	case "benchmark":
		report, err := benchmark.Run(ctx, cfg.ProjectRoot, benchmarkSuite, benchmark.Services{Index: application.Index, Retrieval: application.Retrieval})
		if err != nil {
			return err
		}
		return printJSON(os.Stdout, report)
	case "purge":
		fmt.Fprintf(os.Stdout, "purging project %s (%s)\n", application.ProjectID, cfg.ProjectRoot)
		return application.Purge(ctx)
	}
	return nil
}

func cliProgress(_ context.Context, progress core.Progress) {
	slog.Info(progress.Message())
}

func printJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
