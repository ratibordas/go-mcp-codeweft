package markdown

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"gopkg.in/yaml.v3"
)

const (
	normalSectionTokens = 1200
	maxChunkTokens      = 900
	maxChunkRunes       = maxChunkTokens * 4
)

var (
	atxHeading = regexp.MustCompile(`^(#{1,6})(?:[ \t]+(.*))?$`)
	atxClosing = regexp.MustCompile(`^(.*?)[ \t]+#+[ \t]*$`)
	setextLine = regexp.MustCompile(`^[ \t]*(=+|-+)[ \t]*$`)
	wikiLink   = regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)
	inlineTag  = regexp.MustCompile(`(?:^|[^[:alnum:]_/-])#([[:alnum:]_][[:alnum:]_/-]*)`)
)

type heading struct {
	line, after, level int
	text               string
}

type section struct {
	heading      string
	start, end   int
	contentLines []string
}

type paragraph struct{ start, end int }

type piece struct {
	offset  int
	content string
}

type fenceState struct {
	mark   byte
	length int
}

type metadataEvent struct {
	start, end int
	value      string
}

type sourceScan struct {
	lineStarts []int
	tags       []metadataEvent
	links      []metadataEvent
}

// Parse divides one Markdown document into source-quoted, heading-aware chunks.
func Parse(path string, data []byte, fileHash string) ([]core.DocChunk, []string, error) {
	lines := splitLines(string(data))
	firstContent, tags, aliases, err := frontmatter(lines)
	if err != nil {
		return nil, nil, err
	}
	headings := scanHeadings(lines, firstContent)
	sections := makeSections(lines, firstContent, headings)
	chunks := make([]core.DocChunk, 0, len(sections))
	for _, section := range sections {
		source := strings.Join(section.contentLines, "")
		scan := scanSource(source)
		for _, piece := range splitSection(section) {
			if strings.TrimSpace(piece.content) == "" {
				continue
			}
			chunkTags, links := scan.metadata(piece.offset, len(piece.content), tags)
			startLine, endLine := scan.lineRange(piece.offset, len(piece.content))
			start, end := uint32(section.start+startLine+1), uint32(section.start+endLine+1)
			chunks = append(chunks, core.DocChunk{
				ID:         chunkID(path, section.heading, start, end, piece.content),
				Path:       path,
				Extension:  ".md",
				Heading:    section.heading,
				Content:    piece.content,
				SearchText: searchText(section.heading, aliases, chunkTags, piece.content),
				ChunkHash:  contentHash(piece.content),
				FileHash:   fileHash,
				StartLine:  start,
				EndLine:    end,
				Tags:       chunkTags,
				Links:      links,
			})
		}
	}
	return chunks, nil, nil
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}
	return lines
}

func frontmatter(lines []string) (int, []string, []string, error) {
	if len(lines) == 0 || trimLine(lines[0]) != "---" {
		return 0, nil, nil, nil
	}
	for i := 1; i < len(lines); i++ {
		if trimLine(lines[i]) != "---" {
			continue
		}
		var values map[string]any
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:i], "")), &values); err != nil {
			return 0, nil, nil, fmt.Errorf("parse YAML frontmatter: %w", err)
		}
		return i + 1, normalizeTags(valueStrings(values["tags"])), unique(valueStrings(values["aliases"])), nil
	}
	return 0, nil, nil, nil
}

