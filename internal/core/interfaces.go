package core

import "context"

type Embedder interface {
	Embed(context.Context, []string) ([][]float32, error)
}

type Generator interface {
	Generate(context.Context, GenerateRequest) (string, error)
}

type ProgressSink func(context.Context, Progress)
