package retrieval

import (
	"strings"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func TestExtractTermsFindsPathsAndSymbols(t *testing.T) {
	got := extractTerms("Add \"docs/API guide.md\" under # API Guide for crm.UserService.GetUser and user_id in src/http.tsx")
	want := []string{"docs/API guide.md", "crm.UserService.GetUser", "user_id", "src/http.tsx", "GetUser", "UserService", "API Guide"}
	for _, term := range want {
		if !contains(got, term) {
			t.Fatalf("missing %q in %v", term, got)
		}
	}
}

func TestMaxTokensDefaultsAndClamps(t *testing.T) {
	if normalizedMaxTokens(0) != 3500 || normalizedMaxTokens(1) != 256 || normalizedMaxTokens(20_000) != 12_000 {
		t.Fatal("max token bounds changed")
	}
}

func TestRRFKeepsExactSymbolFirst(t *testing.T) {
	lists := [][]core.Candidate{
		{{ID: "C1", Symbol: "GetUser", Match: "exact_symbol", Weight: 1}, {ID: "C2", Weight: 1}},
		{{ID: "C2", Weight: 1}, {ID: "C1", Symbol: "GetUser", Weight: 1}},
	}
	got := rank(lists, []string{"GetUser"})
	if got[0].ID != "C1" {
		t.Fatalf("exact symbol lost priority: %+v", got)
	}
}

func TestRankAppliesWeightsGraphDistanceAndDiversification(t *testing.T) {
	lists := [][]core.Candidate{{
		{ID: "prod", Path: "prod.go", Weight: 1},
		{ID: "test", Path: "prod_test.go", Weight: .6},
		{ID: "decl", Path: "types.d.ts", Weight: .5},
		{ID: "near", Path: "near.go", Match: "graph_distance_1", Weight: 1},
		{ID: "far", Path: "far.go", Match: "graph_distance_2", Weight: 1},
		{ID: "same1", Path: "same.go", Weight: 1},
		{ID: "same2", Path: "same.go", Weight: 1},
		{ID: "same3", Path: "same.go", Weight: 1},
		{ID: "same4", Path: "same.go", Weight: 1},
		{ID: "other", Path: "other.go", Weight: 1},
	}}
	got := rank(lists, nil)
	if score(got, "prod") <= score(got, "test") || score(got, "test") <= score(got, "decl") {
		t.Fatalf("source weights not applied: %+v", got)
	}
	if score(got, "near") <= score(got, "far") {
		t.Fatalf("graph distance penalty not applied: %+v", got)
	}
	positions := map[string]int{}
	for index, candidate := range got {
		positions[candidate.ID] = index
	}
	if positions["other"] > positions["same4"] {
		t.Fatalf("fourth same-file candidate was not deferred: %+v", got)
	}
}

func TestBudgetKeepsCompleteCandidatesUnderTwelveThousandTokens(t *testing.T) {
	content := strings.Repeat("x", 24_000)
	got := withinCandidateBudget([]core.Candidate{
		{ID: "one", Content: content},
		{ID: "two", Content: content},
		{ID: "three", Content: content},
	}, candidateTokenBudget)
	if len(got) != 2 || got[1].Content != content {
		t.Fatalf("budgeted candidates = %d", len(got))
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func score(values []core.Candidate, id string) float64 {
	for _, value := range values {
		if value.ID == id {
			return value.Score
		}
	}
	return -1
}