func valueStrings(value any) []string {
	switch value := value.(type) {
	case string:
		return []string{value}
	case []any:
		result := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func scanHeadings(lines []string, start int) []heading {
	headings := []heading{}
	stack := [6]string{}
	inFence := false
	fenceMark, fenceLength := byte(0), 0
	for i := start; i < len(lines); i++ {
		if mark, length, ok := fence(lines[i]); ok {
			if !inFence {
				inFence, fenceMark, fenceLength = true, mark, length
			} else if mark == fenceMark && length >= fenceLength && closingFence(lines[i], mark, length) {
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		line, validIndent := markdownLine(lines[i])
		if validIndent {
			if match := atxHeading.FindStringSubmatch(line); match != nil {
				level := len(match[1])
				stack[level-1] = cleanATXHeading(match[2])
				for j := level; j < len(stack); j++ {
					stack[j] = ""
				}
				headings = append(headings, heading{line: i, after: i + 1, level: level, text: headingPath(stack)})
				continue
			}
		}
		if i+1 < len(lines) && validIndent && strings.TrimSpace(line) != "" {
			next, nextValidIndent := markdownLine(lines[i+1])
			if match := setextLine.FindStringSubmatch(next); nextValidIndent && match != nil {
				level := 2
				if match[1][0] == '=' {
					level = 1
				}
				stack[level-1] = strings.TrimSpace(line)
				for j := level; j < len(stack); j++ {
					stack[j] = ""
				}
				headings = append(headings, heading{line: i, after: i + 2, level: level, text: headingPath(stack)})
				i++
			}
		}
	}
	return headings
}

func fence(line string) (byte, int, bool) {
	var valid bool
	line, valid = markdownLine(line)
	if !valid {
		return 0, 0, false
	}
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0, false
	}
	mark := line[0]
	length := 0
	for length < len(line) && line[length] == mark {
		length++
	}
	if mark == '`' && strings.Contains(line[length:], "`") {
		return 0, 0, false
	}
	return mark, length, length >= 3
}

func closingFence(line string, mark byte, length int) bool {
	var valid bool
	line, valid = markdownLine(line)
	if !valid {
		return false
	}
	if len(line) < length || line[0] != mark {
		return false
	}
	return strings.TrimSpace(line[length:]) == ""
}

func markdownLine(line string) (string, bool) {
	line = trimLine(line)
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	if indent > 3 || (indent < len(line) && line[indent] == '\t') {
		return "", false
	}
	return line[indent:], true
}

func cleanATXHeading(text string) string {
	text = strings.TrimSpace(text)
	if match := atxClosing.FindStringSubmatch(text); match != nil {
		return strings.TrimSpace(match[1])
	}
	return text
}

func makeSections(lines []string, firstContent int, headings []heading) []section {
	sections := []section{}
	start, name := firstContent, ""
	for _, heading := range headings {
		if start < heading.line {
			sections = append(sections, section{heading: name, start: start, end: heading.line - 1, contentLines: lines[start:heading.line]})
		}
		start, name = heading.after, heading.text
	}
	if start < len(lines) {
		sections = append(sections, section{heading: name, start: start, end: len(lines) - 1, contentLines: lines[start:]})
	}
	return sections
}

func splitSection(input section) []piece {
	content := strings.Join(input.contentLines, "")
	if estimatedTokens(content) <= normalSectionTokens {
		return []piece{{content: content}}
	}
	paragraphs := paragraphs(input.contentLines, input.start)
	if len(paragraphs) == 0 {
		return nil
	}
	pieces := []piece{}
	offsets := lineOffsets(input.contentLines)
	for start := 0; start < len(paragraphs); {
		paragraph := linesFor(input, paragraphs[start].start, paragraphs[start].end)
		offset := offsets[paragraphs[start].start-input.start]
		if estimatedTokens(paragraph) > maxChunkTokens {
			pieces = append(pieces, runeWindows(paragraph, offset)...)
			start++
			continue
		}
		end := start
		for end+1 < len(paragraphs) && estimatedTokens(linesFor(input, paragraphs[start].start, paragraphs[end+1].end)) <= maxChunkTokens {
			end++
		}
		pieces = append(pieces, piece{offset: offset, content: linesFor(input, paragraphs[start].start, paragraphs[end].end)})
		if end == len(paragraphs)-1 {
			break
		}
		if end == start {
			start++
		} else {
			start = end
		}
	}
	return pieces
}

func runeWindows(content string, offset int) []piece {
	windows := []piece{}
	for from := 0; from < len(content); {
		to, runes := from, 0
		for to < len(content) && runes < maxChunkRunes {
			_, size := utf8.DecodeRuneInString(content[to:])
			to += size
			runes++
		}
		window := content[from:to]
		windows = append(windows, piece{offset: offset + from, content: window})
		from = to
	}
	return windows
}

func paragraphs(lines []string, offset int) []paragraph {
	result := []paragraph{}
	for i := 0; i < len(lines); {
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		if i == len(lines) {
			break
		}
		start := i
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			i++
		}
		result = append(result, paragraph{start: offset + start, end: offset + i - 1})
	}
	return result
}

func linesFor(section section, start, end int) string {
	return strings.Join(section.contentLines[start-section.start:end-section.start+1], "")
}

func lineOffsets(lines []string) []int {
	offsets := make([]int, len(lines))
	offset := 0
	for i, line := range lines {
		offsets[i] = offset
		offset += len(line)
	}
	return offsets
}

func scanSource(source string) sourceScan {
	scan := sourceScan{lineStarts: []int{0}}
	state := fenceState{}
	for start := 0; start < len(source); {
		next := len(source)
		if newline := strings.IndexByte(source[start:], '\n'); newline >= 0 {
			next = start + newline + 1
		}
		line := source[start:next]
		transition := advanceFence(state, line)
		if transition == state && state.mark == 0 {
			for _, match := range inlineTag.FindAllStringSubmatchIndex(line, -1) {
				scan.tags = append(scan.tags, metadataEvent{start: start + match[2] - 1, end: start + match[3], value: line[match[2]:match[3]]})
			}
			for _, match := range wikiLink.FindAllStringSubmatchIndex(line, -1) {
				scan.links = append(scan.links, metadataEvent{start: start + match[0], end: start + match[1], value: strings.TrimSpace(line[match[2]:match[3]])})
			}
		}
		state = transition
		start = next
		if start < len(source) {
			scan.lineStarts = append(scan.lineStarts, start)
		}
	}
	return scan
}

func advanceFence(state fenceState, line string) fenceState {
	mark, length, ok := fence(line)
	if !ok {
		return state
	}
	if state.mark == 0 {
		return fenceState{mark: mark, length: length}
	}
	if mark == state.mark && length >= state.length && closingFence(line, mark, length) {
		return fenceState{}
	}
	return state
}

func (scan sourceScan) lineRange(offset, length int) (int, int) {
	start := sort.Search(len(scan.lineStarts), func(i int) bool { return scan.lineStarts[i] > offset }) - 1
	endOffset := offset + length - 1
	end := sort.Search(len(scan.lineStarts), func(i int) bool { return scan.lineStarts[i] > endOffset }) - 1
	return start, end
}

func (scan sourceScan) metadata(offset, length int, baseTags []string) ([]string, []string) {
	tags := append([]string(nil), baseTags...)
	for _, event := range scan.tagsIn(offset, offset+length) {
		tags = append(tags, event.value)
	}
	links := []string{}
	for _, event := range scan.linksIn(offset, offset+length) {
		links = append(links, event.value)
	}
	return normalizeTags(tags), unique(links)
}

func (scan sourceScan) tagsIn(start, end int) []metadataEvent {
	return eventsIn(scan.tags, start, end)
}

func (scan sourceScan) linksIn(start, end int) []metadataEvent {
	return eventsIn(scan.links, start, end)
}

func eventsIn(events []metadataEvent, start, end int) []metadataEvent {
	first := sort.Search(len(events), func(i int) bool { return events[i].start >= start })
	result := []metadataEvent{}
	for _, event := range events[first:] {
		if event.start >= end {
			break
		}
		if event.end <= end {
			result = append(result, event)
		}
	}
	return result
}

func searchText(heading string, aliases, tags []string, content string) string {
	parts := []string{heading, strings.Join(aliases, " "), strings.Join(tags, " "), content}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func normalizeTags(tags []string) []string {
	for i := range tags {
		tags[i] = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(tags[i], "#")))
	}
	return unique(tags)
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

func headingPath(stack [6]string) string {
	parts := []string{}
	for _, part := range stack {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " > ")
}

func trimLine(line string) string { return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r") }

func estimatedTokens(text string) int { return (utf8.RuneCountInString(text) + 3) / 4 }

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func chunkID(path, heading string, start, end uint32, content string) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%d\x00%s", path, heading, start, end, content)
	return hex.EncodeToString(hash.Sum(nil))
}
