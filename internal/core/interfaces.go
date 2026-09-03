package core

import "context"

type Embedder interface {
	Embed(context.Context, []string) ([][]float32, error)
}

type Generator interface {
	Generate(context.Context, GenerateRequest) (string, error)
}

// ProgressSink must return promptly when ctx is canceled. Indexing waits for
// cooperative callbacks, but only isolates (and cannot stop) a callback that
// ignores its context.
type ProgressSink func(context.Context, Progress)
