package indexer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ratibordas/go-mcp-codeweft/internal/config"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/goparser"
	"github.com/ratibordas/go-mcp-codeweft/internal/graph"
	"github.com/ratibordas/go-mcp-codeweft/internal/markdown"
	"github.com/ratibordas/go-mcp-codeweft/internal/project"
	storepkg "github.com/ratibordas/go-mcp-codeweft/internal/store"
	"github.com/ratibordas/go-mcp-codeweft/internal/tsparser"
)

type Mode string

const (
	Delta Mode = "delta"
	Full  Mode = "full"
)

const embeddingBatchSize = 16

// A callback that ignores cancellation cannot be stopped by Go. This bounded
// wait gives cooperative callbacks time to exit without allowing one hostile
// sink to retain the indexing caller or worker forever.
const progressSinkStopGrace = 100 * time.Millisecond

type Planner interface {
	Plan(context.Context, Mode, string, map[string]core.FileState, []string) (project.ChangePlan, error)
}

type Source interface {
	Read(context.Context, string) ([]byte, error)
	Inspect(context.Context, string) (project.File, string, error)
}

type GoParser interface {
	Parse(context.Context, goparser.Request) (goparser.Result, error)
}

type ScriptParser interface {
	Parse(context.Context, tsparser.Request) (tsparser.Result, error)
}

type MarkdownParser interface {
	Parse(string, []byte, string) ([]core.DocChunk, []string, error)
}

type Store interface {
	LoadManifest(context.Context, string) (map[string]core.FileState, error)
	NextGeneration(context.Context, string) (uint64, error)
	LookupEmbeddings(context.Context, string, []string) (map[string][]float32, error)
	WriteDerived(context.Context, core.IndexedFile) error
	ActivateFile(context.Context, core.FileState) error
	LoadGraph(context.Context, string) ([]core.CodeUnit, []core.CodeEdge, error)
	LoadLatestRun(context.Context, string) (RunSnapshot, error)
	LoadEmbeddingPending(context.Context, string) ([]string, error)
	WriteRun(context.Context, RunSnapshot) error
	CleanupObsolete(context.Context, string) error
}

type Config struct {
	ProjectID      string
	Root           string
	Index          config.Index
	Store          Store
	Planner        Planner
	Source         Source
	GoParser       GoParser
	ScriptParser   ScriptParser
	MarkdownParser MarkdownParser
	Embedder       core.Embedder
	HistoricRates  map[string]float64
	Now            func() time.Time
	UUIDReader     io.Reader
}

type RunSnapshot struct {
	ProjectID                         string
	RunID, Mode, State, Phase, Error  string
	StartedAt                         time.Time
	FinishedAt                        *time.Time
	Completed, Total                  uint64
	Changed, Deleted, Skipped, Failed uint64
	FilesPerSecond, ChunksPerSecond   float64
	ETA                               time.Duration
	PhaseTimings                      map[string]time.Duration
	Warnings, DirtyPaths, Pending     []string
	StartGeneration, TargetGeneration uint64
	GitHead                           string
}

func (r RunSnapshot) Clone() RunSnapshot {
	r.Warnings = append([]string(nil), r.Warnings...)
	r.DirtyPaths = append([]string(nil), r.DirtyPaths...)
	r.Pending = append([]string(nil), r.Pending...)
	r.PhaseTimings = make(map[string]time.Duration, len(r.PhaseTimings))
	for phase, duration := range r.PhaseTimings {
		r.PhaseTimings[phase] = duration
	}
	if r.FinishedAt != nil {
		finished := *r.FinishedAt
		r.FinishedAt = &finished
	}
	return r
}

type FreshnessError struct{ Paths []string }

func (e *FreshnessError) Error() string {
	return "index freshness pending for: " + strings.Join(e.Paths, ", ")
}

type Indexer struct {
	cfg Config

	mu           sync.Mutex
	active       *work
	queued       *work
	nextWaiterID uint64
	tracker      *Tracker
	loaded       bool
	manifest     map[string]core.FileState
	graph        *graph.Graph
	recordedHead string
	dirtyPaths   []string
	metadata     graph.ChangeMetadata
	embedPending map[string]bool
}

type work struct {
	mode   Mode
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	mu                 sync.Mutex
	waiters            map[uint64]*waiter
	sealed, activating bool
	result             core.SyncResult
	err                error
}

type waiter struct {
	ctx       context.Context
	cancel    context.CancelFunc
	sink      core.ProgressSink
	mandatory chan core.Progress
	latest    chan core.Progress
	done      chan struct{}
	exited    chan struct{}
	once      sync.Once
}

func New(cfg Config) *Indexer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.UUIDReader == nil {
		cfg.UUIDReader = rand.Reader
	}
	if cfg.ProjectID == "" {
		cfg.ProjectID = cfg.Root
	}
	if cfg.Planner == nil {
		cfg.Planner = ProjectPlanner{Root: cfg.Root, Index: cfg.Index}
	}
	if cfg.Source == nil {
		cfg.Source = ProjectSource{Root: cfg.Root, Index: cfg.Index}
	}
	if cfg.GoParser == nil {
		cfg.GoParser = goparser.New()
	}
	if cfg.ScriptParser == nil {
		cfg.ScriptParser = tsparser.New()
	}
	if cfg.MarkdownParser == nil {
		cfg.MarkdownParser = markdownAdapter{}
	}
	return &Indexer{
		cfg: cfg, tracker: newTracker(cfg.Now, cfg.HistoricRates), manifest: map[string]core.FileState{}, graph: graph.New(nil, nil),
		metadata: emptyMetadata(), embedPending: map[string]bool{},
	}
}

func (i *Indexer) Sync(ctx context.Context, mode Mode, sink core.ProgressSink) (core.SyncResult, error) {
	if err := ctx.Err(); err != nil {
		return core.SyncResult{}, err
	}
	if mode != Delta && mode != Full {
		return core.SyncResult{}, fmt.Errorf("unknown index mode %q", mode)
	}
	i.mu.Lock()
	selected, start := i.selectWork(mode)
	i.nextWaiterID++
	id := i.nextWaiterID
	registered := newWaiter(ctx, sink)
	selected.mu.Lock()
	selected.waiters[id] = registered
	selected.mu.Unlock()
	deliverCurrent := !start && i.active == selected
	i.mu.Unlock()
	if start {
		go i.run(selected)
	} else if deliverCurrent {
		registered.deliver(i.Status().Progress, false)
	}
	select {
	case <-ctx.Done():
		i.removeWaiter(selected, id)
		return core.SyncResult{}, ctx.Err()
	case <-selected.done:
		i.removeWaiter(selected, id)
		return cloneSyncResult(selected.result), selected.err
	}
}

