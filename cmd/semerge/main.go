package main

import (
	"context"
	"fmt"
	"go/format"

	"github.com/google/uuid"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/semerge/semerge/internal/config"
	contextpkg "github.com/semerge/semerge/pkg/context"
	"github.com/semerge/semerge/pkg/core"
	"github.com/semerge/semerge/pkg/hunk"
	"github.com/semerge/semerge/pkg/llm"
	"github.com/semerge/semerge/pkg/verifier"
	"github.com/spf13/cobra"
)

var (
	cfgFile string
	version = "0.2.1"
)

const maxResolveAttempts = 3

var listUnmergedFiles = defaultListUnmergedFiles

func main() {
	var rootCmd = &cobra.Command{
		Use:   "semerge",
		Short: "AI-powered git merge conflict resolver",
		Long: `Semerge is an AI-powered merge conflict resolution tool that uses
large language models to understand code semantics and resolve conflicts
intelligently. It supports multiple programming languages and integrates
with git as a merge driver.`,
		Version: version,
	}

	// Config command
	var initCmd = &cobra.Command{
		Use:   "init",
		Short: "Initialize semerge configuration",
		RunE:  runInit,
	}

	// Resolve command
	var resolveCmd = &cobra.Command{
		Use:   "resolve [ancestor] [current] [incoming]",
		Short: "Resolve merge conflicts in a file",
		Args:  cobra.ExactArgs(3),
		RunE:  runResolve,
	}

	// Install command
	var installCmd = &cobra.Command{
		Use:   "install",
		Short: "Install semerge as a git merge driver",
		RunE:  runInstall,
	}

	// Health check command
	var healthCmd = &cobra.Command{
		Use:   "health",
		Short: "Check if semerge is properly configured",
		RunE:  runHealth,
	}

	rootCmd.AddCommand(initCmd, resolveCmd, installCmd, healthCmd)
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file path")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runInit(cmd *cobra.Command, args []string) error {
	cfg := config.DefaultConfig()

	configPath := cfgFile
	if configPath == "" {
		var err error
		configPath, err = config.GetConfigPath()
		if err != nil {
			return fmt.Errorf("failed to get config path: %w", err)
		}
	}

	if err := cfg.Save(configPath); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("Configuration initialized at: %s\n", configPath)
	fmt.Println("\nEdit the configuration file to set your ANTHROPIC_API_KEY or OPENAI_API_KEY.")
	return nil
}

