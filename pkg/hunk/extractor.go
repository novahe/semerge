package hunk

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/semerge/semerge/pkg/core"
)

const (
	ConflictMarkerStart      = "<<<<<<<"
	ConflictMarkerSeparator  = "======="
	ConflictMarkerEnd        = ">>>>>>>"
	ConflictMarkerAncestor   = "|||||||"
)

// Extractor handles conflict extraction from merge files
type Extractor struct {
	workDir string
}

// NewExtractor creates a new conflict extractor
func NewExtractor(workDir string) *Extractor {
	return &Extractor{
		workDir: workDir,
	}
}

// ExtractFromMergeFile runs git merge-file and extracts conflicts
func (e *Extractor) ExtractFromMergeFile(ancestor, current, incoming string) (*core.MergeRequest, error) {
	// Run git merge-file to get the merged result with conflict markers
	output, err := e.runGitMergeFile(ancestor, current, incoming)
	if err != nil {
		return nil, fmt.Errorf("git merge-file failed: %w", err)
	}

	// Extract language from file extension
	language := e.detectLanguage(current)

	// Parse conflicts from the output
	conflicts, err := e.ParseConflictMarkers(output, current, language)
	if err != nil {
		return nil, fmt.Errorf("failed to parse conflicts: %w", err)
	}

	return &core.MergeRequest{
		FileName:     current,
		Language:     language,
		AncestorPath: ancestor,
		CurrentPath:  current,
		IncomingPath: incoming,
		Conflicts:    conflicts,
	}, nil
}

// runGitMergeFile executes the git merge-file command
func (e *Extractor) runGitMergeFile(ancestor, current, incoming string) (string, error) {
	cmd := exec.Command("git", "merge-file", "-p", current, ancestor, incoming)
	cmd.Dir = e.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// git merge-file returns non-zero exit code on conflicts
		// This is expected behavior, not an error for us
		// Check if we got any output - if we did, it's likely conflicts
		if stdout.Len() == 0 {
			return "", fmt.Errorf("git merge-file error: %s", stderr.String())
		}
	}

	return stdout.String(), nil
}

// ParseConflictMarkers parses conflict markers from the merged output
func (e *Extractor) ParseConflictMarkers(content, filePath, language string) ([]core.Hunk, error) {
	var hunks []core.Hunk
	lines := strings.Split(content, "\n")

	var inConflict bool
	var inIncoming bool // Track if we're in the incoming section
	var currentHunk *core.Hunk
	var currentLines, incomingLines, ancestorLines []string
	var startLine int
	var lineNum int

	for _, line := range lines {
		lineNum++

		if strings.HasPrefix(line, ConflictMarkerStart) {
			// Start of a new conflict
			inConflict = true
			inIncoming = false
			startLine = lineNum - len(currentLines)
			currentHunk = &core.Hunk{
				StartLine: startLine,
				FileName:  filePath,
				Language:  language,
			}
			currentLines = []string{}
			incomingLines = []string{}
			ancestorLines = []string{}
			continue
		}

		if inConflict {
			if strings.HasPrefix(line, ConflictMarkerAncestor) {
				// Switch to ancestor section (if present)
				continue
			}

			if strings.HasPrefix(line, ConflictMarkerSeparator) {
				// Switch from current to incoming
				inIncoming = true
				continue
			}

			if strings.HasPrefix(line, ConflictMarkerEnd) {
				// End of conflict
				currentHunk.EndLine = lineNum
				currentHunk.Current = strings.Join(currentLines, "\n")
				currentHunk.Incoming = strings.Join(incomingLines, "\n")
				currentHunk.Ancestor = strings.Join(ancestorLines, "\n")

				// Extract context lines around the conflict
				contextLines := e.extractContextLines(lines, startLine, lineNum, 3)
				currentHunk.ContextLines = contextLines

				hunks = append(hunks, *currentHunk)
				currentHunk = nil
				inConflict = false
				inIncoming = false
				continue
			}

			// Add line to appropriate section
			if inIncoming {
				incomingLines = append(incomingLines, line)
			} else {
				currentLines = append(currentLines, line)
			}
		}
	}

	return hunks, nil
}

// extractContextLines extracts context lines around a conflict
func (e *Extractor) extractContextLines(lines []string, start, end, contextSize int) []string {
	contextStart := start - contextSize
	if contextStart < 0 {
		contextStart = 0
	}

	contextEnd := end + contextSize
	if contextEnd >= len(lines) {
		contextEnd = len(lines) - 1
	}

	var context []string
	for i := contextStart; i <= contextEnd; i++ {
		if i < start || i > end {
			context = append(context, lines[i])
		}
	}

	return context
}

// detectLanguage detects the programming language from file extension
func (e *Extractor) detectLanguage(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))

	langMap := map[string]string{
		".go":       "go",
		".py":       "python",
		".js":       "javascript",
		".ts":       "typescript",
		".tsx":      "typescript",
		".jsx":      "javascript",
		".java":     "java",
		".c":        "c",
		".cpp":      "cpp",
		".cc":       "cpp",
		".h":        "c",
		".hpp":      "cpp",
		".rs":       "rust",
		".rb":       "ruby",
		".php":      "php",
		".cs":       "csharp",
		".swift":    "swift",
		".kt":       "kotlin",
		".scala":    "scala",
		".sh":       "bash",
		".bash":     "bash",
		".zsh":      "zsh",
		".json":     "json",
		".yaml":     "yaml",
		".yml":      "yaml",
		".toml":     "toml",
		".md":       "markdown",
		".xml":      "xml",
		".html":     "html",
		".css":      "css",
		".scss":     "scss",
		".less":     "less",
	}

	if lang, ok := langMap[ext]; ok {
		return lang
	}

	return "text"
}

