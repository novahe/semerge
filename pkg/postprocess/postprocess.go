package postprocess

import (
	"fmt"
	"go/format"
	"strings"

	"github.com/semerge/semerge/pkg/core"
)

func BuildResolvedBlocks(conflicts []core.Hunk, resolvedMap map[int]string) ([]string, error) {
	if len(conflicts) == 0 {
		return nil, fmt.Errorf("no conflicts provided")
	}
	if len(resolvedMap) == 0 {
		return nil, fmt.Errorf("no resolved blocks returned")
	}

	resolvedBlocks := make([]string, 0, len(conflicts))
	for idx, conflict := range conflicts {
		resolved, ok := resolvedMap[idx]
		if !ok {
			return nil, fmt.Errorf("missing resolution for conflict id %d", idx)
		}

		sanitized := sanitizeResolutionWithConflict(conflict, resolved)
		if strings.TrimSpace(sanitized) == "" {
			return nil, fmt.Errorf("empty sanitized resolution for conflict id %d", idx)
		}

		resolvedBlocks = append(resolvedBlocks, sanitized)
	}

	for id := range resolvedMap {
		if id < 0 || id >= len(conflicts) {
			return nil, fmt.Errorf("resolution id %d out of range", id)
		}
	}

	return resolvedBlocks, nil
}

func ApplyResolutionsToConflictOutput(conflictOutput string, resolutions []string) (string, error) {
	lines := strings.Split(conflictOutput, "\n")
	outLines := make([]string, 0, len(lines))

	inConflict := false
	resolutionIdx := 0

	for _, line := range lines {
		if strings.HasPrefix(line, "<<<<<<<") {
			if inConflict {
				return "", fmt.Errorf("nested conflict markers detected")
			}
			inConflict = true
			continue
		}

		if inConflict {
			if strings.HasPrefix(line, "|||||||") {
				continue
			}
			if strings.HasPrefix(line, "=======") {
				continue
			}
			if strings.HasPrefix(line, ">>>>>>>") {
				if resolutionIdx >= len(resolutions) {
					return "", fmt.Errorf("insufficient resolutions: need more than %d", resolutionIdx)
				}

				resolved := strings.TrimRight(resolutions[resolutionIdx], "\n")
				if resolved != "" {
					outLines = append(outLines, strings.Split(resolved, "\n")...)
				}

				resolutionIdx++
				inConflict = false
				continue
			}

			continue
		}

		outLines = append(outLines, line)
	}

	if inConflict {
		return "", fmt.Errorf("unterminated conflict markers in merged output")
	}

	if resolutionIdx != len(resolutions) {
		return "", fmt.Errorf("unused resolutions: applied %d of %d", resolutionIdx, len(resolutions))
	}

	result := strings.Join(outLines, "\n")
	if strings.HasSuffix(conflictOutput, "\n") && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}

	return result, nil
}

func PostProcessMergedContent(language, content string) string {
	if strings.ToLower(language) != "go" {
		return content
	}

	if formatted, err := format.Source([]byte(content)); err == nil {
		return string(formatted)
	}

	return content
}

func sanitizeResolutionWithConflict(conflict core.Hunk, resolved string) string {
	_ = conflict
	trimmedResolved := strings.TrimRight(stripFencedCodeBlock(resolved), "\n")
	if trimmedResolved == "" {
		return ""
	}

	lines := strings.Split(trimmedResolved, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "<<<<<<<") || strings.HasPrefix(t, "=======") || strings.HasPrefix(t, ">>>>>>>") || strings.HasPrefix(t, "|||||||") {
			continue
		}
		filtered = append(filtered, line)
	}

	for len(filtered) > 0 && strings.TrimSpace(filtered[0]) == "" {
		filtered = filtered[1:]
	}
	for len(filtered) > 0 && strings.TrimSpace(filtered[len(filtered)-1]) == "" {
		filtered = filtered[:len(filtered)-1]
	}

	if len(filtered) == 0 {
		return trimmedResolved
	}

	return strings.Join(filtered, "\n")
}

func stripFencedCodeBlock(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) >= 2 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		end := -1
		for i := len(lines) - 1; i > 0; i-- {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				end = i
				break
			}
		}

		if end > 0 {
			return strings.Join(lines[1:end], "\n")
		}
	}

	return text
}