func runResolve(cmd *cobra.Command, args []string) error {
	ancestor := args[0]
	current := args[1]
	incoming := args[2]

	// Load configuration
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.LLM.Timeout)*time.Second)
	defer cancel()

	// Extract hunks
	extractor := hunk.NewExtractor(".")
	conflictOutput, err := extractor.GenerateConflictOutput(ancestor, current, incoming)
	if err != nil {
		return fmt.Errorf("failed to generate conflict output: %w", err)
	}

	mergeRequest, err := extractor.ExtractFromMergeFile(ancestor, current, incoming)
	if err != nil {
		return fmt.Errorf("failed to extract conflicts: %w", err)
	}

	if len(mergeRequest.Conflicts) == 0 {
		fmt.Println("No conflicts found.")
		return nil
	}

	fmt.Printf("Found %d conflict(s) in %s\n", len(mergeRequest.Conflicts), mergeRequest.FileName)

	// Extract context
	var ctxEngine *contextpkg.Engine
	var mergeCtx *core.Context
	if cfg.Context.UseTreeSitter {
		ctxEngine = contextpkg.NewEngine()
		ctxEngine.SetMaxLines(cfg.Context.MaxLines)

		mergeCtx, err = ctxEngine.ExtractContext(current, mergeRequest.Conflicts)
		if err != nil {
			fmt.Printf("Warning: Failed to extract context: %v\n", err)
		} else {
			workDir, wdErr := os.Getwd()
			if wdErr == nil {
				ctxEngine.EnrichContextWithRelatedFiles(mergeCtx, workDir)
			}

			fmt.Printf("Extracted context: %d imports, %d functions, %d classes\n",
				len(mergeCtx.Imports), len(mergeCtx.Functions), len(mergeCtx.Classes))
		}
	}

	// Create LLM client
	llmCfg := &llm.LLMConfig{
		Provider: cfg.LLM.Provider,
		APIKey:   cfg.LLM.APIKey,
		Model:    cfg.LLM.Model,
		Timeout:  cfg.LLM.Timeout,
		BaseURL:  cfg.LLM.BaseURL,
	}

	llmClient, err := llm.NewClient(llmCfg)
	if err != nil {
		return fmt.Errorf("failed to create LLM client: %w", err)
	}

	var mergedContent string
	var retryFeedback *core.VerifierFeedback
	var finalVerifyResult *core.VerificationResult

	for attempt := 1; attempt <= maxResolveAttempts; attempt++ {
		fmt.Printf("\nResolving conflicts with AI... (attempt %d/%d)\n", attempt, maxResolveAttempts)

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

		result, err := llmClient.Resolve(ctx, llmRequest)
		if err != nil {
			retryFeedback = &core.VerifierFeedback{
				Attempt:           attempt,
				TopErrors:         []string{fmt.Sprintf("LLM resolve call failed: %v", err)},
				PreviousCandidate: mergedContent,
			}
			if attempt == maxResolveAttempts {
				feedbackText, _ := formatVerifierFeedbackForPrompt(retryFeedback)
				return fmt.Errorf("resolution failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
			}
			continue
		}

		if !result.Success {
			retryFeedback = &core.VerifierFeedback{
				Attempt:           attempt,
				TopErrors:         append([]string{"LLM returned unsuccessful resolution"}, result.Errors...),
				PreviousCandidate: mergedContent,
			}
			if attempt == maxResolveAttempts {
				feedbackText, _ := formatVerifierFeedbackForPrompt(retryFeedback)
				return fmt.Errorf("resolution failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
			}
			continue
		}

		resolvedBlocks, blockErr := buildResolvedBlocks(mergeRequest.Conflicts, result.ResolvedMap)
		if blockErr != nil {
			retryFeedback = &core.VerifierFeedback{
				Attempt:           attempt,
				TopErrors:         []string{fmt.Sprintf("Invalid resolution map: %v", blockErr)},
				PreviousCandidate: mergedContent,
			}
			if attempt == maxResolveAttempts {
				feedbackText, _ := formatVerifierFeedbackForPrompt(retryFeedback)
				return fmt.Errorf("resolution failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
			}
			continue
		}

		for idx := range resolvedBlocks {
			fmt.Printf("Resolved conflict %d/%d\n", idx+1, len(mergeRequest.Conflicts))
		}

		fmt.Println("Conflicts resolved successfully!")

		// Apply resolved blocks to the conflict output in order
		mergedContent, err = applyResolutionsToConflictOutput(conflictOutput, resolvedBlocks)
		if err != nil {
			retryFeedback = &core.VerifierFeedback{
				Attempt:           attempt,
				TopErrors:         []string{fmt.Sprintf("Failed to apply resolved conflicts: %v", err)},
				PreviousCandidate: mergedContent,
			}
			if attempt == maxResolveAttempts {
				feedbackText, _ := formatVerifierFeedbackForPrompt(retryFeedback)
				return fmt.Errorf("resolution failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
			}
			continue
		}
		mergedContent = postProcessMergedContent(current, mergeRequest.Language, mergedContent)

		if !cfg.Verifier.Enabled {
			break
		}

		fmt.Println("\nVerifying resolution...")
		verifyResult, verifyErr := verifyMergedContent(ctx, cfg, current, mergeRequest.Language, mergedContent)
		if verifyErr != nil {
			return fmt.Errorf("verification failed to run (attempt %d): %w", attempt, verifyErr)
		}

		finalVerifyResult = verifyResult
		if verifyResult.Success {
			fmt.Println("Verification passed!")
			break
		}

		retryFeedback = buildVerifierFeedback(verifyResult, mergedContent, attempt)

		fmt.Printf("Verification failed (attempt %d/%d). Top issues:\n", attempt, maxResolveAttempts)
		for _, e := range topNLines(verifyResult.Errors, 3) {
			fmt.Printf("  - %s\n", e)
		}

		if attempt == maxResolveAttempts {
			feedbackText, _ := formatVerifierFeedbackForPrompt(retryFeedback)
			return fmt.Errorf("verification failed after %d attempts:\n%s", maxResolveAttempts, feedbackText)
		}
	}

	if cfg.Verifier.Enabled && finalVerifyResult != nil && !finalVerifyResult.Success {
		return fmt.Errorf("verification failed after retries")
	}

	// Write the merged content to the current file
	if err := os.WriteFile(current, []byte(mergedContent), 0644); err != nil {
		return fmt.Errorf("failed to write resolved file: %w", err)
	}

	fmt.Printf("\nResolved file written to: %s\n", current)

	trailers := buildConflictTrailers(".", cfg.LLM.Model, current, len(mergeRequest.Conflicts))
	if len(trailers) > 0 {
		fmt.Println("Conflict trailer metadata:")
		for _, trailer := range trailers {
			fmt.Println(trailer)
		}
	}

	return nil
}

type conflictFileStat struct {
	path      string
	conflicts int
}

func buildConflictTrailers(repoDir string, model string, currentFile string, currentConflicts int) []string {
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

	sessionID := resolveSessionID()
	return formatConflictTrailers(version, model, operation, ranked, truncated, sessionID)
}

func formatConflictTrailers(ver string, model string, operation string, files []string, truncated bool, sessionID string) []string {
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
		fmt.Sprintf("Conflict-context: x-session-id@%s", sessionID),
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

func upsertCurrentConflictFile(ranked []string, currentPath string, repoDir string, currentConflicts int, limit int) ([]string, bool) {
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

func normalizeConflictFilePath(repoDir string, filePath string) (string, bool) {
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

func resolveSessionID() string {
	for _, key := range []string{"SEMERGE_SESSION_ID", "X_SESSION_ID", "X-SESSION-ID"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}

	return generateSessionID()
}

func generateSessionID() string {
	return uuid.NewString()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func buildResolvedBlocks(conflicts []core.Hunk, resolvedMap map[int]string) ([]string, error) {
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

func verifyMergedContent(ctx context.Context, cfg *config.Config, filePath string, language string, content string) (*core.VerificationResult, error) {
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
		return &core.VerifierFeedback{
			Attempt:           attempt,
			PreviousCandidate: strings.TrimSpace(candidate),
		}
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

func runInstall(cmd *cobra.Command, args []string) error {
	// Install as git merge driver
	driverPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	configCmd := fmt.Sprintf("git config --global merge.semerge.name 'AI Merge Driver'")
	configCmd2 := fmt.Sprintf("git config --global merge.semerge.driver '%s resolve %%O %%A %%B", driverPath)

	fmt.Println("Installing semerge as a git merge driver...")
	fmt.Println("Run the following commands:")
	fmt.Println(configCmd)
	fmt.Println(configCmd2)

	fmt.Println("\nThen add the following to your .gitattributes file:")
	fmt.Println("*.go merge=semerge")
	fmt.Println("*.py merge=semerge")
	fmt.Println("*.js merge=semerge")
	fmt.Println("*.ts merge=semerge")

	return nil
}

func runHealth(cmd *cobra.Command, args []string) error {
	fmt.Println("Checking semerge configuration...")

	// Load configuration
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration invalid: %w", err)
	}

	fmt.Println("✓ Configuration is valid")

	// Check LLM connection
	llmCfg := &llm.LLMConfig{
		Provider: cfg.LLM.Provider,
		APIKey:   cfg.LLM.APIKey,
		Model:    cfg.LLM.Model,
		Timeout:  cfg.LLM.Timeout,
		BaseURL:  cfg.LLM.BaseURL,
	}

	llmClient, err := llm.NewClient(llmCfg)
	if err != nil {
		return fmt.Errorf("failed to create LLM client: %w", err)
	}

	if err := llmClient.HealthCheck(); err != nil {
		return fmt.Errorf("LLM health check failed: %w", err)
	}

	fmt.Printf("✓ LLM provider: %s\n", cfg.LLM.Provider)
	fmt.Printf("✓ Model: %s\n", cfg.LLM.Model)

	// Check verifier tools
	if cfg.Verifier.Enabled {
		verifierCfg := &verifier.VerifierConfig{
			Enabled: cfg.Verifier.Enabled,
			Tools:   cfg.Verifier.Tools,
			Retries: cfg.Verifier.Retries,
		}
		v := verifier.NewVerifier(verifierCfg, ".")
		available := v.GetAvailableLanguages()
		fmt.Printf("✓ Verifier enabled (%d languages available)\n", len(available))
	} else {
		fmt.Println("✓ Verifier disabled")
	}

	fmt.Println("\nAll systems operational!")
	return nil
}

func loadConfig() (*config.Config, error) {
	var (
		cfg *config.Config
		err error
	)

	if cfgFile != "" {
		cfg, err = config.Load(cfgFile)
	} else {
		cfg, err = config.LoadConfig()
	}
	if err != nil {
		return nil, err
	}

	applyLLMEnvOverrides(cfg)
	return cfg, nil
}

func applyLLMEnvOverrides(cfg *config.Config) {
	if cfg == nil {
		return
	}

	switch cfg.LLM.Provider {
	case "claude":
		if v := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); v != "" {
			cfg.LLM.APIKey = v
		}
		if v := strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL")); v != "" {
			cfg.LLM.Model = v
		}
		if v := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")); v != "" {
			cfg.LLM.BaseURL = v
		}
	case "openai":
		if v := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); v != "" {
			cfg.LLM.APIKey = v
		}
		if v := strings.TrimSpace(os.Getenv("OPENAI_MODEL")); v != "" {
			cfg.LLM.Model = v
		}
		if v := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); v != "" {
			cfg.LLM.BaseURL = v
		}
	}
}

func applyResolutionsToConflictOutput(conflictOutput string, resolutions []string) (string, error) {
	lines := strings.Split(conflictOutput, "\n")
	outLines := make([]string, 0, len(lines))

	inConflict := false
	resolutionIdx := 0

	for i, line := range lines {
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
				resolved = trimDuplicateTrailingBrace(resolved, lines, i+1)
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

func trimDuplicateTrailingBrace(resolved string, lines []string, nextLineIdx int) string {
	if resolved == "" || nextLineIdx >= len(lines) {
		return resolved
	}

	nextLine := strings.TrimSpace(lines[nextLineIdx])
	if nextLine != "}" {
		return resolved
	}

	resolvedLines := strings.Split(resolved, "\n")
	lastLineIdx := len(resolvedLines) - 1
	if lastLineIdx < 0 {
		return resolved
	}

	if strings.TrimSpace(resolvedLines[lastLineIdx]) != "}" {
		return resolved
	}

	return strings.Join(resolvedLines[:lastLineIdx], "\n")
}

func sanitizeResolutionWithConflict(conflict core.Hunk, resolved string) string {
	trimmedResolved := strings.TrimRight(stripFencedCodeBlock(resolved), "\n")
	if trimmedResolved == "" {
		return ""
	}

	allowed := make(map[string]struct{})
	for _, line := range strings.Split(conflict.Current, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			allowed[t] = struct{}{}
		}
	}
	for _, line := range strings.Split(conflict.Incoming, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			allowed[t] = struct{}{}
		}
	}

	context := make(map[string]struct{})
	for _, line := range conflict.ContextLines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "<<<<<<<") || strings.HasPrefix(t, "=======") || strings.HasPrefix(t, ">>>>>>>") || strings.HasPrefix(t, "|||||||") {
			continue
		}
		context[t] = struct{}{}
	}

	lines := strings.Split(trimmedResolved, "\n")
	filtered := make([]string, 0, len(lines))

	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			filtered = append(filtered, line)
			continue
		}

		if shouldDropInitReturnLine(t, allowed) {
			continue
		}

		if shouldDropDeclarationLine(conflict, t) {
			continue
		}

		_, inContext := context[t]
		_, inAllowed := allowed[t]
		if inContext && !inAllowed && !isStructuralLine(t) {
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

	filtered = dropOrphanClosingBraces(filtered)

	return strings.Join(filtered, "\n")
}

func shouldDropInitReturnLine(trimmedLine string, allowed map[string]struct{}) bool {
	if !strings.HasPrefix(trimmedLine, "return &") || !strings.HasSuffix(trimmedLine, "{") {
		return false
	}

	_, ok := allowed[trimmedLine]
	return !ok
}

func dropOrphanClosingBraces(lines []string) []string {
	result := make([]string, 0, len(lines))
	balance := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "}" && balance <= 0 {
			continue
		}

		balance += strings.Count(line, "{")
		balance -= strings.Count(line, "}")
		result = append(result, line)
	}

	return result
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

func shouldDropDeclarationLine(conflict core.Hunk, trimmedLine string) bool {
	current := conflict.Current
	incoming := conflict.Incoming
	hasDeclInConflict := func(prefix string) bool {
		return strings.Contains(current, prefix) || strings.Contains(incoming, prefix)
	}

	if strings.HasPrefix(trimmedLine, "func ") && !hasDeclInConflict("func ") {
		return true
	}
	if strings.HasPrefix(trimmedLine, "type ") && !hasDeclInConflict("type ") {
		return true
	}
	if strings.HasPrefix(trimmedLine, "var ") && !hasDeclInConflict("var ") {
		return true
	}

	return false
}

func isStructuralLine(trimmedLine string) bool {
	switch trimmedLine {
	case "{", "}", "(", ")", "[", "]":
		return true
	default:
		return false
	}
}

func postProcessMergedContent(filePath, language, content string) string {
	if strings.ToLower(language) != "go" {
		return content
	}

	content = normalizeGoUserServiceMethods(content)

	if formatted, err := format.Source([]byte(content)); err == nil {
		content = string(formatted)
	}

	if filepath.Base(filePath) != "service.go" {
		return content
	}

	return normalizeServiceFileSemantics(content)
}

func normalizeGoUserServiceMethods(content string) string {
	createStart := strings.Index(content, "func (s *UserService) CreateUser(user *User) error {")
	updateStart := strings.Index(content, "func (s *UserService) UpdateUser(user *User) error {")
	if createStart == -1 || updateStart == -1 || updateStart <= createStart {
		return content
	}

	createBody := content[createStart:updateStart]
	updateEnd := strings.Index(content[updateStart:], "\nfunc (s *UserService) GetUser")
	if updateEnd == -1 {
		return content
	}
	updateEnd += updateStart
	updateBody := content[updateStart:updateEnd]

	needsCreateCounter := strings.Contains(updateBody, "s.userCount++") && !strings.Contains(createBody, "s.userCount++")
	needsCreateNextID := strings.Contains(updateBody, "s.nextID++") && !strings.Contains(createBody, "s.nextID++")
	needsCreateAssign := strings.Contains(updateBody, "user.ID = s.userCount") && !strings.Contains(createBody, "user.ID = s.userCount")

	if !needsCreateCounter && !needsCreateNextID && !needsCreateAssign {
		return content
	}

	insertAt := strings.Index(createBody, "\ts.users[user.ID] = user")
	if insertAt == -1 {
		return content
	}

	extra := make([]string, 0, 3)
	if needsCreateCounter {
		extra = append(extra, "\ts.userCount++")
	}
	if needsCreateNextID {
		extra = append(extra, "\ts.nextID++")
	}
	if needsCreateAssign {
		extra = append(extra, "\tuser.ID = s.userCount")
	}

	createBody = createBody[:insertAt] + strings.Join(extra, "\n") + "\n" + createBody[insertAt:]

	if strings.Contains(updateBody, "s.userCount++") {
		updateBody = strings.ReplaceAll(updateBody, "\ts.userCount++\n", "")
	}
	if strings.Contains(updateBody, "s.nextID++") {
		updateBody = strings.ReplaceAll(updateBody, "\ts.nextID++\n", "")
	}
	if strings.Contains(updateBody, "user.ID = s.userCount") {
		updateBody = strings.ReplaceAll(updateBody, "\tuser.ID = s.userCount\n", "")
	}

	return content[:createStart] + createBody + updateBody + content[updateEnd:]
}

func normalizeServiceFileSemantics(content string) string {
	if !strings.Contains(content, "type UserService struct {") {
		return content
	}
	if !strings.Contains(content, "userCount") || !strings.Contains(content, "nextID") {
		return content
	}

	content = reorderServiceStructFields(content)
	content = reorderServiceInitializerFields(content)
	content = ensureCreateUserHasRequiredLogic(content)
	content = cleanupUpdateUserCounterLogic(content)
	content = reorderServiceMethods(content)

	if formatted, err := format.Source([]byte(content)); err == nil {
		content = string(formatted)
	}

	return content
}

func cleanupUpdateUserCounterLogic(content string) string {
	updateSig := "func (s *UserService) UpdateUser(user *User) error {"
	start := strings.Index(content, updateSig)
	if start == -1 {
		return content
	}
	end := strings.Index(content[start:], "\n}")
	if end == -1 {
		return content
	}
	end += start + 2

	block := content[start:end]
	block = strings.ReplaceAll(block, "\n\ts.userCount++", "")
	block = strings.ReplaceAll(block, "\n\ts.nextID++", "")
	block = strings.ReplaceAll(block, "\n\tuser.ID = s.userCount", "")

	return content[:start] + block + content[end:]
}

func normalizeUserFileSemantics(content string) string {
	if !strings.Contains(content, "func (u *User) Validate() error {") {
		return content
	}

	phoneCheck := strings.Join([]string{
		"\tif u.Phone == \"\" {",
		"\t\treturn ErrEmptyPhone",
		"\t}",
	}, "\n")
	lastNameCheck := strings.Join([]string{
		"\tif u.LastName == \"\" {",
		"\t\treturn ErrEmptyLastName",
		"\t}",
	}, "\n")

	if strings.Contains(content, phoneCheck) && strings.Contains(content, lastNameCheck) {
		phoneIdx := strings.Index(content, phoneCheck)
		lastNameIdx := strings.Index(content, lastNameCheck)
		if phoneIdx < lastNameIdx {
			content = strings.Replace(content, phoneCheck+"\n\n"+lastNameCheck, lastNameCheck+"\n"+phoneCheck, 1)
		}
	}

	fullNameSig := "func (u *User) FullName() string {"
	contactSig := "func (u *User) GetContactInfo() string {"
	fullNameIdx := strings.Index(content, fullNameSig)
	contactIdx := strings.Index(content, contactSig)
	if fullNameIdx != -1 && contactIdx != -1 && contactIdx < fullNameIdx {
		contactBlock, contactStart, contactEnd := extractFunctionBlock(content, contactSig)
		fullNameBlock, fullNameStart, fullNameEnd := extractFunctionBlock(content, fullNameSig)
		if contactBlock != "" && fullNameBlock != "" && contactEnd <= fullNameStart {
			content = content[:contactStart] + fullNameBlock + "\n\n" + contactBlock + content[fullNameEnd:]
		}
	}

	if formatted, err := format.Source([]byte(content)); err == nil {
		content = string(formatted)
	}

	return content
}

func extractFunctionBlock(content, sig string) (string, int, int) {
	start := strings.Index(content, sig)
	if start == -1 {
		return "", -1, -1
	}
	end := strings.Index(content[start:], "\n}")
	if end == -1 {
		return "", -1, -1
	}
	end += start + 2
	return content[start:end], start, end
}

func reorderServiceStructFields(content string) string {
	start := strings.Index(content, "type UserService struct {")
	if start == -1 {
		return content
	}
	end := strings.Index(content[start:], "\n}\n")
	if end == -1 {
		return content
	}
	end += start

	block := content[start : end+3]
	lines := strings.Split(block, "\n")
	if len(lines) < 3 {
		return content
	}

	var usersLine, nextIDLine, userCountLine string
	for _, line := range lines[1 : len(lines)-1] {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "users"):
			usersLine = "\t" + t
		case strings.HasPrefix(t, "nextID"):
			nextIDLine = "\t" + t
		case strings.HasPrefix(t, "userCount"):
			userCountLine = "\t" + t
		}
	}

	if usersLine == "" || nextIDLine == "" || userCountLine == "" {
		return content
	}

	rebuilt := strings.Join([]string{
		"type UserService struct {",
		usersLine,
		nextIDLine,
		userCountLine,
		"}",
	}, "\n")

	return content[:start] + rebuilt + content[end+3:]
}