func (i *Indexer) EnsureFresh(ctx context.Context, paths []string, sink core.ProgressSink) (core.SyncResult, error) {
	result, err := i.Sync(ctx, Delta, sink)
	if err != nil {
		return result, err
	}
	pending := filterPaths(result.Pending, paths)
	if len(pending) != 0 {
		return result, &FreshnessError{Paths: pending}
	}
	return result, nil
}

func (i *Indexer) Initialize(ctx context.Context) error { return i.loadState(ctx) }

func (i *Indexer) Pending() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.tracker.statusSnapshot().Pending...)
}

func (i *Indexer) Status() core.IndexStatus {
	i.mu.Lock()
	tracker := i.tracker
	i.mu.Unlock()
	return tracker.statusSnapshot()
}

func (i *Indexer) Manifest() map[string]core.FileState {
	i.mu.Lock()
	defer i.mu.Unlock()
	return cloneManifest(i.manifest)
}

func (i *Indexer) Graph() *graph.Graph {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.graph
}

func (i *Indexer) selectWork(mode Mode) (*work, bool) {
	if i.active == nil {
		i.active = newWork(mode)
		return i.active, true
	}
	i.active.mu.Lock()
	sealed := i.active.sealed
	i.active.mu.Unlock()
	if i.queued != nil {
		if mode == Full {
			i.queued.mode = Full
		}
		return i.queued, false
	}
	if !sealed && mode == i.active.mode {
		return i.active, false
	}
	i.queued = newWork(mode)
	return i.queued, false
}

func newWork(mode Mode) *work {
	ctx, cancel := context.WithCancel(context.Background())
	return &work{mode: mode, done: make(chan struct{}), ctx: ctx, cancel: cancel, waiters: map[uint64]*waiter{}}
}

func (i *Indexer) run(current *work) {
	current.result, current.err = i.execute(current.ctx, current)
	close(current.done)
	i.mu.Lock()
	if i.active == current {
		i.active = i.queued
		i.queued = nil
		next := i.active
		i.mu.Unlock()
		if next != nil {
			go i.run(next)
		}
		return
	}
	i.mu.Unlock()
}

func (i *Indexer) removeWaiter(current *work, id uint64) {
	i.mu.Lock()
	current.mu.Lock()
	waiter := current.waiters[id]
	delete(current.waiters, id)
	empty, activating := len(current.waiters) == 0, current.activating
	current.mu.Unlock()
	if empty {
		if i.queued == current {
			i.queued = nil
			current.cancel()
		} else if i.active == current && !activating {
			current.cancel()
		}
	}
	i.mu.Unlock()
	if waiter != nil {
		waiter.stop()
	}
}

func (w *work) seal() {
	w.mu.Lock()
	w.sealed = true
	w.mu.Unlock()
}

func (w *work) setActivating() {
	w.mu.Lock()
	w.activating = true
	w.mu.Unlock()
}

func newWaiter(ctx context.Context, sink core.ProgressSink) *waiter {
	callbackCtx, cancel := context.WithCancel(ctx)
	w := &waiter{ctx: callbackCtx, cancel: cancel, sink: sink}
	if sink == nil {
		return w
	}
	// Phase changes are few and mandatory. Keep enough room for a complete run
	// while rate-limited updates remain non-blocking for the indexing worker.
	w.mandatory = make(chan core.Progress, 8)
	w.latest = make(chan core.Progress, 1)
	w.done = make(chan struct{})
	w.exited = make(chan struct{})
	go func() {
		defer close(w.exited)
		for {
			select {
			case progress := <-w.mandatory:
				w.call(progress)
				continue
			default:
			}
			select {
			case <-w.done:
				w.drain()
				return
			case progress := <-w.mandatory:
				w.call(progress)
			case progress := <-w.latest:
				w.call(progress)
			}
		}
	}()
	return w
}

func (w *waiter) call(progress core.Progress) {
	defer func() { _ = recover() }()
	w.sink(w.ctx, progress)
}

func (w *waiter) deliver(progress core.Progress, mandatory bool) {
	if w == nil || w.latest == nil || w.ctx.Err() != nil {
		return
	}
	if mandatory {
		select {
		case <-w.latest:
		default:
		}
		select {
		case w.mandatory <- progress:
		default:
		}
		return
	}
	select {
	case w.latest <- progress:
	default:
		select {
		case <-w.latest:
		default:
		}
		select {
		case w.latest <- progress:
		default:
		}
	}
}

func (w *waiter) drain() {
	for {
		select {
		case progress := <-w.mandatory:
			w.call(progress)
		default:
			select {
			case progress := <-w.latest:
				w.call(progress)
			default:
				return
			}
		}
	}
}

func (w *waiter) stop() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		w.cancel()
		if w.done != nil {
			close(w.done)
			select {
			case <-w.exited:
			case <-time.After(progressSinkStopGrace):
			}
		}
	})
	if w.done == nil {
		w.cancel()
	}
}

