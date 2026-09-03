package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOperationalResultsUseStableJSONFieldNames(t *testing.T) {
	data, err := json.Marshal(struct {
		Status IndexStatus `json:"status"`
		Sync   SyncResult  `json:"sync"`
	}{Status: IndexStatus{ActiveGeneration: 3}, Sync: SyncResult{Generation: 4}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"active_generation":3`) || !strings.Contains(text, `"generation":4`) || strings.Contains(text, "ActiveGeneration") {
		t.Fatalf("JSON = %s", text)
	}
}

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