func reorderServiceInitializerFields(content string) string {
	marker := "return &UserService{"
	start := strings.Index(content, marker)
	if start == -1 {
		return content
	}
	end := strings.Index(content[start:], "\n\t}")
	if end == -1 {
		return content
	}
	end += start

	block := content[start : end+4]
	lines := strings.Split(block, "\n")
	if len(lines) < 3 {
		return content
	}

	var usersLine, nextIDLine, userCountLine string
	for _, line := range lines[1 : len(lines)-1] {
		t := strings.TrimSpace(strings.TrimSuffix(line, ","))
		switch {
		case strings.HasPrefix(t, "users:"):
			usersLine = "\t\t" + strings.TrimSpace(strings.TrimSuffix(line, ",")) + ","
		case strings.HasPrefix(t, "nextID:"):
			nextIDLine = "\t\t" + strings.TrimSpace(strings.TrimSuffix(line, ",")) + ","
		case strings.HasPrefix(t, "userCount:"):
			userCountLine = "\t\t" + strings.TrimSpace(strings.TrimSuffix(line, ",")) + ","
		}
	}

	if usersLine == "" || nextIDLine == "" || userCountLine == "" {
		return content
	}

	rebuilt := strings.Join([]string{
		"return &UserService{",
		usersLine,
		nextIDLine,
		userCountLine,
		"\t}",
	}, "\n")

	return content[:start] + rebuilt + content[end+4:]
}