func (i *Indexer) execute(ctx context.Context, current *work) (result core.SyncResult, resultErr error) {
	if i.cfg.Store == nil {
		return result, errors.New("index store is required")
	}
	if err := i.loadState(ctx); err != nil {
		return result, err
	}
	i.mu.Lock()
	manifest := cloneManifest(i.manifest)
	startGeneration := i.tracker.statusSnapshot().ActiveGeneration
	recordedHead := i.recordedHead
	dirtyPaths := append([]string(nil), i.dirtyPaths...)
	i.mu.Unlock()
	generation, err := i.cfg.Store.NextGeneration(ctx, i.cfg.ProjectID)
	if err != nil {
		return result, err
	}
	result.Generation = generation
	tracker := newTracker(i.cfg.Now, i.cfg.HistoricRates)
	tracker.setEmitter(func(progress core.Progress, mandatory bool) { broadcast(current, progress, mandatory) })
	tracker.begin(generation)
	tracker.activeGeneration(startGeneration)
	i.mu.Lock()
	i.tracker = tracker
	i.mu.Unlock()
	started := i.cfg.Now()
	runID, err := newRunID(i.cfg.UUIDReader)
	if err != nil {
		return result, err
	}
	var plan project.ChangePlan
	degraded := false
	terminalWriteAttempted := false
	defer func() {
		activeGeneration := result.Generation
		if len(result.Pending) != 0 {
			activeGeneration = startGeneration
		}
		tracker.finish(activeGeneration, degraded, resultErr)
		tracker.emitFinal()
		if resultErr != nil && !terminalWriteAttempted {
			i.writeFailedRun(context.Background(), runID, tracker, current.mode, started, startGeneration, result.Generation, plan, resultErr)
		}
	}()

	tracker.phase("scan", 0)
	plan, err = i.cfg.Planner.Plan(ctx, current.mode, recordedHead, manifest, dirtyPaths)
	if err != nil {
		return result, fmt.Errorf("plan index changes: %w", err)
	}
	current.seal()
	result.Warnings = append(result.Warnings, plan.Warnings...)
	inputs, skipped, capturedDeletes, capturePending, warnings, err := i.captureInputs(ctx, plan, manifest)
	if err != nil {
		return result, err
	}
	result.Skipped = skipped
	result.Warnings = append(result.Warnings, warnings...)
	plan.Deleted = sortedUnique(append(plan.Deleted, capturedDeletes...))
	result.Pending = capturePending
	consultedBaseline := captureHashes(manifest, inputs)
	tracker.counts(0, uint64(len(plan.Deleted)), uint64(skipped), 0)
	tracker.advance(uint64(len(inputs)), uint64(len(inputs)), 0, 0)

	tracker.phase("parse", uint64(len(inputs)))
	outputs, parseWarnings, failed, parseMetadata, consulted := i.parse(ctx, inputs, manifest, generation)
	result.Failed = failed
	result.Warnings = append(result.Warnings, parseWarnings...)
	if failed != 0 {
		degraded = true
	}
	tracker.counts(uint64(len(outputs)), uint64(len(plan.Deleted)), uint64(skipped), uint64(failed))
	tracker.advance(uint64(len(inputs)), uint64(len(inputs)), uint64(len(outputs)), 0)

	tracker.phase("graph", uint64(len(outputs)))
	tracker.advance(uint64(len(outputs)), uint64(len(outputs)), uint64(len(outputs)), 0)

	tracker.phase("embed", uint64(chunkCount(outputs)))
	embedWarnings, embeddingPending, err := i.embed(ctx, outputs)
	if err != nil {
		return result, err
	}
	if len(embedWarnings) != 0 {
		degraded = true
	}
	result.Warnings = append(result.Warnings, embedWarnings...)
	tracker.advance(uint64(chunkCount(outputs)), uint64(chunkCount(outputs)), uint64(len(outputs)), uint64(chunkCount(outputs)))

	tracker.phase("persist", uint64(len(outputs)+len(plan.Deleted)))
	pending, activated, err := i.persist(ctx, current, outputs, plan.Deleted, manifest, generation, consulted, consultedBaseline, tracker)
	if err != nil {
		return result, err
	}
	result.Changed = len(outputs)
	result.Deleted = len(plan.Deleted)
	result.Pending = sortedUnique(append(result.Pending, pending...))
	plan.DirtyPaths = sortedUnique(append(plan.DirtyPaths, result.Pending...))
	if len(result.Pending) != 0 {
		degraded = true
	}
	tracker.pending(result.Pending)
	tracker.counts(uint64(result.Changed), uint64(result.Deleted), uint64(result.Skipped), uint64(result.Failed))
	tracker.advance(uint64(len(outputs)+len(plan.Deleted)), uint64(len(outputs)+len(plan.Deleted)), uint64(activated), uint64(chunkCount(outputs)))

	if current.mode == Full && len(result.Pending) == 0 {
		if err := i.cfg.Store.CleanupObsolete(ctx, i.cfg.ProjectID); err != nil {
			warning := "cleanup obsolete rows: " + err.Error()
			result.Warnings = append(result.Warnings, warning)
			degraded = true
		}
	}
	result.Warnings = sortedUnique(result.Warnings)
	for _, warning := range result.Warnings {
		tracker.warn(warning)
	}
	newManifest, err := i.cfg.Store.LoadManifest(ctx, i.cfg.ProjectID)
	if err != nil {
		return result, err
	}
	units, edges, err := i.cfg.Store.LoadGraph(ctx, i.cfg.ProjectID)
	if err != nil {
		return result, err
	}
	i.mu.Lock()
	i.manifest = cloneManifest(newManifest)
	i.graph = graph.New(units, edges)
	i.recordedHead = plan.Head
	i.dirtyPaths = sortedUnique(append(append([]string(nil), plan.DirtyPaths...), result.Pending...))
	i.metadata = mergeMetadata(i.metadata, parseMetadata)
	for path := range i.embedPending {
		delete(i.embedPending, path)
	}
	for path := range embeddingPending {
		i.embedPending[path] = true
	}
	i.mu.Unlock()

	status := tracker.runStatusSnapshot()
	finished := i.cfg.Now()
	degraded = degraded || len(result.Warnings) != 0 || len(result.Pending) != 0 || result.Failed != 0
	runState := "ready"
	if degraded {
		runState = "degraded"
	}
	run := runSnapshot(runID, i.cfg.ProjectID, current.mode, started, &finished, startGeneration, generation, plan, status, runState, "")
	terminalWriteAttempted = true
	if err := i.cfg.Store.WriteRun(ctx, run); err != nil {
		return result, err
	}
	return result, nil
}

func (i *Indexer) loadState(ctx context.Context) error {
	i.mu.Lock()
	if i.loaded {
		i.mu.Unlock()
		return nil
	}
	i.mu.Unlock()
	manifest, err := i.cfg.Store.LoadManifest(ctx, i.cfg.ProjectID)
	if err != nil {
		return err
	}
	units, edges, err := i.cfg.Store.LoadGraph(ctx, i.cfg.ProjectID)
	if err != nil {
		return err
	}
	i.mu.Lock()
	if !i.loaded {
		run, err := i.cfg.Store.LoadLatestRun(ctx, i.cfg.ProjectID)
		if err != nil {
			i.mu.Unlock()
			return err
		}
		pending, err := i.cfg.Store.LoadEmbeddingPending(ctx, i.cfg.ProjectID)
		if err != nil {
			i.mu.Unlock()
			return err
		}
		i.manifest = cloneManifest(manifest)
		i.graph = graph.New(units, edges)
		i.recordedHead = run.GitHead
		i.dirtyPaths = append([]string(nil), run.DirtyPaths...)
		i.tracker.restore(runStatus(run, manifest))
		i.metadata = metadataFromActive(manifest, units, edges)
		for _, path := range pending {
			i.embedPending[path] = true
		}
		i.loaded = true
	}
	i.mu.Unlock()
	return nil
}

