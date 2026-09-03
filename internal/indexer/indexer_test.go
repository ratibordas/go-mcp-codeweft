package indexer_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/goparser"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
	"github.com/ratibordas/go-mcp-codeweft/internal/project"
	"github.com/ratibordas/go-mcp-codeweft/internal/testutil"
	"github.com/ratibordas/go-mcp-codeweft/internal/tsparser"
)

func TestSyncEmitsEveryPhaseInOrder(t *testing.T) {
	deps := newDeps(file("a.md", "hello"))
	idx := indexer.New(deps.config())
	phases := []string{}
	completed := make(chan struct{})
	_, err := idx.Sync(context.Background(), indexer.Delta, func(_ context.Context, progress core.Progress) {
		if len(phases) == 0 || phases[len(phases)-1] != progress.Phase {
			phases = append(phases, progress.Phase)
			if progress.Phase == "persist" {
				close(completed)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("progress sink did not receive terminal phase")
	}
	want := []string{"scan", "parse", "graph", "embed", "persist"}
	if !slices.Equal(phases, want) {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
}

func TestConcurrentWaitersShareWorkAndCanCancel(t *testing.T) {
	deps := newDeps(file("a.md", "hello"))
	deps.planner.block = make(chan struct{})
	idx := indexer.New(deps.config())
	firstCtx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := idx.Sync(firstCtx, indexer.Delta, nil); first <- err }()
	<-deps.planner.started
	joined := make(chan struct{})
	secondCtx, secondCancel := context.WithTimeout(context.Background(), time.Second)
	defer secondCancel()
	second := make(chan error, 1)
	go func() {
		_, err := idx.Sync(secondCtx, indexer.Delta, func(context.Context, core.Progress) {
			select {
			case joined <- struct{}{}:
			default:
			}
		})
		second <- err
	}()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("second caller did not join the active sync")
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter error = %v", err)
	}
	close(deps.planner.block)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if deps.planner.calls != 1 {
		t.Fatalf("planner calls = %d", deps.planner.calls)
	}
}

func TestFullSupersedesQueuedDeltaWithoutInterruptingActiveRun(t *testing.T) {
	deps := newDeps(file("a.md", "hello"))
	deps.planner.block = make(chan struct{})
	idx := indexer.New(deps.config())
	active := make(chan error, 1)
	go func() { _, err := idx.Sync(context.Background(), indexer.Full, nil); active <- err }()
	<-deps.planner.started
	delta := make(chan error, 1)
	go func() { _, err := idx.Sync(context.Background(), indexer.Delta, nil); delta <- err }()
	time.Sleep(20 * time.Millisecond)
	full := make(chan error, 1)
	go func() { _, err := idx.Sync(context.Background(), indexer.Full, nil); full <- err }()
	time.Sleep(20 * time.Millisecond)
	close(deps.planner.block)
	for _, result := range []<-chan error{active, delta, full} {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	deps.planner.mu.Lock()
	modes := append([]indexer.Mode(nil), deps.planner.modes...)
	deps.planner.mu.Unlock()
	if !slices.Equal(modes, []indexer.Mode{indexer.Full, indexer.Full}) {
		t.Fatalf("planned modes = %v", modes)
	}
}

func TestQueuedWaiterDoesNotReceiveActiveRunProgress(t *testing.T) {
	deps := newDeps(file("a.md", "hello"))
	deps.markdown.started = make(chan struct{}, 1)
	deps.markdown.block = make(chan struct{})
	idx := indexer.New(deps.config())
	active := make(chan error, 1)
	go func() { _, err := idx.Sync(context.Background(), indexer.Delta, nil); active <- err }()
	<-deps.markdown.started

	progress := make(chan core.Progress, 1)
	queued := make(chan error, 1)
	go func() {
		_, err := idx.Sync(context.Background(), indexer.Full, func(_ context.Context, update core.Progress) {
			select {
			case progress <- update:
			default:
			}
		})
		queued <- err
	}()
	select {
	case update := <-progress:
		t.Fatalf("queued waiter received active progress: %+v", update)
	case <-time.After(50 * time.Millisecond):
	}

	close(deps.markdown.block)
	if err := <-active; err != nil {
		t.Fatal(err)
	}
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddingsReuseHashesAndBatchMissingChunksInOrder(t *testing.T) {
	files := []project.File{}
	for i := 0; i < 18; i++ {
		files = append(files, file(fmt.Sprintf("%02d.md", i), fmt.Sprintf("chunk-%02d", i)))
	}
	deps := newDeps(files...)
	deps.store.Embeddings[hash("chunk-00")] = vector(1)
	idx := indexer.New(deps.config())
	if _, err := idx.Sync(context.Background(), indexer.Delta, nil); err != nil {
		t.Fatal(err)
	}
	if got := deps.embedder.batchSizes(); !slices.Equal(got, []int{16, 1}) {
		t.Fatalf("embedding batches = %v", got)
	}
	if deps.embedder.inputs[0][0] != "chunk-01" || deps.embedder.inputs[1][0] != "chunk-17" {
		t.Fatalf("embedding order = %v", deps.embedder.inputs)
	}
}

func TestEmbeddingFailureActivatesDocumentsWithoutNewEmbeddings(t *testing.T) {
	deps := newDeps(file("a.md", "hello"))
	deps.embedder.err = errors.New("offline")
	idx := indexer.New(deps.config())
	result, err := idx.Sync(context.Background(), indexer.Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !deps.store.WasActivated("a.md", hash("hello")) || len(result.Warnings) == 0 {
		t.Fatalf("degraded result = %+v", result)
	}
	if got := deps.store.Derived[0].Chunks[0].Embedding; len(got) != 0 {
		t.Fatalf("unexpected embedding length %d", len(got))
	}
	if idx.Status().State != "degraded" {
		t.Fatalf("state = %q", idx.Status().State)
	}
}

func TestFileChangedDuringParseIsNotActivated(t *testing.T) {
	deps := newDeps(file("a.md", "first"))
	deps.markChangedDuringParse("a.md", "second")
	idx := indexer.New(deps.config())
	result, err := idx.Sync(context.Background(), indexer.Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if deps.store.WasActivated("a.md", hash("first")) {
		t.Fatal("raced file hash was activated")
	}
	if !slices.Contains(result.Pending, "a.md") {
		t.Fatalf("changed file was not pending: %+v", result)
	}
	calls := deps.store.SnapshotCalls()
	if !slices.Contains(calls, "write:a.md") {
		t.Fatalf("derived rows were not written: %v", calls)
	}
	deps.markChangedDuringParse("a.md", "third")
	if _, err := idx.EnsureFresh(context.Background(), []string{"a.md"}, nil); err == nil {
		t.Fatal("freshness barrier accepted pending path")
	}
}

func TestEnsureFreshTreatsRequestedPathsAsPrefixes(t *testing.T) {
	deps := newDeps(file("docs/api.md", "first"))
	deps.markChangedDuringParse("docs/api.md", "second")
	idx := indexer.New(deps.config())
	_, err := idx.EnsureFresh(context.Background(), []string{"docs"}, nil)
	var freshnessErr *indexer.FreshnessError
	if !errors.As(err, &freshnessErr) || !slices.Equal(freshnessErr.Paths, []string{"docs/api.md"}) {
		t.Fatalf("error = %v", err)
	}
}

func TestRestatActivationUsesCurrentMetadataWhenHashIsUnchanged(t *testing.T) {
	deps := newDeps(file("a.md", "same"))
	deps.markMetadataChangedDuringParse("a.md", 2)
	idx := indexer.New(deps.config())
	if _, err := idx.Sync(context.Background(), indexer.Delta, nil); err != nil {
		t.Fatal(err)
	}
	if got := deps.store.Activated[0].MTimeNS; got != 2 {
		t.Fatalf("activated mtime = %d, want 2", got)
	}
}

func TestEmbeddingsEmbedEachNewChunkHashOnce(t *testing.T) {
	deps := newDeps(file("a.md", "same"), file("b.md", "same"))
	idx := indexer.New(deps.config())
	if _, err := idx.Sync(context.Background(), indexer.Delta, nil); err != nil {
		t.Fatal(err)
	}
	if got := deps.embedder.batchSizes(); !slices.Equal(got, []int{1}) {
		t.Fatalf("embedding batches = %v, want [1]", got)
	}
	if got := deps.store.Derived[0].Chunks[0].Embedding; len(got) == 0 {
		t.Fatal("first duplicate chunk was not embedded")
	}
	if got := deps.store.Derived[1].Chunks[0].Embedding; len(got) == 0 {
		t.Fatal("second duplicate chunk did not reuse its embedding")
	}
}

func TestDeleteActivatesTombstoneIndependently(t *testing.T) {
	deps := newDeps()
	old := state(file("gone.go", "old"), 1)
	deps.store.ManifestState[old.Path] = old
	deps.planner.plan.Deleted = []string{old.Path}
	idx := indexer.New(deps.config())
	result, err := idx.Sync(context.Background(), indexer.Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || len(deps.store.Activated) != 1 || !deps.store.Activated[0].Deleted {
		t.Fatalf("delete result=%+v activation=%+v", result, deps.store.Activated)
	}
}

func TestMissingCapturedManifestFileActivatesTombstone(t *testing.T) {
	old := file("gone.md", "old")
	deps := newDeps(old)
	deps.store.ManifestState[old.Path] = state(old, 1)
	deps.source.mu.Lock()
	delete(deps.source.files, old.Path)
	delete(deps.source.data, old.Path)
	deps.source.mu.Unlock()
	idx := indexer.New(deps.config())
	result, err := idx.Sync(context.Background(), indexer.Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || len(deps.store.Activated) != 1 || !deps.store.Activated[0].Deleted {
		t.Fatalf("missing file left active: result=%+v activated=%+v", result, deps.store.Activated)
	}
}

func TestProgressSinkCannotBlockOrPanicSync(t *testing.T) {
	block := make(chan struct{})
	deps := newDeps(file("a.md", "hello"))
	idx := indexer.New(deps.config())
	done := make(chan error, 1)
	go func() {
		_, err := idx.Sync(context.Background(), indexer.Delta, func(ctx context.Context, _ core.Progress) {
			select {
			case <-block:
			case <-ctx.Done():
			}
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("progress sink blocked index work")
	}
	close(block)
	if _, err := indexer.New(newDeps(file("b.md", "hello")).config()).Sync(context.Background(), indexer.Delta, func(context.Context, core.Progress) { panic("sink") }); err != nil {
		t.Fatal(err)
	}
}

func TestProgressSinkCancellationWaitsForCooperativeDispatcher(t *testing.T) {
	deps := newDeps(file("a.md", "hello"))
	idx := indexer.New(deps.config())
	exited := make(chan struct{})
	_, err := idx.Sync(context.Background(), indexer.Delta, func(ctx context.Context, _ core.Progress) {
		<-ctx.Done()
		select {
		case <-exited:
		default:
			close(exited)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("cooperative progress sink was not canceled before Sync returned")
	}
}

func TestContextIgnoringProgressSinkIsIsolated(t *testing.T) {
	deps := newDeps(file("a.md", "hello"))
	idx := indexer.New(deps.config())
	block := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := idx.Sync(context.Background(), indexer.Delta, func(context.Context, core.Progress) { <-block })
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("context-ignoring sink blocked Sync")
	}
	close(block)
}

func TestConsultedDependencyChangeLeavesDependentPending(t *testing.T) {
	a := file("a.go", "package p")
	dep := file("dep.go", "package p")
	deps := newDeps(a)
	deps.source.files[dep.Path], deps.source.data[dep.Path] = dep, []byte("package p")
	deps.goParser.result = goparser.Result{Files: []core.IndexedFile{{File: core.FileState{Path: a.Path}}}, Consulted: map[string]string{dep.Path: dep.Hash}}
	deps.goParser.after = func() {
		deps.source.mu.Lock()
		defer deps.source.mu.Unlock()
		deps.source.data[dep.Path] = []byte("package changed")
		deps.source.files[dep.Path] = file(dep.Path, "package changed")
	}
	result, err := indexer.New(deps.config()).Sync(context.Background(), indexer.Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Pending, []string{"a.go", "dep.go"}) || len(deps.store.Activated) != 0 {
		t.Fatalf("consulted dependency activated stale output: result=%+v activated=%v", result, deps.store.Activated)
	}
}

func TestConsultedDependencyMustMatchPlanningSnapshot(t *testing.T) {
	a := file("app/a.go", "package app")
	oldDependency := file("lib/dep.go", "package lib\nconst Version = 1")
	newDependency := file("lib/dep.go", "package lib\nconst Version = 2")
	deps := newDeps(a)
	deps.store.ManifestState[oldDependency.Path] = state(oldDependency, 1)
	deps.source.files[newDependency.Path] = newDependency
	deps.source.data[newDependency.Path] = []byte("package p\nconst Version = 2")
	deps.goParser.result = goparser.Result{
		Files:     []core.IndexedFile{{File: core.FileState{Path: a.Path}}},
		Consulted: map[string]string{newDependency.Path: newDependency.Hash},
	}

	result, err := indexer.New(deps.config()).Sync(context.Background(), indexer.Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Pending, []string{"app/a.go", "lib/dep.go"}) || len(deps.store.Activated) != 0 || len(deps.store.Derived) != 0 {
		t.Fatalf("consulted dependency escaped planning snapshot: result=%+v activated=%v derived=%v", result, deps.store.Activated, deps.store.Derived)
	}
}

func TestConsultedEmptyConfigDeletionLeavesScriptPending(t *testing.T) {
	a := file("a.ts", "export const a = 1")
	config := project.File{Path: "tsconfig.json", Kind: "resolution", Language: "typescript", Extension: ".json", Hash: hash(""), MTimeNS: 1}
	deps := newDeps(a)
	deps.source.files[config.Path], deps.source.data[config.Path] = config, []byte{}
	deps.script.result = tsparser.Result{Files: []core.IndexedFile{{File: core.FileState{Path: a.Path}}}, Consulted: map[string]string{config.Path: config.Hash}}
	deps.script.after = func() {
		deps.source.mu.Lock()
		defer deps.source.mu.Unlock()
		delete(deps.source.files, config.Path)
		delete(deps.source.data, config.Path)
	}
	result, err := indexer.New(deps.config()).Sync(context.Background(), indexer.Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Pending, []string{"a.ts", "tsconfig.json"}) || len(deps.store.Activated) != 0 {
		t.Fatalf("consulted config activated stale output: result=%+v activated=%v", result, deps.store.Activated)
	}
}

func TestUUIDEntropyFailureReturnsBeforeActivation(t *testing.T) {
	deps := newDeps(file("a.md", "hello"))
	cfg := deps.config()
	cfg.UUIDReader = failingReader{}
	_, err := indexer.New(cfg).Sync(context.Background(), indexer.Delta, nil)
	if err == nil || len(deps.store.Activated) != 0 {
		t.Fatalf("UUID failure was not returned before activation: err=%v activated=%v", err, deps.store.Activated)
	}
}

func TestParserFailureWritesEmptyDerivedThenActivatesCurrentHash(t *testing.T) {
	deps := newDeps(file("a.go", "package broken"))
	deps.goParser.err = errors.New("parse failed")
	old := state(file("a.go", "package old"), 1)
	deps.store.ManifestState[old.Path] = old
	idx := indexer.New(deps.config())
	result, err := idx.Sync(context.Background(), indexer.Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 || !deps.store.WasActivated("a.go", hash("package broken")) {
		t.Fatalf("result=%+v activated=%+v", result, deps.store.Activated)
	}
	if got := deps.store.Derived[0]; len(got.Units)+len(got.Edges)+len(got.Chunks) != 0 {
		t.Fatalf("parser failure persisted structure: %+v", got)
	}
	calls := deps.store.SnapshotCalls()
	if !(slices.Index(calls, "write:a.go") < slices.Index(calls, "activate:a.go")) {
		t.Fatalf("call order = %v", calls)
	}
}

func TestGoPackageActivatesAllFilesAndPropagatesGenerationAndHash(t *testing.T) {
	a := file("pkg/a.go", "package pkg")
	b := file("pkg/b.go", "package pkg")
	deps := newDeps(a)
	deps.store.ManifestState[b.Path] = state(b, 1)
	deps.source.files[b.Path] = b
	deps.source.data[b.Path] = []byte("package pkg")
	deps.goParser.result.Files = []core.IndexedFile{
		{File: core.FileState{Path: a.Path}, Units: []core.CodeUnit{{ID: "a", Path: a.Path}}},
		{File: core.FileState{Path: b.Path}, Units: []core.CodeUnit{{ID: "b", Path: b.Path}}},
	}
	idx := indexer.New(deps.config())
	result, err := idx.Sync(context.Background(), indexer.Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed != 2 || !slices.Equal(testutil.SortedPaths(deps.store.Activated), []string{"pkg/a.go", "pkg/b.go"}) {
		t.Fatalf("result=%+v activated=%+v", result, deps.store.Activated)
	}
	for _, derived := range deps.store.Derived {
		if derived.File.Generation != result.Generation || derived.Units[0].Generation != result.Generation || derived.Units[0].FileHash != derived.File.Hash {
			t.Fatalf("metadata was not propagated: %+v", derived)
		}
	}
}

func TestFullCleanupFailureIsOnlyAWarningAndRunStoresGitState(t *testing.T) {
	deps := newDeps(file("a.md", "hello"))
	deps.planner.plan.Head = "head"
	deps.planner.plan.DirtyPaths = []string{"a.md"}
	deps.store.CleanupError = errors.New("busy")
	idx := indexer.New(deps.config())
	result, err := idx.Sync(context.Background(), indexer.Full, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) == 0 || len(deps.store.Runs) == 0 {
		t.Fatalf("result=%+v runs=%+v", result, deps.store.Runs)
	}
	run := deps.store.Runs[len(deps.store.Runs)-1]
	if run.GitHead != "head" || !slices.Equal(run.DirtyPaths, []string{"a.md"}) || run.State == "" {
		t.Fatalf("run = %+v", run)
	}
}

func TestInitializeRestoresStatusWithoutStartingAWrite(t *testing.T) {
	deps := newDeps()
	deps.store.ManifestState["a.go"] = core.FileState{Path: "a.go", Hash: hash("a"), Generation: 5, ParserVersion: project.ParserVersion}
	deps.store.LatestRun = indexer.RunSnapshot{State: "ready", TargetGeneration: 5}
	idx := indexer.New(deps.config())
	if err := idx.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if idx.Status().State != "ready" || idx.Status().ActiveGeneration != 5 || idx.Manifest()["a.go"].Generation != 5 {
		t.Fatalf("status=%+v manifest=%+v", idx.Status(), idx.Manifest())
	}
	if len(deps.store.Runs) != 0 || len(deps.store.Activated) != 0 {
		t.Fatal("initialize wrote index state")
	}
}

type dependencies struct {
	store     *testutil.FakeStore
	planner   *fakePlanner
	source    *fakeSource
	goParser  *fakeGoParser
	script    *fakeScriptParser
	markdown  *fakeMarkdown
	embedder  *fakeEmbedder
	projectID string
}

func newDeps(files ...project.File) *dependencies {
	plan := project.ChangePlan{Changed: append([]project.File(nil), files...)}
	source := &fakeSource{files: map[string]project.File{}, data: map[string][]byte{}}
	for _, file := range files {
		source.files[file.Path] = file
		source.data[file.Path] = []byte(fileContents[file.Hash])
	}
	return &dependencies{
		store: testutil.NewFakeStore(), planner: &fakePlanner{plan: plan, started: make(chan struct{}, 10)}, source: source,
		goParser: &fakeGoParser{}, script: &fakeScriptParser{}, markdown: &fakeMarkdown{}, embedder: &fakeEmbedder{}, projectID: "p",
	}
}

func (d *dependencies) config() indexer.Config {
	return indexer.Config{ProjectID: d.projectID, Root: "/project", Store: d.store, Planner: d.planner, Source: d.source,
		GoParser: d.goParser, ScriptParser: d.script, MarkdownParser: d.markdown, Embedder: d.embedder}
}

func (d *dependencies) markChangedDuringParse(path, contents string) {
	d.markdown.after = func() {
		changed := file(path, contents)
		d.source.mu.Lock()
		d.source.files[path], d.source.data[path] = changed, []byte(contents)
		d.source.mu.Unlock()
	}
}

func (d *dependencies) markMetadataChangedDuringParse(path string, mtimeNS int64) {
	d.markdown.after = func() {
		d.source.mu.Lock()
		file := d.source.files[path]
		file.MTimeNS = mtimeNS
		d.source.files[path] = file
		d.source.mu.Unlock()
	}
}

type fakePlanner struct {
	mu      sync.Mutex
	plan    project.ChangePlan
	block   chan struct{}
	started chan struct{}
	calls   int
	modes   []indexer.Mode
}

func (p *fakePlanner) Plan(ctx context.Context, mode indexer.Mode, _ string, _ map[string]core.FileState, _ []string) (project.ChangePlan, error) {
	p.mu.Lock()
	p.calls++
	p.modes = append(p.modes, mode)
	p.mu.Unlock()
	p.started <- struct{}{}
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return project.ChangePlan{}, ctx.Err()
		}
	}
	return p.plan, nil
}

type fakeSource struct {
	mu    sync.Mutex
	files map[string]project.File
	data  map[string][]byte
}

func (s *fakeSource) Read(_ context.Context, path string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[path]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeSource) Inspect(_ context.Context, path string) (project.File, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, ok := s.files[path]
	if !ok {
		return project.File{}, "", os.ErrNotExist
	}
	return file, "", nil
}

type fakeGoParser struct {
	result goparser.Result
	err    error
	after  func()
}

func (p *fakeGoParser) Parse(context.Context, goparser.Request) (goparser.Result, error) {
	if p.after != nil {
		after := p.after
		p.after = nil
		after()
	}
	return p.result, p.err
}

type fakeScriptParser struct {
	result tsparser.Result
	err    error
	after  func()
}

func (p *fakeScriptParser) Parse(context.Context, tsparser.Request) (tsparser.Result, error) {
	if p.after != nil {
		after := p.after
		p.after = nil
		after()
	}
	return p.result, p.err
}

type fakeMarkdown struct {
	after   func()
	started chan struct{}
	block   chan struct{}
}

func (p *fakeMarkdown) Parse(path string, data []byte, fileHash string) ([]core.DocChunk, []string, error) {
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
	if p.block != nil {
		<-p.block
	}
	if p.after != nil {
		after := p.after
		p.after = nil
		after()
	}
	return []core.DocChunk{{ID: path, Path: path, Content: string(data), ChunkHash: hash(string(data)), FileHash: fileHash}}, nil, nil
}

type fakeEmbedder struct {
	mu     sync.Mutex
	inputs [][]string
	err    error
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inputs = append(e.inputs, append([]string(nil), texts...))
	if e.err != nil {
		return nil, e.err
	}
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = vector(float32(i + 1))
	}
	return result, nil
}

func (e *fakeEmbedder) batchSizes() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]int, len(e.inputs))
	for i := range e.inputs {
		result[i] = len(e.inputs[i])
	}
	return result
}

var fileContents = map[string]string{}

func file(path, contents string) project.File {
	result := project.File{Path: path, Size: int64(len(contents)), MTimeNS: 1, Hash: hash(contents)}
	fileContents[result.Hash] = contents
	switch {
	case slices.Contains([]string{".go"}, extension(path)):
		result.Kind, result.Language, result.Extension = "code", "go", ".go"
	case slices.Contains([]string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}, extension(path)):
		result.Kind, result.Language, result.Extension = "code", "typescript", extension(path)
	default:
		result.Kind, result.Language, result.Extension = "document", "markdown", ".md"
	}
	return result
}

func state(file project.File, generation uint64) core.FileState {
	return core.FileState{ProjectID: "p", Path: file.Path, Kind: file.Kind, Language: file.Language, Extension: file.Extension,
		Size: file.Size, MTimeNS: file.MTimeNS, Hash: file.Hash, ParserVersion: project.ParserVersion, Generation: generation}
}

func extension(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[i:]
		}
	}
	return ""
}

func hash(contents string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(contents))) }

func vector(value float32) []float32 {
	result := make([]float32, 1024)
	result[0] = value
	return result
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
