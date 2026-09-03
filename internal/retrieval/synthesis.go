package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

var (
	expansionSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["terms"],"properties":{"terms":{"type":"array","maxItems":6,"items":{"type":"string"}}}}`)
	synthesisSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["summary","citations"],"properties":{"summary":{"type":"string"},"citations":{"type":"array","items":{"type":"string"}}}}`)
)

type expansionResponse struct {
	Terms []string `json:"terms"`
}

type synthesisResponse struct {
	Summary   string   `json:"summary"`
	Citations []string `json:"citations"`
}

func (s *Service) SearchContext(ctx context.Context, req core.SearchRequest, sink core.ProgressSink) (core.ContextResult, error) {
	started := time.Now()
	retrieved, err := s.Retrieve(ctx, req, sink)
	if err != nil {
		return core.ContextResult{}, err
	}
	warnings := append([]string(nil), retrieved.Warnings...)
	generationDuration := time.Duration(0)
	if s.cfg.Generator != nil && weakRecall(retrieved.Candidates) {
		callStarted := time.Now()
		terms, expansionErr := s.expand(ctx, req.Question)
		generationDuration += time.Since(callStarted)
		if expansionErr != nil {
			warnings = append(warnings, "query expansion unavailable: "+expansionErr.Error())
		} else if len(terms) != 0 {
			expandedRequest := req
			expandedRequest.Question = req.Question + "\n" + strings.Join(terms, "\n")
			expanded, retrieveErr := s.Retrieve(ctx, expandedRequest, sink)
			if retrieveErr != nil {
				return core.ContextResult{}, retrieveErr
			}
			retrieved.Indexing += expanded.Indexing
			retrieved.Retrieval += expanded.Retrieval
			retrieved.Candidates = expanded.Candidates
			warnings = append(warnings, expanded.Warnings...)
		}
	}
	candidates := labelCandidates(retrieved.Candidates)
	citations := candidateIDs(candidates)
	summary := "Relevant evidence is listed below."
	if s.cfg.Generator != nil && len(candidates) != 0 {
		callStarted := time.Now()
		generatedSummary, generatedCitations, synthesisErr := s.synthesize(ctx, req.Question, candidates)
		generationDuration += time.Since(callStarted)
		if synthesisErr != nil {
			warnings = append(warnings, "synthesis unavailable: "+synthesisErr.Error())
		} else if len(generatedCitations) == 0 {
			warnings = append(warnings, "synthesis returned no valid citations")
		} else {
			summary, citations = generatedSummary, generatedCitations
		}
	}
	manifest := s.cfg.Freshener.Manifest()
	evidenceByID := make(map[string]core.Evidence, len(citations))
	for _, candidate := range candidates {
		if !containsString(citations, candidate.ID) {
			continue
		}
		evidence, evidenceErr := extractEvidence(s.cfg.Root, candidate, manifest)
		if evidenceErr != nil {
			warnings = append(warnings, evidenceErr.Error())
			continue
		}
		evidenceByID[candidate.ID] = evidence
	}
	validCitations := make([]string, 0, len(citations))
	for _, id := range citations {
		if _, ok := evidenceByID[id]; ok {
			validCitations = append(validCitations, id)
		}
	}
	summary = removeRejectedMarkers(summary, stringSet(validCitations))
	requested := req.MaxTokens
	if requested == 0 {
		requested = s.cfg.MaxTokens
	}
	requested = normalizedMaxTokens(requested)
	summary, evidence, used, truncated := budgetEvidence(summary, validCitations, evidenceByID, requested)
	status := s.cfg.Freshener.Status()
	state := status.State
	if state == "" {
		state = "ready"
	}
	if len(status.Pending) != 0 {
		state = "degraded"
	}
	sort.Strings(warnings)
	return core.ContextResult{
		Summary: summary, Evidence: evidence, Warnings: unique(warnings),
		Freshness: core.Freshness{Generation: retrieved.Generation, State: state, Pending: status.Pending},
		Timing:    core.Timing{Indexing: retrieved.Indexing, Retrieval: retrieved.Retrieval, Generation: generationDuration, Total: time.Since(started)},
		Budget:    core.Budget{Requested: requested, Used: used, Truncated: truncated},
	}, nil
}

func (s *Service) expand(ctx context.Context, question string) ([]string, error) {
	prompt := "Return JSON only. Suggest at most six short search terms for this codebase question. Do not return paths or shell syntax.\nQuestion: " + question
	response, err := s.cfg.Generator.Generate(ctx, core.GenerateRequest{Prompt: prompt, Schema: expansionSchema})
	if err != nil {
		return nil, err
	}
	var decoded expansionResponse
	if err := decodeStrict(response, &decoded); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	terms := make([]string, 0, min(len(decoded.Terms), 6))
	for _, term := range decoded.Terms {
		term = strings.TrimSpace(term)
		if !safeExpansionTerm(term) || seen[term] {
			continue
		}
		seen[term] = true
		terms = append(terms, term)
		if len(terms) == 6 {
			break
		}
	}
	return terms, nil
}