func (i *Indexer) captureInputs(ctx context.Context, plan project.ChangePlan, manifest map[string]core.FileState) (map[string]capturedFile, int, []string, []string, []string, error) {
	inputs := map[string]capturedFile{}
	skipped := 0
	warnings := []string{}
	capturedDeletes := []string{}
	pending := []string{}
	resolutionPaths := []string{}
	failedPaths := []string{}
	deleted := stringSet(plan.Deleted)
	for _, file := range plan.Changed {
		if deleted[file.Path] {
			continue
		}
		if file.Kind == "resolution" {
			resolutionPaths = append(resolutionPaths, file.Path)
			captured, err := i.capture(ctx, file.Path, file)
			if err != nil {
				failedPaths = append(failedPaths, file.Path)
				warnings = append(warnings, file.Path+": "+err.Error())
				if os.IsNotExist(err) && manifest[file.Path].Path != "" {
					capturedDeletes = append(capturedDeletes, file.Path)
				} else {
					pending = append(pending, file.Path)
				}
				continue
			}
			inputs[file.Path] = captured
			continue
		}
		if !supported(file.Extension) {
			skipped++
			continue
		}
		captured, err := i.capture(ctx, file.Path, file)
		if err != nil {
			failedPaths = append(failedPaths, file.Path)
			warnings = append(warnings, file.Path+": "+err.Error())
			if os.IsNotExist(err) && manifest[file.Path].Path != "" {
				capturedDeletes = append(capturedDeletes, file.Path)
			} else {
				pending = append(pending, file.Path)
			}
			continue
		}
		inputs[file.Path] = captured
	}
	changedPaths := append(append(append([]string(nil), plan.Deleted...), resolutionPaths...), failedPaths...)
	for path := range inputs {
		changedPaths = append(changedPaths, path)
	}
	affected := i.expandAffected(changedPaths, inputs, manifest)
	if len(failedPaths) != 0 {
		pending = append(pending, affected...)
	}
	for _, path := range affected {
		if deleted[path] || inputs[path].file.Path != "" {
			continue
		}
		old, ok := manifest[path]
		if !ok || !supported(old.Extension) {
			continue
		}
		captured, err := i.capture(ctx, path, projectFile(old))
		if err != nil {
			warnings = append(warnings, path+": "+err.Error())
			if os.IsNotExist(err) {
				capturedDeletes = append(capturedDeletes, path)
			} else {
				pending = append(pending, path)
			}
			continue
		}
		inputs[path] = captured
	}
	i.mu.Lock()
	embedPaths := make([]string, 0, len(i.embedPending))
	for path := range i.embedPending {
		embedPaths = append(embedPaths, path)
	}
	i.mu.Unlock()
	for _, path := range embedPaths {
		if inputs[path].file.Path != "" {
			continue
		}
		old, ok := manifest[path]
		if !ok || old.Extension != ".md" {
			continue
		}
		captured, err := i.capture(ctx, path, projectFile(old))
		if err == nil {
			inputs[path] = captured
		}
	}
	return inputs, skipped, sortedUnique(capturedDeletes), sortedUnique(pending), sortedUnique(warnings), nil
}

type capturedFile struct {
	file project.File
	data []byte
}

func (i *Indexer) capture(ctx context.Context, path string, fallback project.File) (capturedFile, error) {
	if err := ctx.Err(); err != nil {
		return capturedFile{}, err
	}
	meta, reason, err := i.cfg.Source.Inspect(ctx, path)
	if err != nil {
		return capturedFile{}, err
	}
	if reason != "" {
		if strings.Contains(reason, "no such file") {
			return capturedFile{}, fmt.Errorf("inspect %s: %w", path, os.ErrNotExist)
		}
		return capturedFile{}, errors.New(reason)
	}
	data, err := i.cfg.Source.Read(ctx, path)
	if err != nil {
		return capturedFile{}, err
	}
	if meta.Path == "" {
		meta = fallback
	}
	meta.Hash = contentHash(data)
	return capturedFile{file: meta, data: data}, nil
}

func (i *Indexer) expandAffected(changed []string, inputs map[string]capturedFile, manifest map[string]core.FileState) []string {
	i.mu.Lock()
	metadata := cloneMetadata(i.metadata)
	i.mu.Unlock()
	result := []string{}
	goChanged, scriptChanged := []string{}, []string{}
	for _, changedPath := range changed {
		if isGoResolution(changedPath) {
			directory := path.Dir(changedPath)
			workspace := path.Base(changedPath) == "go.work"
			for filePath, state := range manifest {
				if state.Extension == ".go" && (workspace || directory == "." || path.Dir(filePath) == directory || strings.HasPrefix(filePath, directory+"/")) {
					result = append(result, filePath)
				}
			}
			for filePath, input := range inputs {
				if input.file.Extension == ".go" && (workspace || directory == "." || path.Dir(filePath) == directory || strings.HasPrefix(filePath, directory+"/")) {
					result = append(result, filePath)
				}
			}
			continue
		}
		if isScriptResolution(changedPath) {
			directory := path.Dir(changedPath)
			for filePath, state := range manifest {
				if scriptExtension(state.Extension) && (directory == "." || strings.HasPrefix(filePath, directory+"/")) {
					result = append(result, filePath)
				}
			}
			for filePath, input := range inputs {
				if scriptExtension(input.file.Extension) && (directory == "." || strings.HasPrefix(filePath, directory+"/")) {
					result = append(result, filePath)
				}
			}
			continue
		}
		ext := strings.ToLower(path.Ext(changedPath))
		if ext == ".go" {
			goChanged = append(goChanged, changedPath)
		} else if scriptExtension(ext) {
			scriptChanged = append(scriptChanged, changedPath)
		}
	}
	packages := graph.AffectedGo(goChanged, metadata)
	packageSet := stringSet(packages)
	dirs := map[string]bool{}
	for _, changedPath := range goChanged {
		dirs[path.Dir(changedPath)] = true
	}
	for filePath, state := range manifest {
		if state.Extension == ".go" && (dirs[path.Dir(filePath)] || packageSet[metadata.GoFilePackage[filePath]]) {
			result = append(result, filePath)
		}
	}
	scripts := graph.AffectedScript(scriptChanged, metadata)
	result = append(result, scripts...)
	for filePath := range inputs {
		result = append(result, filePath)
	}
	return sortedUnique(result)
}

func isGoResolution(filePath string) bool {
	switch path.Base(filePath) {
	case "go.mod", "go.sum", "go.work":
		return true
	}
	return false
}

func isScriptResolution(filePath string) bool {
	switch path.Base(filePath) {
	case "tsconfig.json", "jsconfig.json", "package.json":
		return true
	}
	return false
}

