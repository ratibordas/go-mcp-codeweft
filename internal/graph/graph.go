package graph

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

const (
	Upstream   = "upstream"
	Downstream = "downstream"
	Both       = "both"
)

type Match struct {
	Unit     core.CodeUnit
	Distance int
	Relation string
	Path     string
}

type Graph struct {
	unitsByID map[string]core.CodeUnit
	idsByPath map[string][]string
	idsByName map[string][]string
	out       map[string][]core.CodeEdge
	in        map[string][]core.CodeEdge
}

func New(units []core.CodeUnit, edges []core.CodeEdge) *Graph {
	g := &Graph{
		unitsByID: make(map[string]core.CodeUnit, len(units)),
		idsByPath: make(map[string][]string),
		idsByName: make(map[string][]string),
		out:       make(map[string][]core.CodeEdge),
		in:        make(map[string][]core.CodeEdge),
	}
	for _, unit := range units {
		if unit.ID == "" {
			continue
		}
		if current, exists := g.unitsByID[unit.ID]; !exists || preferredUnit(unit, current) {
			g.unitsByID[unit.ID] = unit
		}
	}
	for _, id := range sortedMapKeys(g.unitsByID) {
		unit := g.unitsByID[id]
		if unit.Path != "" {
			g.idsByPath[unit.Path] = append(g.idsByPath[unit.Path], unit.ID)
		}
		if unit.Name != "" {
			g.idsByName[unit.Name] = append(g.idsByName[unit.Name], unit.ID)
		}
	}
	for key := range g.idsByPath {
		g.idsByPath[key] = uniqueSorted(g.idsByPath[key])
	}
	for key := range g.idsByName {
		g.idsByName[key] = uniqueSorted(g.idsByName[key])
	}
	seen := map[string]bool{}
	for _, edge := range sortedEdges(edges) {
		if edge.SourceID == "" || edge.TargetID == "" {
			continue
		}
		key := edgeKey(edge)
		if seen[key] {
			continue
		}
		seen[key] = true
		g.out[edge.SourceID] = append(g.out[edge.SourceID], edge)
		g.in[edge.TargetID] = append(g.in[edge.TargetID], edge)
	}
	for id := range g.out {
		sort.Slice(g.out[id], func(i, j int) bool { return fullEdgeKey(g.out[id][i]) < fullEdgeKey(g.out[id][j]) })
	}
	for id := range g.in {
		sort.Slice(g.in[id], func(i, j int) bool { return fullEdgeKey(g.in[id][i]) < fullEdgeKey(g.in[id][j]) })
	}
	return g
}

func (g *Graph) Neighbors(ids []string, direction string, depth int) []Match {
	if g == nil || (direction != Upstream && direction != Downstream && direction != Both) || depth < 1 || depth > 2 {
		return nil
	}
	seen := map[string]int{}
	queue := make([]walk, 0, len(ids))
	for _, id := range uniqueSorted(ids) {
		seen[id] = 0
		queue = append(queue, walk{id: id})
	}
	matches := map[string]Match{}
	for head := 0; head < len(queue); head++ {
		current := queue[head]
		if current.distance == depth {
			continue
		}
		for _, step := range g.steps(current.id, direction) {
			nextDistance := current.distance + 1
			if prior, exists := seen[step.id]; exists && prior <= nextDistance {
				continue
			}
			seen[step.id] = nextDistance
			queue = append(queue, walk{id: step.id, distance: nextDistance})
			if unit, exists := g.unitsByID[step.id]; exists {
				matches[step.id] = Match{Unit: unit, Distance: nextDistance, Relation: step.edge.Relation, Path: step.edge.Path}
			}
		}
	}
	result := make([]Match, 0, len(matches))
	for _, match := range matches {
		result = append(result, match)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Unit.ID != result[j].Unit.ID {
			return result[i].Unit.ID < result[j].Unit.ID
		}
		return result[i].Distance < result[j].Distance
	})
	return result
}

