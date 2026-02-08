package workflow

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/semerge/semerge/internal/config"
	contextpkg "github.com/semerge/semerge/pkg/context"
	"github.com/semerge/semerge/pkg/core"
	"github.com/semerge/semerge/pkg/hunk"
	"github.com/semerge/semerge/pkg/llm"
	"github.com/semerge/semerge/pkg/postprocess"
	"github.com/semerge/semerge/pkg/verifier"
)

const maxResolveAttempts = 3

var listUnmergedFiles = defaultListUnmergedFiles

type ResolveResult struct {
	FilePath      string
	ConflictCount int
	Trailers      []string
}

func ResolveAndWrite(ctx context.Context, cfg *config.Config, ancestor, current, incoming, repoDir, version string) (*ResolveResult, error) {
	extractor := hunk.NewExtractor(repoDir)
	conflictOutput, err := extractor.GenerateConflictOutput(ancestor, current, incoming)
	if err != nil {
		return nil, fmt.Errorf("failed to generate conflict output: %w", err)
	}

	mergeRequest, err := extractor.ExtractFromMergeFile(ancestor, current, incoming)
	if err != nil {
		return nil, fmt.Errorf("failed to extract conflicts: %w", err)
	}

	if len(mergeRequest.Conflicts) == 0 {
		return &ResolveResult{FilePath: current, ConflictCount: 0}, nil
	}

	var mergeCtx *core.Context
	if cfg.Context.UseTreeSitter {
		ctxEngine := contextpkg.NewEngine()
		ctxEngine.SetMaxLines(cfg.Context.MaxLines)
		mergeCtx, _ = ctxEngine.ExtractContext(current, mergeRequest.Conflicts)
		if mergeCtx != nil {
			ctxEngine.EnrichContextWithRelatedFiles(mergeCtx, repoDir)
		}
	}

	llmCfg := &llm.LLMConfig{
		Provider: cfg.LLM.Provider,
		APIKey:   cfg.LLM.APIKey,
		Model:    cfg.LLM.Model,
		Timeout:  cfg.LLM.Timeout,
		BaseURL:  cfg.LLM.BaseURL,
	}

	llmClient, err := llm.NewClient(llmCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM client: %w", err)
	}

	var mergedContent string
	var retryFeedback *core.VerifierFeedback
	var finalVerifyResult *core.VerificationResult

	for attempt := 1; attempt <= maxResolveAttempts; attempt++ {
		feedbackText, previousCandidate := formatVerifierFeedbackForPrompt(retryFeedback)
		llmRequest := &llm.MergeRequest{
			FileName:          mergeRequest.FileName,
			Language:          mergeRequest.Language,
			Hunks:             mergeRequest.Conflicts,
			Context:           mergeCtx,
			Attempt:           attempt,
			RetryFeedback:     feedbackText,
			PreviousCandidate: previousCandidate,
		}

		resolveResult, resolveErr := llmClient.Resolve(ctx, llmRequest)
		if resolveErr != nil {
			retryFeedback = &core.VerifierFeedback{
				Attempt:           attempt,
				TopErrors:         []string{fmt.Sprintf("LLM resolve call failed: %v", resolveErr)},
				PreviousCandidate: mergedContent,
			}
			if attempt == maxResolveAttempts {
				feedbackText, _ = formatVerifierFeedbackForPrompt(retryFeedback)
				return nil, fmt.Errorf("resolution failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
			}
			continue
		}

		if !resolveResult.Success {
			retryFeedback = &core.VerifierFeedback{
				Attempt:           attempt,
				TopErrors:         append([]string{"LLM returned unsuccessful resolution"}, resolveResult.Errors...),
				PreviousCandidate: mergedContent,
			}
			if attempt == maxResolveAttempts {
				feedbackText, _ = formatVerifierFeedbackForPrompt(retryFeedback)
				return nil, fmt.Errorf("resolution failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
			}
			continue
		}

		resolvedBlocks, blockErr := postprocess.BuildResolvedBlocks(mergeRequest.Conflicts, resolveResult.ResolvedMap)
		if blockErr != nil {
			retryFeedback = &core.VerifierFeedback{
				Attempt:           attempt,
				TopErrors:         []string{fmt.Sprintf("Invalid resolution map: %v", blockErr)},
				PreviousCandidate: mergedContent,
			}
			if attempt == maxResolveAttempts {
				feedbackText, _ = formatVerifierFeedbackForPrompt(retryFeedback)
				return nil, fmt.Errorf("resolution failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
			}
			continue
		}

		mergedContent, err = postprocess.ApplyResolutionsToConflictOutput(conflictOutput, resolvedBlocks)
		if err != nil {
			retryFeedback = &core.VerifierFeedback{
				Attempt:           attempt,
				TopErrors:         []string{fmt.Sprintf("Failed to apply resolved conflicts: %v", err)},
				PreviousCandidate: mergedContent,
			}
			if attempt == maxResolveAttempts {
				feedbackText, _ = formatVerifierFeedbackForPrompt(retryFeedback)
				return nil, fmt.Errorf("resolution failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
			}
			continue
		}

		mergedContent = postprocess.PostProcessMergedContent(mergeRequest.Language, mergedContent)
		if !cfg.Verifier.Enabled {
			break
		}

		verifyResult, verifyErr := verifyMergedContent(ctx, cfg, current, mergeRequest.Language, mergedContent)
		if verifyErr != nil {
			return nil, fmt.Errorf("verification failed to run (attempt %d): %w", attempt, verifyErr)
		}

		finalVerifyResult = verifyResult
		if verifyResult.Success {
			break
		}

		retryFeedback = buildVerifierFeedback(verifyResult, mergedContent, attempt)
		if attempt == maxResolveAttempts {
			feedbackText, _ = formatVerifierFeedbackForPrompt(retryFeedback)
			return nil, fmt.Errorf("verification failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
		}
	}

	if cfg.Verifier.Enabled && finalVerifyResult != nil && !finalVerifyResult.Success {
		return nil, fmt.Errorf("verification failed after retries")
	}

	if err := os.WriteFile(current, []byte(mergedContent), 0644); err != nil {
		return nil, fmt.Errorf("failed to write resolved file: %w", err)
	}

	trailers := buildConflictTrailers(repoDir, cfg.LLM.Model, current, len(mergeRequest.Conflicts), version)
	return &ResolveResult{FilePath: current, ConflictCount: len(mergeRequest.Conflicts), Trailers: trailers}, nil
}

func verifyMergedContent(ctx context.Context, cfg *config.Config, filePath, language, content string) (*core.VerificationResult, error) {
	verifierCfg := &verifier.VerifierConfig{
		Enabled: cfg.Verifier.Enabled,
		Tools:   cfg.Verifier.Tools,
		Retries: cfg.Verifier.Retries,
	}
	v := verifier.NewVerifier(verifierCfg, ".")
	return v.Verify(ctx, content, filePath, language)
}

func buildVerifierFeedback(result *core.VerificationResult, candidate string, attempt int) *core.VerifierFeedback {
	if result == nil {
		return &core.VerifierFeedback{Attempt: attempt, PreviousCandidate: strings.TrimSpace(candidate)}
	}

	return &core.VerifierFeedback{
		Attempt:           attempt,
		Tool:              result.Tool,
		ExitCode:          result.ExitCode,
		TopErrors:         topNLines(result.Errors, 5),
		TopWarnings:       topNLines(result.Warnings, 3),
		PreviousCandidate: strings.TrimSpace(candidate),
	}
}

func formatVerifierFeedbackForPrompt(feedback *core.VerifierFeedback) (string, string) {
	if feedback == nil {
		return "", ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Attempt %d verification failed.\n", feedback.Attempt))
	if feedback.Tool != "" {
		b.WriteString(fmt.Sprintf("Tool: %s\n", feedback.Tool))
	}
	if feedback.ExitCode != 0 {
		b.WriteString(fmt.Sprintf("ExitCode: %d\n", feedback.ExitCode))
	}
	if len(feedback.TopErrors) > 0 {
		b.WriteString("Top errors:\n")
		for i, e := range feedback.TopErrors {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, e))
		}
	}
	if len(feedback.TopWarnings) > 0 {
		b.WriteString("Top warnings:\n")
		for i, w := range feedback.TopWarnings {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, w))
		}
	}

	return strings.TrimSpace(b.String()), feedback.PreviousCandidate
}

func topNLines(lines []string, n int) []string {
	if n <= 0 || len(lines) == 0 {
		return nil
	}

	unique := make(map[string]struct{}, len(lines))
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if _, ok := unique[trimmed]; ok {
			continue
		}
		unique[trimmed] = struct{}{}
		cleaned = append(cleaned, trimmed)
	}

	sort.Strings(cleaned)
	if len(cleaned) <= n {
		return cleaned
	}
	return cleaned[:n]
}

type conflictFileStat struct {
	path      string
	conflicts int
}

func buildConflictTrailers(repoDir, model, currentFile string, currentConflicts int, ver string) []string {
	operation := detectGitOperation(repoDir)
	if operation == "" {
		return nil
	}

	ranked, truncated := collectTopConflictFiles(repoDir, 8)
	if path, ok := normalizeConflictFilePath(repoDir, currentFile); ok && currentConflicts > 0 {
		ranked, truncated = upsertCurrentConflictFile(ranked, path, repoDir, currentConflicts, 8)
	}
	if len(ranked) == 0 {
		return nil
	}

	return formatConflictTrailers(ver, model, operation, ranked, truncated)
}

func formatConflictTrailers(ver, model, operation string, files []string, truncated bool) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "unknown"
	}

	filesLine := strings.Join(files, " ")
	if filesLine == "" {
		filesLine = "unknown"
	}
	if truncated {
		filesLine += " ..."
	}

	return []string{
		fmt.Sprintf("Conflict-resolved-by: semerge/%s@%s (%s)", ver, model, operation),
		fmt.Sprintf("Conflict-files: %s", filesLine),
	}
}