func (i *Indexer) parse(ctx context.Context, inputs map[string]capturedFile, manifest map[string]core.FileState, generation uint64) (map[string]core.IndexedFile, []string, int, graph.ChangeMetadata, map[string]string) {
	outputs := map[string]core.IndexedFile{}
	warnings := []string{}
	failed := 0
	metadata := emptyMetadata()
	consulted := map[string]string{}
	allHashes := make(map[string]string, len(manifest)+len(inputs))
	for path, file := range manifest {
		allHashes[path] = file.Hash
	}
	for path, input := range inputs {
		allHashes[path] = input.file.Hash
	}
	goPaths, scriptPaths, markdownPaths, resolutionPaths := []string{}, []string{}, []string{}, []string{}
	for path, input := range inputs {
		switch {
		case input.file.Kind == "resolution":
			resolutionPaths = append(resolutionPaths, path)
		case input.file.Extension == ".go":
			goPaths = append(goPaths, path)
		case scriptExtension(input.file.Extension):
			scriptPaths = append(scriptPaths, path)
		case input.file.Extension == ".md":
			markdownPaths = append(markdownPaths, path)
		}
	}
	sort.Strings(goPaths)
	sort.Strings(scriptPaths)
	sort.Strings(markdownPaths)
	sort.Strings(resolutionPaths)
	for _, filePath := range resolutionPaths {
		outputs[filePath] = core.IndexedFile{File: fileState(i.cfg.ProjectID, inputs[filePath].file, generation)}
	}
	for _, filePath := range markdownPaths {
		if err := ctx.Err(); err != nil {
			return outputs, append(warnings, err.Error()), failed, metadata, consulted
		}
		input := inputs[filePath]
		chunks, parseWarnings, err := i.cfg.MarkdownParser.Parse(filePath, input.data, input.file.Hash)
		warnings = append(warnings, parseWarnings...)
		indexed := core.IndexedFile{File: fileState(i.cfg.ProjectID, input.file, generation), Chunks: chunks, Warnings: parseWarnings}
		if err != nil {
			failed++
			indexed.Chunks = nil
			warning := filePath + ": " + err.Error()
			indexed.Warnings = append(indexed.Warnings, warning)
			warnings = append(warnings, warning)
		}
		outputs[filePath] = normalizeIndexed(indexed, indexed.File)
	}
	if len(goPaths) != 0 {
		patterns := packagePatterns(goPaths, i.metadata)
		parsed, err := i.cfg.GoParser.Parse(ctx, goparser.Request{Root: i.cfg.Root, Patterns: patterns, Generation: generation, FileHashes: allHashes, Sources: capturedSources(inputs, nil)})
		if err != nil {
			warning := "Go parser: " + err.Error()
			warnings = append(warnings, warning)
			for _, filePath := range goPaths {
				input := inputs[filePath]
				outputs[filePath] = core.IndexedFile{File: fileState(i.cfg.ProjectID, input.file, generation), Warnings: []string{warning}}
				failed++
			}
		} else {
			for path, hash := range parsed.Consulted {
				consulted[path] = hash
			}
			warnings = append(warnings, parsed.Warnings...)
			metadata.GoFilePackage = cloneStringMap(parsed.FilePackages)
			metadata.GoReverseImport = cloneStringLists(parsed.ReversePackageImports)
			for _, indexed := range parsed.Files {
				if input, ok := inputs[indexed.File.Path]; ok {
					state := fileState(i.cfg.ProjectID, input.file, generation)
					outputs[state.Path] = normalizeIndexed(indexed, state)
				}
			}
			failed += fillMissing(outputs, inputs, goPaths, i.cfg.ProjectID, generation, "Go parser produced no output", &warnings)
		}
	}
	if len(scriptPaths) != 0 {
		parsed, err := i.cfg.ScriptParser.Parse(ctx, tsparser.Request{Root: i.cfg.Root, Paths: scriptPaths, Generation: generation, FileHashes: allHashes, Sources: capturedSources(inputs, nil)})
		if err != nil {
			warning := "script parser: " + err.Error()
			warnings = append(warnings, warning)
			for _, filePath := range scriptPaths {
				input := inputs[filePath]
				outputs[filePath] = core.IndexedFile{File: fileState(i.cfg.ProjectID, input.file, generation), Warnings: []string{warning}}
				failed++
			}
		} else {
			for path, hash := range parsed.Consulted {
				consulted[path] = hash
			}
			warnings = append(warnings, parsed.Warnings...)
			metadata.ScriptReverseImport = cloneStringLists(parsed.ReverseModuleImports)
			for _, indexed := range parsed.Files {
				if input, ok := inputs[indexed.File.Path]; ok {
					state := fileState(i.cfg.ProjectID, input.file, generation)
					outputs[state.Path] = normalizeIndexed(indexed, state)
				}
			}
			failed += fillMissing(outputs, inputs, scriptPaths, i.cfg.ProjectID, generation, "script parser produced no output", &warnings)
		}
	}
	return outputs, sortedUnique(warnings), failed, metadata, consulted
}

func (i *Indexer) embed(ctx context.Context, outputs map[string]core.IndexedFile) ([]string, map[string]bool, error) {
	type chunkRef struct {
		path  string
		index int
	}
	refs := []chunkRef{}
	hashes := []string{}
	for _, path := range sortedOutputPaths(outputs) {
		file := outputs[path]
		for index, chunk := range file.Chunks {
			refs = append(refs, chunkRef{path: path, index: index})
			hashes = append(hashes, chunk.ChunkHash)
		}
	}
	reused, err := i.cfg.Store.LookupEmbeddings(ctx, i.cfg.ProjectID, hashes)
	if err != nil {
		return nil, nil, err
	}
	missing := []chunkRef{}
	missingRefs := map[string][]chunkRef{}
	for _, ref := range refs {
		file := outputs[ref.path]
		chunk := &file.Chunks[ref.index]
		if embedding, ok := reused[chunk.ChunkHash]; ok {
			chunk.Embedding = append([]float32(nil), embedding...)
		} else {
			if len(missingRefs[chunk.ChunkHash]) == 0 {
				missing = append(missing, ref)
			}
			missingRefs[chunk.ChunkHash] = append(missingRefs[chunk.ChunkHash], ref)
		}
		outputs[ref.path] = file
	}
	pending := map[string]bool{}
	if len(missing) == 0 {
		return nil, pending, nil
	}
	if i.cfg.Embedder == nil {
		for _, ref := range missing {
			pending[ref.path] = true
		}
		return []string{"embedding model is unavailable"}, pending, nil
	}
	for start := 0; start < len(missing); start += embeddingBatchSize {
		end := min(start+embeddingBatchSize, len(missing))
		texts := make([]string, end-start)
		for offset, ref := range missing[start:end] {
			texts[offset] = outputs[ref.path].Chunks[ref.index].Content
		}
		embeddings, err := i.cfg.Embedder.Embed(ctx, texts)
		if err != nil || len(embeddings) != len(texts) {
			warning := "embedding unavailable"
			if err != nil {
				warning += ": " + err.Error()
			}
			for _, ref := range missing[start:] {
				for _, duplicate := range missingRefs[outputs[ref.path].Chunks[ref.index].ChunkHash] {
					pending[duplicate.path] = true
				}
			}
			return []string{warning}, pending, nil
		}
		for offset, ref := range missing[start:end] {
			for _, duplicate := range missingRefs[outputs[ref.path].Chunks[ref.index].ChunkHash] {
				file := outputs[duplicate.path]
				file.Chunks[duplicate.index].Embedding = append([]float32(nil), embeddings[offset]...)
				outputs[duplicate.path] = file
			}
		}
	}
	return nil, pending, nil
}

