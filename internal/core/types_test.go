package core

import (
	"strings"
	"testing"
	"time"
)

func TestProgressMessageIncludesOperationalMetrics(t *testing.T) {
	message := Progress{
		Phase:           "indexing",
		Elapsed:         2 * time.Second,
		ETA:             time.Second,
		FilesPerSecond:  3.5,
		ChunksPerSecond: 7.25,
	}.Message()
	for _, want := range []string{"indexing", "elapsed", "2s", "files/s", "3.50", "chunks/s", "7.25", "eta", "1s"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q does not contain %q", message, want)
		}
	}
}
