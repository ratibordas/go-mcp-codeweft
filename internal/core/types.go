package core

import (
	"encoding/json"
	"fmt"
	"time"
)

type FileState struct {
	ProjectID     string
	Path          string
	Kind          string
	Language      string
	Extension     string
	Size          int64
	MTimeNS       int64
	Hash          string
	ParserVersion string
	Generation    uint64
	Deleted       bool
}

type CodeUnit struct {
	ID, Name, QualifiedName, Kind, Language, Extension, Path, Source, FileHash string
	StartLine, EndLine                                                         uint32
	Generation                                                                 uint64
	Weight                                                                     float64
}

type CodeEdge struct {
	SourceID, TargetID, Relation, Path, FileHash, Resolution string
	StartLine, EndLine                                       uint32
	Generation                                               uint64
}

type DocChunk struct {
	ID, Path, Extension, Heading, Content, SearchText, ChunkHash, FileHash string
	StartLine, EndLine                                                     uint32
	Tags, Links                                                            []string
	Embedding                                                              []float32
	Generation                                                             uint64
}

type IndexedFile struct {
	File     FileState
	Units    []CodeUnit
	Edges    []CodeEdge
	Chunks   []DocChunk
	Warnings []string
}

type Candidate struct {
	ID, Type, Match, Language, Extension, Path, Symbol, Relation, Heading, FileHash string
	StartLine, EndLine                                                              uint32
	Score, Weight                                                                   float64
	Content                                                                         string
}

type SearchRequest struct {
	Question  string
	Paths     []string
	MaxTokens int
}

type ImpactRequest struct {
	Symbol, Path, Direction string
	Depth                   int
}

type GenerateRequest struct {
	Prompt string
	Schema json.RawMessage
}

type Progress struct {
	Phase                             string
	Completed, Total                  uint64
	Changed, Deleted, Skipped, Failed uint64
	Elapsed, ETA                      time.Duration
	FilesPerSecond, ChunksPerSecond   float64
}

func (p Progress) Message() string {
	return fmt.Sprintf("%s: elapsed %s, files/s %.2f, chunks/s %.2f, eta %s", p.Phase, p.Elapsed, p.FilesPerSecond, p.ChunksPerSecond, p.ETA)
}

type IndexStatus struct {
	State, Phase, LastError            string
	ActiveGeneration, TargetGeneration uint64
	Progress                           Progress
	LastSuccess                        time.Time
	Pending, Warnings                  []string
	PhaseTimings                       map[string]time.Duration
}

type SyncResult struct {
	Generation                        uint64
	Changed, Deleted, Skipped, Failed int
	Pending, Warnings                 []string
}

type ModelHealth struct {
	GenerationAvailable, EmbeddingAvailable bool
	Warnings                                []string
}

type RetrievalResult struct {
	Candidates []Candidate   `json:"candidates"`
	Warnings   []string      `json:"warnings"`
	Generation uint64        `json:"generation"`
	Indexing   time.Duration `json:"indexing"`
	Retrieval  time.Duration `json:"retrieval"`
}

type Evidence struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Language  string `json:"language,omitempty"`
	Extension string `json:"extension"`
	Path      string `json:"path"`
	Symbol    string `json:"symbol,omitempty"`
	Relation  string `json:"relation,omitempty"`
	Format    string `json:"format,omitempty"`
	Heading   string `json:"heading,omitempty"`
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
	Snippet   string `json:"snippet,omitempty"`
	Quote     string `json:"quote,omitempty"`
}

type Freshness struct {
	Generation uint64   `json:"generation"`
	State      string   `json:"state"`
	Pending    []string `json:"pending,omitempty"`
}

type Timing struct {
	Indexing   time.Duration `json:"indexing"`
	Retrieval  time.Duration `json:"retrieval"`
	Generation time.Duration `json:"generation"`
	Total      time.Duration `json:"total"`
}

type Budget struct {
	Requested int  `json:"requested"`
	Used      int  `json:"used"`
	Truncated bool `json:"truncated"`
}

type ContextResult struct {
	Summary   string     `json:"summary"`
	Evidence  []Evidence `json:"evidence"`
	Warnings  []string   `json:"warnings"`
	Freshness Freshness  `json:"freshness"`
	Timing    Timing     `json:"timing"`
	Budget    Budget     `json:"budget"`
}

type ImpactResult struct {
	Origin     Candidate   `json:"origin"`
	Matches    []Candidate `json:"matches"`
	Warnings   []string    `json:"warnings"`
	Generation uint64      `json:"generation"`
	Timing     Timing      `json:"timing"`
}