func collectTopConflictFiles(repoDir string, limit int) ([]string, bool) {
	files, err := listUnmergedFiles(repoDir)
	if err != nil || len(files) == 0 {
		return nil, false
	}

	stats := make([]conflictFileStat, 0, len(files))
	for _, file := range files {
		content, readErr := os.ReadFile(filepath.Join(repoDir, file))
		if readErr != nil {
			continue
		}

		count := strings.Count(string(content), "<<<<<<<")
		if count <= 0 {
			continue
		}

		stats = append(stats, conflictFileStat{path: file, conflicts: count})
	}

	if len(stats) == 0 {
		return nil, false
	}

	sort.Slice(stats, func(i, j int) bool {
		if stats[i].conflicts != stats[j].conflicts {
			return stats[i].conflicts > stats[j].conflicts
		}
		return stats[i].path < stats[j].path
	})

	truncated := false
	if limit > 0 && len(stats) > limit {
		stats = stats[:limit]
		truncated = true
	}

	result := make([]string, 0, len(stats))
	for _, stat := range stats {
		result = append(result, stat.path)
	}

	return result, truncated
}

func upsertCurrentConflictFile(ranked []string, currentPath, repoDir string, currentConflicts, limit int) ([]string, bool) {
	if currentPath == "" {
		return ranked, false
	}

	stats := make([]conflictFileStat, 0, len(ranked)+1)
	seen := make(map[string]bool, len(ranked)+1)

	for _, path := range ranked {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true

		conflicts := 1
		if content, err := os.ReadFile(filepath.Join(repoDir, path)); err == nil {
			if cnt := strings.Count(string(content), "<<<<<<<"); cnt > 0 {
				conflicts = cnt
			}
		}

		stats = append(stats, conflictFileStat{path: path, conflicts: conflicts})
	}

	updated := false
	for idx := range stats {
		if stats[idx].path == currentPath {
			stats[idx].conflicts = currentConflicts
			updated = true
			break
		}
	}
	if !updated {
		stats = append(stats, conflictFileStat{path: currentPath, conflicts: currentConflicts})
	}

	sort.Slice(stats, func(i, j int) bool {
		if stats[i].conflicts != stats[j].conflicts {
			return stats[i].conflicts > stats[j].conflicts
		}
		return stats[i].path < stats[j].path
	})

	truncated := false
	if limit > 0 && len(stats) > limit {
		stats = stats[:limit]
		truncated = true
	}

	out := make([]string, 0, len(stats))
	for _, stat := range stats {
		out = append(out, stat.path)
	}

	return out, truncated
}

