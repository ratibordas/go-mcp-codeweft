package markdown

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestParsePreservesHeadingAndQuoteLines(t *testing.T) {
	data := []byte("---\ntags: [api]\naliases: [Customers API]\n---\n# API\nIntro.\n\n## Create\nUse `POST /customers`.\n")
	chunks, warnings, err := Parse("docs/api.md", data, strings.Repeat("a", 64))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("parse failed: %v %v", err, warnings)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	last := chunks[1]
	if last.Heading != "API > Create" || last.StartLine != 9 || last.EndLine != 9 {
		t.Fatalf("unexpected chunk: %+v", last)
	}
	if last.Content != "Use `POST /customers`.\n" {
		t.Fatalf("content is not the source quote: %q", last.Content)
	}
	if last.Extension != ".md" || !contains(last.Tags, "api") || !strings.Contains(last.SearchText, "Customers API") {
		t.Fatalf("metadata missing: %+v", last)
	}
}

func TestParseRecognizesSetextLinksTagsAndIgnoresFencedSyntax(t *testing.T) {
	data := []byte("---\ntags:\n  - Platform\n---\nOverview\n========\nUse [[architecture/services|services]] and #Roadmap.\n\n```go\n# not a heading\n[[not-a-link]]\n```\n\nDatabase\n--------\nSee [[architecture/database]].\n")
	chunks, warnings, err := Parse("README.md", data, strings.Repeat("b", 64))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("parse failed: %v %v", err, warnings)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	first := chunks[0]
	if first.Heading != "Overview" || first.StartLine != 7 || first.EndLine != 13 {
		t.Fatalf("unexpected first chunk: %+v", first)
	}
	if !contains(first.Tags, "platform") || !contains(first.Tags, "roadmap") {
		t.Fatalf("tags missing: %+v", first.Tags)
	}
	if !contains(first.Links, "architecture/services") || contains(first.Links, "not-a-link") {
		t.Fatalf("links were not parsed safely: %+v", first.Links)
	}
	if chunks[1].Heading != "Overview > Database" || chunks[1].StartLine != 16 || chunks[1].EndLine != 16 || !contains(chunks[1].Links, "architecture/database") {
		t.Fatalf("unexpected second chunk: %+v", chunks[1])
	}
}

func TestParseSplitsOversizedSectionsAtParagraphsWithOverlap(t *testing.T) {
	paragraphs := []string{
		strings.Repeat("a", 1200), strings.Repeat("b", 1200), strings.Repeat("c", 1200),
		strings.Repeat("d", 1200), strings.Repeat("e", 1200),
	}
	data := []byte("# Large\n" + strings.Join(paragraphs, "\n\n") + "\n")
	chunks, _, err := Parse("docs/large.md", data, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4", len(chunks))
	}
	for i, chunk := range chunks {
		if estimatedTokens(chunk.Content) > 900 {
			t.Fatalf("chunk %d exceeds token limit: %d", i, estimatedTokens(chunk.Content))
		}
		if i > 0 && !strings.HasPrefix(chunk.Content, paragraphs[i]) {
			t.Fatalf("chunk %d does not begin with the previous paragraph", i)
		}
	}
}