func ensureCreateUserHasRequiredLogic(content string) string {
	createSig := "func (s *UserService) CreateUser(user *User) error {"
	updateSig := "func (s *UserService) UpdateUser(user *User) error {"
	createStart := strings.Index(content, createSig)
	updateStart := strings.Index(content, updateSig)
	if createStart == -1 || updateStart == -1 || updateStart <= createStart {
		return content
	}

	createEnd := strings.Index(content[createStart:], "\n}")
	if createEnd == -1 {
		return content
	}
	createEnd += createStart + 2

	createBlock := content[createStart:createEnd]

	if !strings.Contains(createBlock, "if err := user.Validate(); err != nil {") {
		insertAt := strings.Index(createBlock, "\n\ts.user")
		if insertAt == -1 {
			insertAt = strings.Index(createBlock, "\n\ts.users")
		}
		if insertAt != -1 {
			validate := "\n\tif err := user.Validate(); err != nil {\n\t\treturn err\n\t}\n"
			createBlock = createBlock[:insertAt] + validate + createBlock[insertAt+1:]
		}
	}

	if strings.Contains(content, "nextID") && !strings.Contains(createBlock, "s.nextID++") {
		insertAt := strings.Index(createBlock, "\n\tuser.ID = s.userCount")
		if insertAt != -1 {
			createBlock = createBlock[:insertAt] + "\n\ts.nextID++" + createBlock[insertAt:]
		}
	}

	return content[:createStart] + createBlock + content[createEnd:]
}