func (g *Graph) Impact(symbolOrPath, direction string, depth int) core.ImpactResult {
	result := core.ImpactResult{}
	if depth < 1 || depth > 2 {
		result.Warnings = []string{"graph depth must be 1 or 2"}
		return result
	}
	if direction != Upstream && direction != Downstream && direction != Both {
		result.Warnings = []string{fmt.Sprintf("unknown graph direction %q", direction)}
		return result
	}
	id, warning := g.resolve(symbolOrPath)
	if warning != "" {
		result.Warnings = []string{warning}
		return result
	}
	origin := g.unitsByID[id]
	result.Origin = candidate(origin, "")
	for _, match := range g.Neighbors([]string{id}, direction, depth) {
		item := candidate(match.Unit, match.Relation)
		item.Match = fmt.Sprintf("graph_distance_%d", match.Distance)
		result.Matches = append(result.Matches, item)
	}
	return result
}

type walk struct {
	id       string
	distance int
}

type step struct {
	id   string
	edge core.CodeEdge
}

func (g *Graph) steps(id, direction string) []step {
	result := []step{}
	if direction == Downstream || direction == Both {
		for _, edge := range g.out[id] {
			result = append(result, step{id: edge.TargetID, edge: edge})
		}
	}
	if direction == Upstream || direction == Both {
		for _, edge := range g.in[id] {
			result = append(result, step{id: edge.SourceID, edge: edge})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].id != result[j].id {
			return result[i].id < result[j].id
		}
		return fullEdgeKey(result[i].edge) < fullEdgeKey(result[j].edge)
	})
	return result
}

func (g *Graph) resolve(value string) (string, string) {
	if g == nil || value == "" {
		return "", "graph origin is empty or unavailable"
	}
	if _, exists := g.unitsByID[value]; exists {
		return value, ""
	}
	if ids := g.idsByPath[value]; len(ids) == 1 {
		return ids[0], ""
	} else if len(ids) > 1 {
		return "", fmt.Sprintf("ambiguous path %q: %s", value, strings.Join(ids, ", "))
	}
	if ids := g.idsByName[value]; len(ids) == 1 {
		return ids[0], ""
	} else if len(ids) > 1 {
		return "", fmt.Sprintf("ambiguous symbol %q: %s", value, strings.Join(ids, ", "))
	}
	return "", fmt.Sprintf("graph origin %q not found", value)
}

func candidate(unit core.CodeUnit, relation string) core.Candidate {
	symbol := unit.QualifiedName
	if symbol == "" {
		symbol = unit.Name
	}
	return core.Candidate{
		ID: unit.ID, Type: unit.Kind, Language: unit.Language, Extension: unit.Extension, Path: unit.Path,
		Symbol: symbol, Relation: relation, FileHash: unit.FileHash, StartLine: unit.StartLine,
		EndLine: unit.EndLine, Weight: unit.Weight, Content: unit.Source,
	}
}

func sortedEdges(edges []core.CodeEdge) []core.CodeEdge {
	result := append([]core.CodeEdge(nil), edges...)
	sort.Slice(result, func(i, j int) bool {
		return fullEdgeKey(result[i]) < fullEdgeKey(result[j])
	})
	return result
}

func preferredUnit(candidate, current core.CodeUnit) bool {
	if candidate.Generation != current.Generation {
		return candidate.Generation > current.Generation
	}
	return completeUnitKey(candidate) < completeUnitKey(current)
}

func completeUnitKey(unit core.CodeUnit) string {
	return strings.Join([]string{
		unit.ID, unit.Name, unit.QualifiedName, unit.Kind, unit.Language, unit.Extension, unit.Path, unit.Source, unit.FileHash,
		strconv.FormatUint(uint64(unit.StartLine), 10), strconv.FormatUint(uint64(unit.EndLine), 10), strconv.FormatUint(unit.Generation, 10),
		strconv.FormatFloat(unit.Weight, 'g', -1, 64),
	}, "\x00")
}

func edgeKey(edge core.CodeEdge) string {
	return strings.Join([]string{edge.SourceID, edge.Relation, edge.TargetID, edge.Path, fmt.Sprint(edge.StartLine)}, "\x00")
}

func fullEdgeKey(edge core.CodeEdge) string {
	return strings.Join([]string{edgeKey(edge), edge.FileHash, edge.Resolution, fmt.Sprint(edge.EndLine), fmt.Sprint(edge.Generation)}, "\x00")
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func sortedMapKeys(values map[string]core.CodeUnit) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