func (i *Indexer) persist(ctx context.Context, current *work, outputs map[string]core.IndexedFile, deleted []string, manifest map[string]core.FileState, generation uint64, consulted, consultedBaseline map[string]string, tracker *Tracker) ([]string, int, error) {
	pending := []string{}
	if changed := i.changedConsulted(ctx, consulted, consultedBaseline); changed != "" {
		return consultedPending(changed, outputs, deleted), 0, nil
	}
	for _, path := range sortedUnique(deleted) {
		if changed := i.changedConsulted(ctx, consulted, consultedBaseline); changed != "" {
			return consultedPending(changed, outputs, deleted), 0, nil
		}
		current.setActivating()
		file := manifest[path]
		file.ProjectID, file.Path, file.ParserVersion, file.Generation, file.Deleted = i.cfg.ProjectID, path, project.ParserVersion, generation, true
		if err := i.cfg.Store.ActivateFile(ctx, file); err != nil {
			return pending, 0, err
		}
	}
	activated := 0
	paths := sortedOutputPaths(outputs)
	for index, path := range paths {
		if err := ctx.Err(); err != nil {
			return pending, activated, err
		}
		if changed := i.changedConsulted(ctx, consulted, consultedBaseline); changed != "" {
			return sortedUnique(append(pending, consultedPending(changed, mapSubset(outputs, paths[index:]), nil)...)), activated, nil
		}
		indexed := outputs[path]
		current.setActivating()
		if err := i.cfg.Store.WriteDerived(ctx, indexed); err != nil {
			return pending, activated, err
		}
		current, reason, err := i.cfg.Source.Inspect(ctx, path)
		if err != nil || reason != "" || current.Hash != indexed.File.Hash {
			pending = append(pending, path)
			continue
		}
		if changed := i.changedConsulted(ctx, consulted, consultedBaseline); changed != "" {
			return sortedUnique(append(pending, consultedPending(changed, mapSubset(outputs, paths[index:]), nil)...)), activated, nil
		}
		if err := i.cfg.Store.ActivateFile(ctx, fileState(i.cfg.ProjectID, current, generation)); err != nil {
			return pending, activated, err
		}
		activated++
		tracker.advance(uint64(index+1+len(deleted)), uint64(len(paths)+len(deleted)), uint64(activated), 0)
	}
	return sortedUnique(pending), activated, nil
}

// changedConsulted is the final ABA fence. Parsers report the exact bytes they
// consumed; activation is allowed only while every such path still has them.
func (i *Indexer) changedConsulted(ctx context.Context, consulted, baseline map[string]string) string {
	for _, filePath := range sortedMapKeys(consulted) {
		if baseline[filePath] != consulted[filePath] {
			return filePath
		}
		data, err := i.cfg.Source.Read(ctx, filePath)
		if err != nil || contentHash(data) != consulted[filePath] {
			return filePath
		}
	}
	return ""
}

func captureHashes(manifest map[string]core.FileState, inputs map[string]capturedFile) map[string]string {
	result := make(map[string]string, len(manifest)+len(inputs))
	for filePath, file := range manifest {
		result[filePath] = file.Hash
	}
	for filePath, input := range inputs {
		result[filePath] = input.file.Hash
	}
	return result
}

func consultedPending(changed string, outputs map[string]core.IndexedFile, deleted []string) []string {
	paths := append([]string{changed}, sortedOutputPaths(outputs)...)
	return sortedUnique(append(paths, deleted...))
}