// ExtractHunksFromFiles extracts hunks directly from three file versions
func (e *Extractor) ExtractHunksFromFiles(ancestor, current, incoming string) ([]core.Hunk, error) {
	currentContent, err := os.ReadFile(current)
	if err != nil {
		return nil, fmt.Errorf("failed to read current file: %w", err)
	}

	incomingContent, err := os.ReadFile(incoming)
	if err != nil {
		return nil, fmt.Errorf("failed to read incoming file: %w", err)
	}

	ancestorContent, err := os.ReadFile(ancestor)
	if err != nil {
		return nil, fmt.Errorf("failed to read ancestor file: %w", err)
	}

	// Use diff to find conflicting regions
	hunks, err := e.extractConflictingRegions(
		string(ancestorContent),
		string(currentContent),
		string(incomingContent),
		current,
	)

	return hunks, err
}

// extractConflictingRegions finds conflicting regions using diff algorithm
func (e *Extractor) extractConflictingRegions(ancestor, current, incoming, filePath string) ([]core.Hunk, error) {
	var hunks []core.Hunk

	// Split into lines
	ancestorLines := strings.Split(ancestor, "\n")
	currentLines := strings.Split(current, "\n")
	incomingLines := strings.Split(incoming, "\n")

	// Find regions where both current and incoming differ from ancestor
	// This is a simplified implementation
	// A production version would use a proper three-way diff algorithm

	conflictStart := -1
	maxLines := max(len(ancestorLines), len(currentLines), len(incomingLines))

	for i := 0; i < maxLines; i++ {
		ancestorLine := getLine(ancestorLines, i)
		currentLine := getLine(currentLines, i)
		incomingLine := getLine(incomingLines, i)

		// Check if this is a conflict
		currentChanged := currentLine != ancestorLine
		incomingChanged := incomingLine != ancestorLine
		bothChanged := currentChanged && incomingChanged && currentLine != incomingLine

		if bothChanged && conflictStart == -1 {
			// Start of a conflict
			conflictStart = i
		} else if !bothChanged && conflictStart != -1 {
			// End of a conflict
			hunk := core.Hunk{
				StartLine: conflictStart,
				EndLine:   i - 1,
				Current:   strings.Join(currentLines[conflictStart:i], "\n"),
				Incoming:  strings.Join(incomingLines[conflictStart:i], "\n"),
				Ancestor:  strings.Join(ancestorLines[conflictStart:i], "\n"),
				FileName:  filePath,
				Language:  e.detectLanguage(filePath),
			}
			hunks = append(hunks, hunk)
			conflictStart = -1
		}
	}

	// Handle conflict at end of file
	if conflictStart != -1 {
		hunk := core.Hunk{
			StartLine: conflictStart,
			EndLine:   maxLines - 1,
			Current:   strings.Join(currentLines[conflictStart:], "\n"),
			Incoming:  strings.Join(incomingLines[conflictStart:], "\n"),
			Ancestor:  strings.Join(ancestorLines[conflictStart:], "\n"),
			FileName:  filePath,
			Language:  e.detectLanguage(filePath),
		}
		hunks = append(hunks, hunk)
	}

	return hunks, nil
}

// getLine safely gets a line from a slice
func getLine(lines []string, index int) string {
	if index < 0 || index >= len(lines) {
		return ""
	}
	return lines[index]
}

// max returns the maximum of multiple integers
func max(values ...int) int {
	maxVal := values[0]
	for _, v := range values[1:] {
		if v > maxVal {
			maxVal = v
		}
	}
	return maxVal
}

// FindConflictMarkers searches for conflict markers in a file
func (e *Extractor) FindConflictMarkers(filePath string) (bool, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	conflictPattern := regexp.MustCompile(`^<{7}|={7}|>{7}`)

	for scanner.Scan() {
		if conflictPattern.MatchString(scanner.Text()) {
			return true, nil
		}
	}

	return false, scanner.Err()
}

// RemoveConflictMarkers removes conflict markers and resolves to one side
func (e *Extractor) RemoveConflictMarkers(content string, preferIncoming bool) string {
	lines := strings.Split(content, "\n")
	var result []string
	skipIncoming := false

	for _, line := range lines {
		if strings.HasPrefix(line, ConflictMarkerStart) {
			skipIncoming = false
			continue
		}
		if strings.HasPrefix(line, ConflictMarkerSeparator) {
			skipIncoming = true
			continue
		}
		if strings.HasPrefix(line, ConflictMarkerEnd) {
			skipIncoming = false
			continue
		}

		// If we're in the incoming section and prefer current, skip
		// If we're in the current section and prefer incoming, skip
		if preferIncoming && !skipIncoming {
			result = append(result, line)
		} else if !preferIncoming && skipIncoming {
			result = append(result, line)
		} else if !skipIncoming && !strings.HasPrefix(line, ConflictMarkerAncestor) {
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}
