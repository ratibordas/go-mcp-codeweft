package retrieval

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

const (
	rrfConstant          = 60.0
	candidateTokenBudget = 12_000
)

var (
	quotedTerm  = regexp.MustCompile(`["']([^"']+)["']`)
	headingTerm = regexp.MustCompile(`(?:\b[A-Z][A-Za-z0-9_-]*\s+){1,}[A-Z][A-Za-z0-9_-]*\b`)
	termToken   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+|(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+|[A-Za-z_][A-Za-z0-9_]*`)
)

func extractTerms(question string) []string {
	seen := map[string]bool{}
	terms := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			terms = append(terms, value)
		}
	}
	for _, match := range quotedTerm.FindAllStringSubmatch(question, -1) {
		add(match[1])
	}
	for _, heading := range headingTerm.FindAllString(question, -1) {
		add(heading)
	}
	for _, token := range termToken.FindAllString(question, -1) {
		if strings.ContainsAny(token, "/._") || identifierShaped(token) {
			add(token)
		}
		if strings.Contains(token, ".") && !strings.Contains(token, "/") {
			for _, part := range strings.Split(token, ".") {
				if identifierShaped(part) {
					add(part)
				}
			}
		}
	}
	return terms
}

func identifierShaped(value string) bool {
	if strings.Contains(value, "_") || value == "" {
		return value != ""
	}
	for index, r := range value {
		if r >= 'A' && r <= 'Z' && index > 0 {
			return true
		}
	}
	return value[0] >= 'A' && value[0] <= 'Z'
}

func rank(lists [][]core.Candidate, terms []string) []core.Candidate {
	byID := map[string]core.Candidate{}
	for _, list := range lists {
		for index, candidate := range list {
			candidate = markExact(candidate, terms)
			key := candidateKey(candidate)
			current, exists := byID[key]
			if !exists {
				current = candidate
				current.Score = 0
			}
			current.Score += 1 / (rrfConstant + float64(index+1))
			if current.Match == "" {
				current.Match = candidate.Match
			}
			byID[key] = current
		}
	}
	result := make([]core.Candidate, 0, len(byID))
	for _, candidate := range byID {
		candidate.Score = boostedScore(candidate, terms)
		result = append(result, candidate)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		if result[i].StartLine != result[j].StartLine {
			return result[i].StartLine < result[j].StartLine
		}
		return result[i].ID < result[j].ID
	})
	return diversify(result)
}

func markExact(candidate core.Candidate, terms []string) core.Candidate {
	for _, term := range terms {
		if term == candidate.Path || term == path.Base(candidate.Path) {
			candidate.Match = "exact_path"
			return candidate
		}
	}
	for _, term := range terms {
		if term == candidate.Symbol || strings.HasSuffix(candidate.Symbol, "."+term) {
			candidate.Match = "exact_symbol"
			return candidate
		}
	}
	return candidate
}

func boostedScore(candidate core.Candidate, terms []string) float64 {
	boost := 0.0
	for _, term := range terms {
		if term == candidate.Path || term == path.Base(candidate.Path) {
			boost = max(boost, 2)
		}
		if term == candidate.Symbol || strings.HasSuffix(candidate.Symbol, "."+term) {
			boost = max(boost, 1.5)
		}
		if candidate.Heading != "" && strings.Contains(strings.ToLower(candidate.Heading), strings.ToLower(term)) {
			boost = max(boost, .5)
		}
	}
	if candidate.Match == "exact_path" {
		boost = max(boost, 2)
	}
	if candidate.Match == "exact_symbol" {
		boost = max(boost, 1.5)
	}
	if candidate.Match == "graph_distance_1" {
		boost += 1
	}
	score := (candidate.Score + boost) * sourceWeight(candidate)
	if candidate.Match == "graph_distance_2" {
		score *= .75
	}
	return score
}

func sourceWeight(candidate core.Candidate) float64 {
	if candidate.Weight > 0 {
		return candidate.Weight
	}
	lower := strings.ToLower(candidate.Path)
	if strings.HasSuffix(lower, ".d.ts") {
		return .5
	}
	base := path.Base(lower)
	if strings.Contains(base, "_test.") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return .6
	}
	return 1
}

func diversify(candidates []core.Candidate) []core.Candidate {
	counts := map[string]int{}
	primary := make([]core.Candidate, 0, len(candidates))
	deferred := make([]core.Candidate, 0)
	for _, candidate := range candidates {
		if candidate.Path != "" && counts[candidate.Path] >= 3 {
			deferred = append(deferred, candidate)
			continue
		}
		counts[candidate.Path]++
		primary = append(primary, candidate)
	}
	return append(primary, deferred...)
}

func withinCandidateBudget(candidates []core.Candidate, budget int) []core.Candidate {
	used := 0
	result := make([]core.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		tokens := (utf8.RuneCountInString(candidate.Content) + 3) / 4
		if used+tokens > budget {
			continue
		}
		used += tokens
		result = append(result, candidate)
	}
	return result
}

func normalizedMaxTokens(value int) int {
	if value == 0 {
		return 3500
	}
	return min(max(value, 256), 12000)
}

func candidateKey(candidate core.Candidate) string {
	if candidate.ID != "" {
		return candidate.ID
	}
	return strings.Join([]string{candidate.Type, candidate.Path, candidate.Symbol, candidate.Heading, candidate.Relation}, "\x00")
}