func mapSubset(outputs map[string]core.IndexedFile, paths []string) map[string]core.IndexedFile {
	result := make(map[string]core.IndexedFile, len(paths))
	for _, filePath := range paths {
		result[filePath] = outputs[filePath]
	}
	return result
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (i *Indexer) writeFailedRun(ctx context.Context, runID string, tracker *Tracker, mode Mode, started time.Time, startGeneration, targetGeneration uint64, plan project.ChangePlan, runErr error) {
	finished := i.cfg.Now()
	run := runSnapshot(runID, i.cfg.ProjectID, mode, started, &finished, startGeneration, targetGeneration, plan, tracker.statusSnapshot(), "failed", runErr.Error())
	_ = i.cfg.Store.WriteRun(ctx, run)
}

func runSnapshot(runID, projectID string, mode Mode, started time.Time, finished *time.Time, startGeneration, targetGeneration uint64, plan project.ChangePlan, status core.IndexStatus, state, runError string) RunSnapshot {
	return RunSnapshot{
		ProjectID: projectID, RunID: runID, Mode: string(mode), State: state,
		Phase: status.Phase, StartedAt: started, FinishedAt: finished, Completed: status.Progress.Completed, Total: status.Progress.Total,
		Changed: status.Progress.Changed, Deleted: status.Progress.Deleted, Skipped: status.Progress.Skipped, Failed: status.Progress.Failed,
		FilesPerSecond: status.Progress.FilesPerSecond, ChunksPerSecond: status.Progress.ChunksPerSecond, ETA: status.Progress.ETA,
		PhaseTimings: status.PhaseTimings, Warnings: status.Warnings, Error: runError, StartGeneration: startGeneration,
		TargetGeneration: targetGeneration, GitHead: plan.Head, DirtyPaths: sortedUnique(plan.DirtyPaths), Pending: sortedUnique(status.Pending),
	}.Clone()
}

func newRunID(reader io.Reader) (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(reader, value[:]); err != nil {
		return "", fmt.Errorf("generate index run UUID: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

func broadcast(current *work, progress core.Progress, mandatory bool) {
	current.mu.Lock()
	waiters := make([]*waiter, 0, len(current.waiters))
	for _, waiter := range current.waiters {
		waiters = append(waiters, waiter)
	}
	current.mu.Unlock()
	for _, waiter := range waiters {
		waiter.deliver(progress, mandatory)
	}
}

type ProjectPlanner struct {
	Root  string
	Index config.Index
}

func (p ProjectPlanner) Plan(ctx context.Context, mode Mode, recordedHead string, manifest map[string]core.FileState, dirty []string) (project.ChangePlan, error) {
	plan, err := project.PlanWithInput(ctx, p.Root, project.PlanInput{RecordedHead: recordedHead, Manifest: manifest, DirtyPaths: dirty, Index: p.Index})
	if err != nil || mode == Delta {
		return plan, err
	}
	files, err := project.DiscoverWithIndex(ctx, p.Root, p.Index)
	if err != nil {
		return project.ChangePlan{}, err
	}
	plan.Changed = files
	return plan, nil
}

type ProjectSource struct {
	Root  string
	Index config.Index
}

func (s ProjectSource) Read(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, reason, err := project.InspectWithIndex(s.Root, path, s.Index); err != nil {
		return nil, err
	} else if reason != "" {
		return nil, errors.New(reason)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(s.Root, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	return os.ReadFile(resolved)
}

func (s ProjectSource) Inspect(ctx context.Context, path string) (project.File, string, error) {
	if err := ctx.Err(); err != nil {
		return project.File{}, "", err
	}
	file, reason, err := project.InspectWithIndex(s.Root, path, s.Index)
	if err != nil || reason != "" {
		if strings.Contains(reason, "no such file") {
			return project.File{}, "", fmt.Errorf("inspect %s: %w", path, os.ErrNotExist)
		}
		return file, reason, err
	}
	data, err := s.Read(ctx, path)
	if err != nil {
		return project.File{}, "", err
	}
	file.Hash = contentHash(data)
	return file, "", nil
}

type StoreAdapter struct{ Store *storepkg.Store }

func NewStoreAdapter(store *storepkg.Store) Store { return StoreAdapter{Store: store} }

func (s StoreAdapter) LoadManifest(ctx context.Context, projectID string) (map[string]core.FileState, error) {
	return s.Store.LoadManifest(ctx, projectID)
}
func (s StoreAdapter) NextGeneration(ctx context.Context, projectID string) (uint64, error) {
	return s.Store.NextGeneration(ctx, projectID)
}
func (s StoreAdapter) LookupEmbeddings(ctx context.Context, projectID string, hashes []string) (map[string][]float32, error) {
	return s.Store.LoadEmbeddings(ctx, projectID, hashes)
}
func (s StoreAdapter) WriteDerived(ctx context.Context, file core.IndexedFile) error {
	return s.Store.WriteDerived(ctx, file)
}
func (s StoreAdapter) ActivateFile(ctx context.Context, file core.FileState) error {
	return s.Store.ActivateFile(ctx, file)
}
func (s StoreAdapter) LoadGraph(ctx context.Context, projectID string) ([]core.CodeUnit, []core.CodeEdge, error) {
	return s.Store.LoadGraph(ctx, projectID)
}
func (s StoreAdapter) LoadLatestRun(ctx context.Context, projectID string) (RunSnapshot, error) {
	run, err := s.Store.LoadLatestSuccessfulRun(ctx, projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return RunSnapshot{}, nil
	}
	if err != nil {
		return RunSnapshot{}, err
	}
	phaseTimings := map[string]time.Duration{}
	for phase, millis := range run.PhaseTimings {
		phaseTimings[phase] = time.Duration(millis) * time.Millisecond
	}
	eta := time.Duration(0)
	if run.ETAMillis != nil {
		eta = time.Duration(*run.ETAMillis) * time.Millisecond
	}
	return RunSnapshot{ProjectID: run.ProjectID, RunID: run.RunID, Mode: run.Mode, State: run.State, Phase: run.Phase, Error: run.Error, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, Completed: run.Completed, Total: run.Total, Changed: run.Changed, Deleted: run.Deleted, Skipped: run.Skipped, Failed: run.Failed, FilesPerSecond: run.FilesPerSecond, ChunksPerSecond: run.ChunksPerSecond, ETA: eta, PhaseTimings: phaseTimings, Warnings: run.Warnings, DirtyPaths: run.DirtyPaths, Pending: run.Pending, StartGeneration: run.StartGeneration, TargetGeneration: run.TargetGeneration, GitHead: run.GitHead}.Clone(), nil
}
func (s StoreAdapter) LoadEmbeddingPending(ctx context.Context, projectID string) ([]string, error) {
	return s.Store.LoadEmbeddingPending(ctx, projectID)
}
func (s StoreAdapter) CleanupObsolete(ctx context.Context, projectID string) error {
	return s.Store.CleanupObsolete(ctx, projectID)
}
func (s StoreAdapter) WriteRun(ctx context.Context, run RunSnapshot) error {
	converted := storepkg.Run{
		ProjectID: run.ProjectID, RunID: run.RunID, Mode: run.Mode, State: run.State, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
		Phase: run.Phase, Completed: run.Completed, Total: run.Total, Changed: run.Changed, Deleted: run.Deleted, Skipped: run.Skipped,
		Failed: run.Failed, FilesPerSecond: run.FilesPerSecond, ChunksPerSecond: run.ChunksPerSecond, PhaseTimings: map[string]uint64{},
		Warnings: append([]string(nil), run.Warnings...), Error: run.Error, StartGeneration: run.StartGeneration,
		TargetGeneration: run.TargetGeneration, GitHead: run.GitHead, DirtyPaths: append([]string(nil), run.DirtyPaths...), Pending: append([]string(nil), run.Pending...),
	}
	if run.ETA > 0 {
		value := uint64(run.ETA / time.Millisecond)
		converted.ETAMillis = &value
	}
	for phase, duration := range run.PhaseTimings {
		converted.PhaseTimings[phase] = uint64(duration / time.Millisecond)
	}
	return s.Store.WriteRun(ctx, converted)
}

type markdownAdapter struct{}

func (markdownAdapter) Parse(path string, data []byte, hash string) ([]core.DocChunk, []string, error) {
	return markdown.Parse(path, data, hash)
}

func normalizeIndexed(indexed core.IndexedFile, state core.FileState) core.IndexedFile {
	indexed.File = state
	indexed.Warnings = sortedUnique(indexed.Warnings)
	for index := range indexed.Units {
		indexed.Units[index].Path = state.Path
		indexed.Units[index].FileHash = state.Hash
		indexed.Units[index].Generation = state.Generation
	}
	for index := range indexed.Edges {
		indexed.Edges[index].Path = state.Path
		indexed.Edges[index].FileHash = state.Hash
		indexed.Edges[index].Generation = state.Generation
	}
	for index := range indexed.Chunks {
		indexed.Chunks[index].Path = state.Path
		indexed.Chunks[index].FileHash = state.Hash
		indexed.Chunks[index].Generation = state.Generation
	}
	return indexed
}

func fillMissing(outputs map[string]core.IndexedFile, inputs map[string]capturedFile, paths []string, projectID string, generation uint64, message string, warnings *[]string) int {
	failed := 0
	for _, path := range paths {
		if _, exists := outputs[path]; exists {
			continue
		}
		warning := path + ": " + message
		input := inputs[path]
		outputs[path] = core.IndexedFile{File: fileState(projectID, input.file, generation), Warnings: []string{warning}}
		*warnings = append(*warnings, warning)
		failed++
	}
	return failed
}

func fileState(projectID string, file project.File, generation uint64) core.FileState {
	return core.FileState{ProjectID: projectID, Path: file.Path, Kind: file.Kind, Language: file.Language, Extension: file.Extension,
		Size: file.Size, MTimeNS: file.MTimeNS, Hash: file.Hash, ParserVersion: project.ParserVersion, Generation: generation}
}

func projectFile(file core.FileState) project.File {
	return project.File{Path: file.Path, Kind: file.Kind, Language: file.Language, Extension: file.Extension, Size: file.Size, MTimeNS: file.MTimeNS, Hash: file.Hash}
}

func packagePatterns(paths []string, metadata graph.ChangeMetadata) []string {
	patterns := []string{}
	for _, filePath := range paths {
		if packagePath := metadata.GoFilePackage[filePath]; packagePath != "" {
			patterns = append(patterns, packagePath)
			continue
		}
		dir := path.Dir(filePath)
		if dir == "." {
			patterns = append(patterns, ".")
		} else {
			patterns = append(patterns, "./"+dir)
		}
	}
	return sortedUnique(patterns)
}

func supported(extension string) bool {
	return extension == ".go" || extension == ".md" || scriptExtension(extension)
}

func scriptExtension(extension string) bool {
	switch extension {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

func contentHash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func capturedSources(inputs map[string]capturedFile, paths []string) map[string][]byte {
	if paths == nil {
		paths = make([]string, 0, len(inputs))
		for path := range inputs {
			paths = append(paths, path)
		}
	}
	result := make(map[string][]byte, len(paths))
	for _, path := range paths {
		if input, ok := inputs[path]; ok {
			result[path] = append([]byte(nil), input.data...)
		}
	}
	return result
}

func chunkCount(outputs map[string]core.IndexedFile) int {
	total := 0
	for _, output := range outputs {
		total += len(output.Chunks)
	}
	return total
}

func sortedOutputPaths(outputs map[string]core.IndexedFile) []string {
	paths := make([]string, 0, len(outputs))
	for path := range outputs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func sortedUnique(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	compact := result[:0]
	for _, value := range result {
		if value != "" && (len(compact) == 0 || compact[len(compact)-1] != value) {
			compact = append(compact, value)
		}
	}
	return compact
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func cloneManifest(source map[string]core.FileState) map[string]core.FileState {
	result := make(map[string]core.FileState, len(source))
	for path, file := range source {
		result[path] = file
	}
	return result
}

func cloneSyncResult(result core.SyncResult) core.SyncResult {
	result.Pending = append([]string(nil), result.Pending...)
	result.Warnings = append([]string(nil), result.Warnings...)
	return result
}

func filterPaths(pending, requested []string) []string {
	if len(requested) == 0 {
		return sortedUnique(pending)
	}
	result := []string{}
	for _, pendingPath := range pending {
		for _, requestedPath := range requested {
			if pendingPath == requestedPath || strings.HasPrefix(pendingPath, strings.TrimSuffix(requestedPath, "/")+"/") {
				result = append(result, pendingPath)
				break
			}
		}
	}
	return sortedUnique(result)
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func emptyMetadata() graph.ChangeMetadata {
	return graph.ChangeMetadata{GoFilePackage: map[string]string{}, GoReverseImport: map[string][]string{}, ScriptReverseImport: map[string][]string{}, SurfaceChanged: map[string]bool{}, ResolutionScope: map[string][]string{}}
}

func cloneMetadata(source graph.ChangeMetadata) graph.ChangeMetadata {
	return graph.ChangeMetadata{GoFilePackage: cloneStringMap(source.GoFilePackage), GoReverseImport: cloneStringLists(source.GoReverseImport),
		ScriptReverseImport: cloneStringLists(source.ScriptReverseImport), SurfaceChanged: cloneBoolMap(source.SurfaceChanged), ResolutionScope: cloneStringLists(source.ResolutionScope)}
}

func mergeMetadata(current, update graph.ChangeMetadata) graph.ChangeMetadata {
	result := cloneMetadata(current)
	for path, packagePath := range update.GoFilePackage {
		result.GoFilePackage[path] = packagePath
	}
	for packagePath, reverse := range update.GoReverseImport {
		result.GoReverseImport[packagePath] = append([]string(nil), reverse...)
	}
	for path, reverse := range update.ScriptReverseImport {
		result.ScriptReverseImport[path] = append([]string(nil), reverse...)
	}
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneStringLists(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for key, value := range source {
		result[key] = append([]string(nil), value...)
	}
	return result
}

func runStatus(run RunSnapshot, manifest map[string]core.FileState) core.IndexStatus {
	active := uint64(0)
	for _, file := range manifest {
		if file.Generation > active {
			active = file.Generation
		}
	}
	if len(run.Pending) == 0 && run.TargetGeneration > active {
		active = run.TargetGeneration
	} else if len(run.Pending) != 0 && run.StartGeneration > active {
		active = run.StartGeneration
	}
	lastSuccess := time.Time{}
	if run.FinishedAt != nil {
		lastSuccess = *run.FinishedAt
	}
	state := run.State
	if state == "" {
		state = "idle"
	}
	return core.IndexStatus{State: state, Phase: run.Phase, ActiveGeneration: active, TargetGeneration: run.TargetGeneration, LastSuccess: lastSuccess, Pending: append([]string(nil), run.Pending...), Warnings: append([]string(nil), run.Warnings...), PhaseTimings: cloneDurations(run.PhaseTimings), Progress: core.Progress{Completed: run.Completed, Total: run.Total, Changed: run.Changed, Deleted: run.Deleted, Skipped: run.Skipped, Failed: run.Failed, FilesPerSecond: run.FilesPerSecond, ChunksPerSecond: run.ChunksPerSecond, ETA: run.ETA}}
}

func cloneDurations(values map[string]time.Duration) map[string]time.Duration {
	result := make(map[string]time.Duration, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func metadataFromActive(manifest map[string]core.FileState, units []core.CodeUnit, edges []core.CodeEdge) graph.ChangeMetadata {
	result := emptyMetadata()
	byID := map[string]core.CodeUnit{}
	packageByID := map[string]string{}
	fileByID := map[string]string{}
	for _, unit := range units {
		byID[unit.ID] = unit
		if unit.Kind == "package" && unit.Language == "go" {
			packageByID[unit.ID] = unit.QualifiedName
		}
		if unit.Kind == "file" {
			fileByID[unit.ID] = unit.Path
		}
	}
	// Go packages may share a directory (notably external test packages), so
	// reconstruct ownership from package->file contains edges, never directory.
	for _, edge := range edges {
		if edge.Relation != "contains" {
			continue
		}
		if packageName := packageByID[edge.SourceID]; packageName != "" {
			if filePath := fileByID[edge.TargetID]; filePath != "" {
				result.GoFilePackage[filePath] = packageName
			}
		}
	}
	for _, edge := range edges {
		if edge.Relation != "imports" {
			continue
		}
		source, target := byID[edge.SourceID], byID[edge.TargetID]
		if source.Language == "go" {
			if sourcePackage, targetPackage := result.GoFilePackage[source.Path], packageByID[edge.TargetID]; sourcePackage != "" && targetPackage != "" {
				result.GoReverseImport[targetPackage] = append(result.GoReverseImport[targetPackage], sourcePackage)
			}
		}
		if scriptExtension(source.Extension) && scriptExtension(target.Extension) {
			result.ScriptReverseImport[target.Path] = append(result.ScriptReverseImport[target.Path], source.Path)
		}
	}
	for filePath, file := range manifest {
		if file.Extension == ".go" && result.GoFilePackage[filePath] == "" {
			// Missing graph rows are conservative: a direct-file delta remains safe.
			result.GoFilePackage[filePath] = filePath
		}
	}
	return mergeMetadata(emptyMetadata(), result)
}
