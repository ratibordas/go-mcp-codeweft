package indexer

import (
	"testing"
	"time"
)

func TestEstimateETAUsesCurrentThenHistoricRate(t *testing.T) {
	if got := estimateETA(2, 10, 4, 1); got != 2*time.Second {
		t.Fatalf("current ETA = %s", got)
	}
	if got := estimateETA(2, 10, 0, 2); got != 4*time.Second {
		t.Fatalf("historic ETA = %s", got)
	}
	if got := estimateETA(10, 10, 4, 2); got != 0 {
		t.Fatalf("complete ETA = %s", got)
	}
}

func TestStatusReturnsDefensiveCopies(t *testing.T) {
	tracker := newTracker(time.Now, nil)
	tracker.begin(3)
	tracker.phase("scan", 1)
	tracker.warn("first")
	first := tracker.statusSnapshot()
	first.Warnings[0] = "mutated"
	first.PhaseTimings["scan"] = time.Hour
	second := tracker.statusSnapshot()
	if second.Warnings[0] != "first" || second.PhaseTimings["scan"] == time.Hour {
		t.Fatalf("tracker leaked mutable state: %+v", second)
	}
}