func normalizeConflictFilePath(repoDir, filePath string) (string, bool) {
	trimmed := strings.TrimSpace(filePath)
	if trimmed == "" {
		return "", false
	}

	cleaned := filepath.Clean(trimmed)
	if filepath.IsAbs(cleaned) {
		rel, err := filepath.Rel(repoDir, cleaned)
		if err != nil {
			return "", false
		}
		cleaned = rel
	}

	cleaned = filepath.ToSlash(cleaned)
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", false
	}

	return cleaned, true
}

func defaultListUnmergedFiles(repoDir string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--name-only", "--diff-filter=U")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	raw := strings.Split(strings.TrimSpace(string(out)), "\n")
	files := make([]string, 0, len(raw))
	for _, item := range raw {
		path := strings.TrimSpace(item)
		if path == "" {
			continue
		}
		files = append(files, path)
	}

	return files, nil
}

func detectGitOperation(repoDir string) string {
	gitDir, err := resolveGitDir(repoDir)
	if err != nil {
		return ""
	}

	if fileExists(filepath.Join(gitDir, "CHERRY_PICK_HEAD")) {
		return "cherry-pick"
	}
	if fileExists(filepath.Join(gitDir, "REVERT_HEAD")) {
		return "revert"
	}
	if fileExists(filepath.Join(gitDir, "REBASE_HEAD")) || fileExists(filepath.Join(gitDir, "rebase-merge")) || fileExists(filepath.Join(gitDir, "rebase-apply")) {
		return "rebase"
	}
	if fileExists(filepath.Join(gitDir, "MERGE_HEAD")) {
		return "merge"
	}

	return ""
}

func resolveGitDir(repoDir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	gitDir := strings.TrimSpace(string(out))
	if gitDir == "" {
		return "", fmt.Errorf("empty git dir")
	}

	if filepath.IsAbs(gitDir) {
		return gitDir, nil
	}

	return filepath.Join(repoDir, gitDir), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
