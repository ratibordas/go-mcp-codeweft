package indexer

import (
	"sync"
	"time"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

const progressInterval = 100 * time.Millisecond

type Tracker struct {
	mu sync.RWMutex

	now          func() time.Time
	history      map[string]float64
	status       core.IndexStatus
	started      time.Time
	phaseStarted time.Time
	lastEmit     time.Time
	emit         func(core.Progress)
}

func NewTracker(history map[string]float64) *Tracker {
	return newTracker(time.Now, history)
}

func newTracker(now func() time.Time, history map[string]float64) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{
		now: now, history: cloneRates(history),
		status: core.IndexStatus{State: "idle", PhaseTimings: map[string]time.Duration{}},
	}
}

func (t *Tracker) begin(generation uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.started, t.phaseStarted, t.lastEmit = now, now, time.Time{}
	t.status = core.IndexStatus{
		State: "indexing", TargetGeneration: generation,
		PhaseTimings: map[string]time.Duration{},
	}
}

func (t *Tracker) phase(phase string, total uint64) {
	t.mu.Lock()
	now := t.now()
	if previous := t.status.Phase; previous != "" {
		t.status.PhaseTimings[previous] += now.Sub(t.phaseStarted)
	}
	t.phaseStarted = now
	t.status.Phase = phase
	t.status.Progress.Phase = phase
	t.status.Progress.Completed = 0
	t.status.Progress.Total = total
	t.status.Progress.ETA = estimateETA(0, total, 0, t.history[phase])
	t.status.Progress.Elapsed = now.Sub(t.started)
	t.lastEmit = now
	progress, emit := t.status.Progress, t.emit
	t.mu.Unlock()
	if emit != nil {
		emit(progress)
	}
}

func (t *Tracker) advance(completed, total, files, chunks uint64) {
	t.mu.Lock()
	now := t.now()
	phaseElapsed := now.Sub(t.phaseStarted)
	currentRate := 0.0
	if phaseElapsed > 0 {
		currentRate = float64(completed) / phaseElapsed.Seconds()
	}
	t.status.Progress.Completed = completed
	t.status.Progress.Total = total
	t.status.Progress.Elapsed = now.Sub(t.started)
	t.status.Progress.ETA = estimateETA(completed, total, currentRate, t.history[t.status.Phase])
	if elapsed := t.status.Progress.Elapsed.Seconds(); elapsed > 0 {
		t.status.Progress.FilesPerSecond = float64(files) / elapsed
		t.status.Progress.ChunksPerSecond = float64(chunks) / elapsed
	}
	if !t.lastEmit.IsZero() && now.Sub(t.lastEmit) < progressInterval {
		t.mu.Unlock()
		return
	}
	t.lastEmit = now
	progress, emit := t.status.Progress, t.emit
	t.mu.Unlock()
	if emit != nil {
		emit(progress)
	}
}

func (t *Tracker) counts(changed, deleted, skipped, failed uint64) {
	t.mu.Lock()
	t.status.Progress.Changed = changed
	t.status.Progress.Deleted = deleted
	t.status.Progress.Skipped = skipped
	t.status.Progress.Failed = failed
	t.mu.Unlock()
}

func (t *Tracker) activeGeneration(generation uint64) {
	t.mu.Lock()
	t.status.ActiveGeneration = generation
	t.mu.Unlock()
}

func (t *Tracker) warn(warning string) {
	if warning == "" {
		return
	}
	t.mu.Lock()
	t.status.Warnings = appendUnique(t.status.Warnings, warning)
	t.mu.Unlock()
}

func (t *Tracker) pending(paths []string) {
	t.mu.Lock()
	t.status.Pending = sortedUnique(paths)
	t.mu.Unlock()
}

func (t *Tracker) finish(generation uint64, degraded bool, err error) {
	t.mu.Lock()
	now := t.now()
	if t.status.Phase != "" {
		t.status.PhaseTimings[t.status.Phase] += now.Sub(t.phaseStarted)
	}
	t.status.Progress.Elapsed = now.Sub(t.started)
	if err != nil {
		t.status.State = "degraded"
		t.status.LastError = err.Error()
	} else {
		t.status.ActiveGeneration = generation
		t.status.LastSuccess = now
		if degraded || len(t.status.Warnings) != 0 || len(t.status.Pending) != 0 || t.status.Progress.Failed != 0 {
			t.status.State = "degraded"
		} else {
			t.status.State = "ready"
		}
	}
	t.mu.Unlock()
}

// emitFinal publishes the terminal snapshot before the owning work signals
// completion. Unlike advance, it is never throttled.
func (t *Tracker) emitFinal() {
	t.mu.RLock()
	progress, emit := t.status.Progress, t.emit
	t.mu.RUnlock()
	if emit != nil {
		emit(progress)
	}
}

func (t *Tracker) statusSnapshot() core.IndexStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return cloneStatus(t.status)
}

func (t *Tracker) runStatusSnapshot() core.IndexStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	status := cloneStatus(t.status)
	now := t.now()
	if status.Phase != "" {
		status.PhaseTimings[status.Phase] += now.Sub(t.phaseStarted)
	}
	status.Progress.Elapsed = now.Sub(t.started)
	return status
}

func (t *Tracker) setEmitter(emit func(core.Progress)) {
	t.mu.Lock()
	t.emit = emit
	t.mu.Unlock()
}

func (t *Tracker) restore(status core.IndexStatus) {
	t.mu.Lock()
	t.status = cloneStatus(status)
	t.mu.Unlock()
}

func estimateETA(completed, total uint64, currentRate, historicRate float64) time.Duration {
	rate := currentRate
	if rate == 0 {
		rate = historicRate
	}
	if rate <= 0 || total <= completed {
		return 0
	}
	return time.Duration(float64(total-completed) / rate * float64(time.Second))
}

func cloneStatus(status core.IndexStatus) core.IndexStatus {
	status.Pending = append([]string(nil), status.Pending...)
	status.Warnings = append([]string(nil), status.Warnings...)
	status.PhaseTimings = make(map[string]time.Duration, len(status.PhaseTimings))
	for phase, duration := range status.PhaseTimings {
		status.PhaseTimings[phase] = duration
	}
	return status
}

func cloneRates(rates map[string]float64) map[string]float64 {
	result := make(map[string]float64, len(rates))
	for phase, rate := range rates {
		result[phase] = rate
	}
	return result
}