func TestParseKeepsUnchangedChunkIdentityStable(t *testing.T) {
	before := []byte("# One\nBefore.\n\n# Two\nStable.\n")
	after := []byte("# One\nAfter!.\n\n# Two\nStable.\n")
	first, _, err := Parse("docs/stable.md", before, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Parse("docs/stable.md", after, strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	if first[1].ChunkHash != second[1].ChunkHash || first[1].ID != second[1].ID {
		t.Fatalf("unrelated edit changed stable chunk: before=%+v after=%+v", first[1], second[1])
	}
	if second[1].FileHash != strings.Repeat("e", 64) {
		t.Fatalf("file hash was not retained: %q", second[1].FileHash)
	}
}

func TestParseEmptyDocument(t *testing.T) {
	chunks, warnings, err := Parse("docs/empty.md", nil, strings.Repeat("f", 64))
	if err != nil || len(warnings) != 0 || len(chunks) != 0 {
		t.Fatalf("unexpected empty document result: chunks=%+v warnings=%v err=%v", chunks, warnings, err)
	}
}

func TestChunkID(t *testing.T) {
	content := "Quote.\n"
	wantBytes := sha256.Sum256([]byte("docs/api.md\x00API\x006\x006\x00" + content))
	want := hex.EncodeToString(wantBytes[:])
	if got := chunkID("docs/api.md", "API", 6, 6, content); got != want {
		t.Fatalf("chunkID() = %q, want %q", got, want)
	}
}

func TestParseCapsSingleParagraphWindows(t *testing.T) {
	for _, body := range []string{
		strings.Repeat("x", 7205) + "\n",
		strings.Repeat("界", 7205) + "\n",
	} {
		chunks, _, err := Parse("docs/huge.md", []byte("# Huge\n"+body), strings.Repeat("a", 64))
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) < 2 {
			t.Fatalf("got %d chunks, want rune windows", len(chunks))
		}
		var got string
		for _, chunk := range chunks {
			if estimatedTokens(chunk.Content) > maxChunkTokens {
				t.Fatalf("piece exceeds cap: %d", estimatedTokens(chunk.Content))
			}
			if chunk.StartLine != 2 || chunk.EndLine != 2 {
				t.Fatalf("partial-line range = %d-%d, want 2-2", chunk.StartLine, chunk.EndLine)
			}
			got += chunk.Content
		}
		if got != body {
			t.Fatalf("windows do not cover source exactly")
		}
	}
}

func TestParseRecognizesOnlyMarkdownValidFenceAndSetextIndentation(t *testing.T) {
	data := []byte("# Root\n    ```go\n[[four-space-link]]\n    ```\n\t~~~ts\n[[tab-link]]\n\t~~~\n## Visible\n   ```go\n[[hidden-backtick]]\n````\n   ~~~\n[[hidden-tilde]]\n~~\n[[still-hidden]]\n~~~~\n## After\n[[after]]\n\n    Four\n    ----\n\tTab\n\t====\nZero\n----\nText\n")
	chunks, _, err := Parse("docs/fences.md", data, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4", len(chunks))
	}
	if !contains(chunks[0].Links, "four-space-link") || !contains(chunks[0].Links, "tab-link") {
		t.Fatalf("invalid indented fences hid visible links: %+v", chunks[0].Links)
	}
	if chunks[1].Heading != "Root > Visible" || contains(chunks[1].Links, "hidden-backtick") || contains(chunks[1].Links, "hidden-tilde") || contains(chunks[1].Links, "still-hidden") {
		t.Fatalf("fence handling failed: %+v", chunks[1])
	}
	if chunks[2].Heading != "Root > After" || !contains(chunks[2].Links, "after") {
		t.Fatalf("closing fence or later heading failed: %+v", chunks[2])
	}
	if chunks[3].Heading != "Root > Zero" || strings.Contains(chunks[3].Heading, "Four") || strings.Contains(chunks[3].Heading, "Tab") {
		t.Fatalf("indented setext was recognized: %+v", chunks[3])
	}
}

func TestParseATXClosingHashesNeedWhitespace(t *testing.T) {
	data := []byte("# Root\n## H#\nfirst\n## H ##\nsecond\n##\nthird\n")
	chunks, _, err := Parse("docs/atx.md", data, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	if chunks[0].Heading != "Root > H#" || chunks[1].Heading != "Root > H" || chunks[2].Heading != "Root" {
		t.Fatalf("unexpected ATX ancestry: %+v", chunks)
	}
	if chunks[0].ID != chunkID("docs/atx.md", "Root > H#", 3, 3, "first\n") || chunks[1].ID != chunkID("docs/atx.md", "Root > H", 5, 5, "second\n") {
		t.Fatalf("ATX heading changed chunk IDs")
	}
}

func TestParseKeepsWrongFenceCloserOpaqueAtEOF(t *testing.T) {
	data := []byte("# Root\n```go\n[[hidden]]\n~~~\n## Still hidden\n")
	chunks, _, err := Parse("docs/eof.md", data, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Heading != "Root" || contains(chunks[0].Links, "hidden") {
		t.Fatalf("wrong fence type closed an EOF fence: %+v", chunks)
	}
}

func TestParseRejectsBacktickInfoStringsContainingBackticks(t *testing.T) {
	invalid := []byte("# Root\n```go `invalid`\n## Visible\n[[visible]]\n```\n")
	chunks, _, err := Parse("docs/backticks.md", invalid, strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[1].Heading != "Root > Visible" || !contains(chunks[1].Links, "visible") {
		t.Fatalf("invalid backtick opener hid later Markdown: %+v", chunks)
	}

	tilde := []byte("# Root\n~~~go `allowed`\n[[hidden]]\n~~~\n## After\n[[after]]\n")
	chunks, _, err = Parse("docs/tilde.md", tilde, strings.Repeat("f", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || contains(chunks[0].Links, "hidden") || chunks[1].Heading != "Root > After" || !contains(chunks[1].Links, "after") {
		t.Fatalf("tilde fence variants were not handled: %+v", chunks)
	}
}

func TestParseRuneWindowsKeepNarrowEvidenceAndLocalMetadata(t *testing.T) {
	first := strings.Repeat("a", 3500) + " [[first]] #first\n"
	second := strings.Repeat("b", 3500) + " [[second]] #second\n"
	body := first + second
	chunks, _, err := Parse("docs/windows.md", []byte("# Windows\n"+body), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if chunks[0].StartLine != 2 || chunks[0].EndLine != 3 || chunks[1].StartLine != 3 || chunks[1].EndLine != 3 {
		t.Fatalf("window ranges are not narrow: %+v", chunks)
	}
	if !contains(chunks[0].Links, "first") || contains(chunks[0].Links, "second") || !contains(chunks[0].Tags, "first") || contains(chunks[0].Tags, "second") {
		t.Fatalf("first window metadata leaked: %+v", chunks[0])
	}
	if !contains(chunks[1].Links, "second") || contains(chunks[1].Links, "first") || !contains(chunks[1].Tags, "second") || contains(chunks[1].Tags, "first") {
		t.Fatalf("second window metadata leaked: %+v", chunks[1])
	}
	var got string
	for _, chunk := range chunks {
		if estimatedTokens(chunk.Content) > maxChunkTokens {
			t.Fatalf("window exceeds cap: %d", estimatedTokens(chunk.Content))
		}
		quoted := strings.Join(splitLines(body)[chunk.StartLine-2:chunk.EndLine-1], "")
		if !strings.Contains(quoted, chunk.Content) {
			t.Fatalf("source-line quote does not contain window: %+v", chunk)
		}
		got += chunk.Content
	}
	if got != body {
		t.Fatal("windows do not cover the original paragraph in order")
	}
}

func TestParseRuneWindowsPreserveOriginalFenceState(t *testing.T) {
	body := "````go\n" + strings.Repeat("x", 3500) + "\n" + strings.Repeat("y", 3500) + " [[hidden]] #hidden\n```\n````\n[[visible]] #visible\n"
	chunks, _, err := Parse("docs/fenced-windows.md", []byte("# Fenced\n"+body), strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if contains(chunks[0].Links, "hidden") || contains(chunks[0].Tags, "hidden") {
		t.Fatalf("first window exposed fenced metadata: %+v", chunks[0])
	}
	last := chunks[1]
	if contains(last.Links, "hidden") || contains(last.Tags, "hidden") || !contains(last.Links, "visible") || !contains(last.Tags, "visible") {
		t.Fatalf("window did not preserve original fence state: %+v", last)
	}
}

func TestParseSourceScanHandlesFencesSplitAtWindowBoundaries(t *testing.T) {
	hidden := " [[hidden]] #hidden\n"
	filler := strings.Repeat("b", 3596-len(hidden)) + hidden
	body := strings.Repeat("a", 3597) + "\n```\n" + filler + "```\n[[visible]] #visible\n"
	chunks, _, err := Parse("docs/split-fences.md", []byte("# Split\n"+body), strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 || !strings.HasSuffix(chunks[0].Content, "\n``") || !strings.HasSuffix(chunks[1].Content, "\n``") {
		t.Fatalf("fence delimiters did not split at windows: %+v", chunks)
	}
	for i, chunk := range chunks {
		if contains(chunk.Links, "hidden") || contains(chunk.Tags, "hidden") {
			t.Fatalf("chunk %d exposed fenced metadata: %+v", i, chunk)
		}
	}
	if !contains(chunks[2].Links, "visible") || !contains(chunks[2].Tags, "visible") {
		t.Fatalf("metadata after split closer was not visible: %+v", chunks[2])
	}
}

func TestParseSourceScanUsesOffsetsForDuplicateUTF8Metadata(t *testing.T) {
	line := strings.Repeat("界", 2500) + " [[same]] #same\n"
	chunks, _, err := Parse("docs/duplicate.md", []byte("# Duplicate\n"+line+line), strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	for i, chunk := range chunks {
		if !contains(chunk.Links, "same") || !contains(chunk.Tags, "same") || estimatedTokens(chunk.Content) > maxChunkTokens {
			t.Fatalf("duplicate UTF-8 event %d was not local and bounded: %+v", i, chunk)
		}
	}
}

func TestParseNearTwoMiBIsDeterministic(t *testing.T) {
	body := strings.Repeat("x", 2*1024*1024-16) + "\n"
	first, _, err := Parse("docs/large.md", []byte("# Large\n"+body), strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Parse("docs/large.md", []byte("# Large\n"+body), strings.Repeat("e", 64))
	if err != nil || len(first) < 500 || len(first) != len(second) || first[0].ID != second[0].ID || first[len(first)-1].ID != second[len(second)-1].ID {
		t.Fatalf("large parse was not deterministic: first=%d second=%d err=%v", len(first), len(second), err)
	}
}

func TestParseFrontmatterAndEvidenceBoundaries(t *testing.T) {
	tests := []struct {
		name, data               string
		wantErr                  bool
		wantContent, wantHeading string
	}{
		{name: "malformed", data: "---\ntags: [oops\n---\n# Body\ntext\n", wantErr: true},
		{name: "unterminated", data: "---\ntags: [api]\n# Body\ntext\n", wantContent: "---\ntags: [api]\n"},
		{name: "not at beginning", data: "text\n---\ntags: [api]\n---\nvalue\n", wantContent: "value\n", wantHeading: "tags: [api]"},
		{name: "BOM remains source", data: "\ufeff---\ntags: [api]\n---\n# Body\ntext\n", wantContent: "\ufeff---\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chunks, _, err := Parse("docs/boundary.md", []byte(test.data), strings.Repeat("d", 64))
			if test.wantErr {
				if err == nil {
					t.Fatal("expected frontmatter error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(chunks) == 0 || chunks[0].Content != test.wantContent || (test.wantHeading != "" && chunks[0].Heading != test.wantHeading) || contains(chunks[0].Tags, "api") {
				t.Fatalf("frontmatter boundary changed source: %+v", chunks)
			}
		})
	}
	chunks, _, err := Parse("docs/crlf.md", []byte("# Title\r\nExact quote.\r\n"), strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].StartLine != 2 || chunks[0].EndLine != 2 || chunks[0].Content != "Exact quote.\r\n" {
		t.Fatalf("CRLF quote is not exact: %+v", chunks)
	}
	if got, want := contentHash("Exact quote.\n"), "89e84f75e67ae1d7d187c2732f887d4269d3b0577f8b5df31d5692d94860af84"; got != want {
		t.Fatalf("content hash = %q, want %q", got, want)
	}
}

func TestParseObsidianFixtures(t *testing.T) {
	for _, path := range []string{
		"../../testdata/obsidian/README.md",
		"../../testdata/obsidian/architecture/services.md",
		"../../testdata/obsidian/architecture/database.md",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		chunks, _, err := Parse(strings.TrimPrefix(path, "../../testdata/obsidian/"), data, strings.Repeat("f", 64))
		if err != nil || len(chunks) != 1 || chunks[0].Extension != ".md" {
			t.Fatalf("fixture %s: chunks=%+v err=%v", path, chunks, err)
		}
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