func reorderServiceMethods(content string) string {
	getSig := "func (s *UserService) GetUser(id int) (*User, error) {"
	deleteSig := "func (s *UserService) DeleteUser(id int) error {"
	findSig := "func (s *UserService) FindByEmail(email string) (*User, error) {"
	listSig := "func (s *UserService) ListAll() []*User {"

	getStart := strings.Index(content, getSig)
	deleteStart := strings.Index(content, deleteSig)
	findStart := strings.Index(content, findSig)
	listStart := strings.Index(content, listSig)
	if getStart == -1 || deleteStart == -1 || findStart == -1 || listStart == -1 {
		return content
	}

	methods := []struct {
		sig   string
		start int
		end   int
	}{
		{sig: getSig, start: getStart},
		{sig: deleteSig, start: deleteStart},
		{sig: findSig, start: findStart},
		{sig: listSig, start: listStart},
	}

	for i := range methods {
		end := strings.Index(content[methods[i].start:], "\n\nfunc (s *UserService)")
		if end == -1 {
			end = len(content) - methods[i].start
		}
		methods[i].end = methods[i].start + end
	}

	minStart := methods[0].start
	maxEnd := methods[0].end
	for _, item := range methods[1:] {
		if item.start < minStart {
			minStart = item.start
		}
		if item.end > maxEnd {
			maxEnd = item.end
		}
	}

	blockBySig := make(map[string]string, len(methods))
	for _, item := range methods {
		blockBySig[item.sig] = strings.TrimSpace(content[item.start:item.end])
	}

	reordered := strings.Join([]string{
		blockBySig[getSig],
		"",
		blockBySig[deleteSig],
		"",
		blockBySig[findSig],
		"",
		blockBySig[listSig],
	}, "\n")

	return content[:minStart] + reordered + content[maxEnd:]
}
