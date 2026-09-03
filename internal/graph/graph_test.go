package graph

import (
	"reflect"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func TestNeighborsUsesSortedMinimumDistanceAcrossCycles(t *testing.T) {
	g := New(
		[]core.CodeUnit{{ID: "A"}, {ID: "B"}, {ID: "C"}, {ID: "D"}, {ID: "E"}},
		[]core.CodeEdge{
			{SourceID: "A", TargetID: "C", Relation: "calls", Path: "a.go", StartLine: 3},
			{SourceID: "A", TargetID: "B", Relation: "calls", Path: "a.go", StartLine: 2},
			{SourceID: "A", TargetID: "B", Relation: "calls", Path: "a.go", StartLine: 2},
			{SourceID: "B", TargetID: "A", Relation: "calls", Path: "b.go", StartLine: 1},
			{SourceID: "B", TargetID: "D", Relation: "calls", Path: "b.go", StartLine: 2},
			{SourceID: "C", TargetID: "D", Relation: "calls", Path: "c.go", StartLine: 2},
			{SourceID: "D", TargetID: "E", Relation: "calls", Path: "d.go", StartLine: 2},
		},
	)

	got := g.Neighbors([]string{"A"}, Downstream, 2)
	if ids := matchIDs(got); !reflect.DeepEqual(ids, []string{"B", "C", "D"}) {
		t.Fatalf("match IDs = %v", ids)
	}
	if got[2].Distance != 2 || got[2].Relation != "calls" || got[2].Path != "b.go" {
		t.Fatalf("D evidence = %+v", got[2])
	}
}

func TestNeighborsSkipsMissingTargetsAndRejectsInvalidBounds(t *testing.T) {
	g := New(
		[]core.CodeUnit{{ID: "A"}, {ID: "B"}},
		[]core.CodeEdge{
			{SourceID: "A", TargetID: "missing", Relation: "calls", Path: "a.go", StartLine: 1},
			{SourceID: "missing", TargetID: "B", Relation: "calls", Path: "missing.go", StartLine: 1},
		},
	)
	if ids := matchIDs(g.Neighbors([]string{"A"}, Downstream, 2)); !reflect.DeepEqual(ids, []string{"B"}) {
		t.Fatalf("missing target traversal = %v", ids)
	}
	for _, depth := range []int{0, 3} {
		if got := g.Neighbors([]string{"A"}, Downstream, depth); len(got) != 0 {
			t.Fatalf("depth %d = %+v", depth, got)
		}
	}
}

func TestNeighborsNeverConnectsThroughEmptyEndpointIDs(t *testing.T) {
	g := New(
		[]core.CodeUnit{{ID: "A"}, {ID: "B"}, {ID: "C"}, {ID: "D"}, {ID: "Z"}},
		[]core.CodeEdge{
			{SourceID: "A", TargetID: "", Relation: "unresolved", Path: "a.go", StartLine: 1},
			{SourceID: "B", TargetID: "", Relation: "unresolved", Path: "b.go", StartLine: 1},
			{SourceID: "", TargetID: "C", Relation: "unresolved", Path: "c.go", StartLine: 1},
			{SourceID: "", TargetID: "D", Relation: "unresolved", Path: "d.go", StartLine: 1},
			{SourceID: "A", TargetID: "Z", Relation: "calls", Path: "a.go", StartLine: 2},
		},
	)
	for _, test := range []struct {
		direction string
		want      []string
	}{
		{Upstream, []string{}},
		{Downstream, []string{"Z"}},
		{Both, []string{"Z"}},
	} {
		if ids := matchIDs(g.Neighbors([]string{"A"}, test.direction, 2)); !reflect.DeepEqual(ids, test.want) {
			t.Fatalf("%s traversal = %v", test.direction, ids)
		}
	}
}

func TestNeighborsSupportsUpstreamDownstreamAndBoth(t *testing.T) {
	g := New(
		[]core.CodeUnit{{ID: "A"}, {ID: "B"}, {ID: "C"}},
		[]core.CodeEdge{
			{SourceID: "A", TargetID: "B", Relation: "calls", Path: "a.go", StartLine: 1},
			{SourceID: "B", TargetID: "C", Relation: "calls", Path: "b.go", StartLine: 1},
		},
	)
	if ids := matchIDs(g.Neighbors([]string{"B"}, Upstream, 1)); !reflect.DeepEqual(ids, []string{"A"}) {
		t.Fatalf("upstream = %v", ids)
	}
	if ids := matchIDs(g.Neighbors([]string{"B"}, Downstream, 1)); !reflect.DeepEqual(ids, []string{"C"}) {
		t.Fatalf("downstream = %v", ids)
	}
	if ids := matchIDs(g.Neighbors([]string{"B"}, Both, 1)); !reflect.DeepEqual(ids, []string{"A", "C"}) {
		t.Fatalf("both = %v", ids)
	}
}

func TestImpactResolvesExactIDPathAndNameAndReportsAmbiguity(t *testing.T) {
	g := New(
		[]core.CodeUnit{
			{ID: "id", Name: "same", Path: "a.go", Kind: "function", Language: "go", StartLine: 2, EndLine: 4},
			{ID: "other", Name: "same", Path: "b.go", Kind: "function", Language: "go", StartLine: 1, EndLine: 2},
			{ID: "target", Name: "target", Path: "target.go", Kind: "function", Language: "go"},
		},
		[]core.CodeEdge{{SourceID: "id", TargetID: "target", Relation: "calls", Path: "a.go", StartLine: 3}},
	)
	if got := g.Impact("id", Downstream, 1); got.Origin.ID != "id" || len(got.Matches) != 1 || got.Matches[0].Relation != "calls" {
		t.Fatalf("exact ID impact = %+v", got)
	}
	if got := g.Impact("a.go", Downstream, 1); got.Origin.ID != "id" {
		t.Fatalf("path impact = %+v", got)
	}
	if got := g.Impact("same", Downstream, 1); len(got.Warnings) != 1 || len(got.Matches) != 0 {
		t.Fatalf("ambiguous name impact = %+v", got)
	}
	if got := g.Impact("id", Downstream, 3); len(got.Warnings) != 1 || len(got.Matches) != 0 {
		t.Fatalf("invalid depth impact = %+v", got)
	}
	if got := g.Impact("id", "sideways", 1); len(got.Warnings) != 1 || len(got.Matches) != 0 {
		t.Fatalf("invalid direction impact = %+v", got)
	}
	ambiguousPath := New([]core.CodeUnit{{ID: "left", Path: "same.go"}, {ID: "right", Path: "same.go"}}, nil)
	if got := ambiguousPath.Impact("same.go", Downstream, 1); len(got.Warnings) != 1 || got.Origin.ID != "" {
		t.Fatalf("ambiguous path impact = %+v", got)
	}
}

func TestNewChoosesDuplicateUnitWinnerIndependentOfInputOrder(t *testing.T) {
	older := core.CodeUnit{ID: "same", Name: "a", QualifiedName: "a", Kind: "function", Language: "go", Extension: ".go", Path: "a.go", Source: "a", FileHash: "a", StartLine: 1, EndLine: 2, Generation: 1, Weight: 0.1}
	newer := core.CodeUnit{ID: "same", Name: "z", QualifiedName: "z", Kind: "function", Language: "go", Extension: ".go", Path: "z.go", Source: "z", FileHash: "z", StartLine: 9, EndLine: 10, Generation: 2, Weight: 0.1}
	for _, order := range [][]core.CodeUnit{{older, newer}, {newer, older}} {
		if got := New(order, nil).Impact("same", Downstream, 1).Origin; got.Path != "z.go" || got.Symbol != "z" || got.Weight != 0.1 {
			t.Fatalf("winner = %+v", got)
		}
	}
	lowWeight := newer
	lowWeight.Weight = 0.2
	highWeight := newer
	highWeight.Weight = 0.9
	for _, order := range [][]core.CodeUnit{{highWeight, lowWeight}, {lowWeight, highWeight}} {
		if got := New(order, nil).Impact("same", Downstream, 1).Origin; got.Weight != 0.2 {
			t.Fatalf("weight tie-break winner = %+v", got)
		}
	}
}

func matchIDs(matches []Match) []string {
	ids := make([]string, len(matches))
	for i, match := range matches {
		ids[i] = match.Unit.ID
	}
	return ids
}