func (s *Service) synthesize(ctx context.Context, question string, candidates []core.Candidate) (string, []string, error) {
	type promptCandidate struct {
		ID, Type, Path, Symbol, Heading, Content string
		StartLine, EndLine                       uint32
	}
	const instruction = "Return JSON only. Answer using only the supplied evidence. Cite only supplied IDs in the summary. Do not create snippets or quotes.\nQuestion: "
	items := make([]promptCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		item := promptCandidate{
			ID: candidate.ID, Type: candidate.Type, Path: candidate.Path, Symbol: candidate.Symbol,
			Heading: candidate.Heading, Content: candidate.Content, StartLine: candidate.StartLine, EndLine: candidate.EndLine,
		}
		prospective := append(append([]promptCandidate(nil), items...), item)
		encoded, marshalErr := json.Marshal(prospective)
		if marshalErr != nil {
			return "", nil, marshalErr
		}
		if estimatedTokens(instruction+question+"\nEvidence: "+string(encoded)) > candidateTokenBudget {
			continue
		}
		items = prospective
	}
	if len(items) == 0 {
		return "", nil, fmt.Errorf("no candidate fits the synthesis input budget")
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return "", nil, err
	}
	prompt := instruction + question + "\nEvidence: " + string(encoded)
	response, err := s.cfg.Generator.Generate(ctx, core.GenerateRequest{Prompt: prompt, Schema: synthesisSchema})
	if err != nil {
		return "", nil, err
	}
	var decoded synthesisResponse
	if err := decodeStrict(response, &decoded); err != nil {
		return "", nil, err
	}
	allowed := map[string]bool{}
	for _, item := range items {
		allowed[item.ID] = true
	}
	markers := citationMarkers(decoded.Summary)
	valid := []string{}
	seen := map[string]bool{}
	for _, id := range decoded.Citations {
		if allowed[id] && markers[id] && !seen[id] {
			seen[id] = true
			valid = append(valid, id)
		}
	}
	return removeRejectedMarkers(decoded.Summary, stringSet(valid)), valid, nil
}

func weakRecall(candidates []core.Candidate) bool {
	strong := 0
	for _, candidate := range candidates {
		if candidate.Match == "exact_path" || candidate.Match == "exact_symbol" {
			return false
		}
		if candidate.Score > .02 {
			strong++
		}
	}
	return strong < 5
}

func safeExpansionTerm(term string) bool {
	count := utf8.RuneCountInString(term)
	if count < 2 || count > 80 || strings.ContainsAny(term, `/\\;|&$`+"`"+`()<>`) {
		return false
	}
	for _, r := range term {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func decodeStrict(data string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode model JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode model JSON: trailing data")
		}
		return fmt.Errorf("decode model JSON: %w", err)
	}
	return nil
}

func labelCandidates(candidates []core.Candidate) []core.Candidate {
	result := append([]core.Candidate(nil), candidates...)
	code, docs := 0, 0
	for index := range result {
		if result[index].Extension == ".md" || result[index].Type == "doc" || result[index].Type == "documentation" {
			docs++
			result[index].ID = fmt.Sprintf("D%d", docs)
		} else {
			code++
			result[index].ID = fmt.Sprintf("C%d", code)
		}
	}
	return result
}

func candidateIDs(candidates []core.Candidate) []string {
	result := make([]string, len(candidates))
	for index := range candidates {
		result[index] = candidates[index].ID
	}
	return result
}

func citationMarkers(summary string) map[string]bool {
	markers := map[string]bool{}
	for start := 0; start < len(summary); {
		open := strings.IndexByte(summary[start:], '[')
		if open < 0 {
			break
		}
		open += start
		close := strings.IndexByte(summary[open+1:], ']')
		if close < 0 {
			break
		}
		close += open + 1
		if id := summary[open+1 : close]; id != "" && !strings.ContainsAny(id, " []\t\r\n") {
			markers[id] = true
		}
		start = close + 1
	}
	return markers
}

func removeRejectedMarkers(summary string, allowed map[string]bool) string {
	for id := range citationMarkers(summary) {
		if !allowed[id] {
			summary = strings.ReplaceAll(summary, "["+id+"]", "")
		}
	}
	return strings.TrimSpace(summary)
}

func budgetEvidence(summary string, citations []string, byID map[string]core.Evidence, budget int) (string, []core.Evidence, int, bool) {
	used := estimatedTokens(summary)
	truncated := false
	if used > budget {
		summary, used, truncated = "", 0, true
	}
	result := make([]core.Evidence, 0, len(citations))
	included := map[string]bool{}
	for _, id := range citations {
		evidence := byID[id]
		encoded, _ := json.Marshal(evidence)
		tokens := estimatedTokens(string(encoded))
		if used+tokens > budget {
			truncated = true
			continue
		}
		used += tokens
		result = append(result, evidence)
		included[id] = true
	}
	summary = removeRejectedMarkers(summary, included)
	used = estimatedTokens(summary)
	for _, evidence := range result {
		encoded, _ := json.Marshal(evidence)
		used += estimatedTokens(string(encoded))
	}
	return summary, result, used, truncated
}

func estimatedTokens(value string) int { return (utf8.RuneCountInString(value) + 3) / 4 }

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
