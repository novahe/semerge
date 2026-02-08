package verifier

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/semerge/semerge/pkg/core"
)

// Verifier handles code verification using linters
type Verifier struct {
	config    *VerifierConfig
	retries   int
	workDir   string
}

// VerifierConfig represents verifier configuration
type VerifierConfig struct {
	Enabled bool                      `yaml:"enabled"`
	Tools   map[string]string         `yaml:"tools"` // language -> command
	Retries int                       `yaml:"retries"`
}

// NewVerifier creates a new verifier
func NewVerifier(config *VerifierConfig, workDir string) *Verifier {
	retries := config.Retries
	if retries == 0 {
		retries = 3
	}

	return &Verifier{
		config:  config,
		retries: retries,
		workDir: workDir,
	}
}

// Verify verifies the merged code using language-specific linters
func (v *Verifier) Verify(ctx context.Context, content string, filePath string, language string) (*core.VerificationResult, error) {
	if !v.config.Enabled {
		return &core.VerificationResult{
			Success: true,
			Tool:    "disabled",
		}, nil
	}

	// Get the linter command for this language
	toolCmd, ok := v.config.Tools[language]
	if !ok {
		// No linter configured for this language, return success
		return &core.VerificationResult{
			Success: true,
			Tool:    "none",
		}, nil
	}

	// Create a temporary file with the content
	tmpFile, err := v.createTempFile(content, filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile)

	// Run the linter with retry logic
	var lastErr error
	for attempt := 0; attempt < v.retries; attempt++ {
		result, err := v.runLinter(ctx, toolCmd, tmpFile, language)
		if err != nil {
			lastErr = err
			continue
		}

		// If verification passed, return success
		if result.Success {
			return result, nil
		}

		// If verification failed, return the errors
		return result, nil
	}

	// All retries failed
	return &core.VerificationResult{
		Success: false,
		Errors:  []string{lastErr.Error()},
		Tool:    toolCmd,
	}, lastErr
}

// VerifyWithRetry verifies with automatic repair attempts
func (v *Verifier) VerifyWithRetry(ctx context.Context, content string, filePath string, language string, repairFunc func(content string, errors []string) (string, error)) (*core.VerificationResult, string, error) {
	currentContent := content

	for attempt := 0; attempt < v.retries; attempt++ {
		result, err := v.Verify(ctx, currentContent, filePath, language)
		if err != nil {
			return nil, currentContent, err
		}

		if result.Success {
			return result, currentContent, nil
		}

		// Try to repair the content
		if repairFunc != nil {
			repaired, err := repairFunc(currentContent, result.Errors)
			if err != nil {
				return nil, currentContent, fmt.Errorf("repair failed: %w", err)
			}
			currentContent = repaired
		} else {
			// No repair function, return the failure
			return result, currentContent, nil
		}
	}

	return &core.VerificationResult{
		Success: false,
		Errors:  []string{"max retries exceeded"},
	}, currentContent, nil
}

// createTempFile creates a temporary file with the given content
func (v *Verifier) createTempFile(content string, originalPath string) (string, error) {
	// Create temp file with same extension
	ext := filepath.Ext(originalPath)
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("semerge_*%s", ext))
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	// Write content
	if _, err := tmpFile.WriteString(content); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}

	return tmpFile.Name(), nil
}

// runLinter runs a linter command on a file
func (v *Verifier) runLinter(ctx context.Context, toolCmd string, filePath string, language string) (*core.VerificationResult, error) {
	// Parse command
	parts := strings.Fields(toolCmd)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty tool command")
	}

	// Replace {file} placeholder with actual file path
	args := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		arg := parts[i]
		if strings.Contains(arg, "{file}") {
			arg = strings.ReplaceAll(arg, "{file}", filePath)
		}
		args = append(args, arg)
	}

	cmd := exec.CommandContext(ctx, parts[0], args...)
	cmd.Dir = v.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &core.VerificationResult{
		Tool:   parts[0],
		Output: stdout.String() + "\n" + stderr.String(),
	}

	// Check exit code
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		}

		// Parse errors from output
		result.Errors = v.parseErrors(stdout.String(), stderr.String(), language)
		result.Warnings = v.parseWarnings(stdout.String(), stderr.String(), language)

		// Non-zero exit code typically means errors
		result.Success = false
	} else {
		result.ExitCode = 0
		result.Success = true
	}

	return result, nil
}

// parseErrors parses errors from linter output
func (v *Verifier) parseErrors(stdout, stderr string, language string) []string {
	output := stdout + "\n" + stderr
	var errors []string

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Filter for error lines based on language
		if v.isErrorLine(line, language) {
			errors = append(errors, line)
		}
	}

	return errors
}

// parseWarnings parses warnings from linter output
func (v *Verifier) parseWarnings(stdout, stderr string, language string) []string {
	output := stdout + "\n" + stderr
	var warnings []string

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Filter for warning lines based on language
		if v.isWarningLine(line, language) {
			warnings = append(warnings, line)
		}
	}

	return warnings
}

// isErrorLine checks if a line is an error
func (v *Verifier) isErrorLine(line string, language string) bool {
	lower := strings.ToLower(line)

	// Common error indicators
	errorIndicators := []string{
		"error:",
		"err:",
		"failed",
		"cannot",
		"undefined",
		"type error",
		"syntax error",
		"parse error",
	}

	for _, indicator := range errorIndicators {
		if strings.Contains(lower, indicator) {
			return true
		}
	}

	// Language-specific patterns
	switch language {
	case "go":
		return strings.Contains(line, ".go:") && !strings.Contains(lower, "warning")
	case "python":
		return strings.Contains(lower, "error") && !strings.Contains(lower, "warning")
	case "javascript", "typescript":
		return strings.Contains(line, "error") && !strings.Contains(lower, "warning")
	}

	return false
}

// isWarningLine checks if a line is a warning
func (v *Verifier) isWarningLine(line string, language string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "warning") || strings.Contains(lower, "warn")
}

// GetDefaultTools returns default linter commands for various languages
func GetDefaultTools() map[string]string {
	return map[string]string{
		"go":          "go vet {file}",
		"python":      "ruff check {file}",
		"javascript":  "eslint {file}",
		"typescript":  "eslint {file}",
		"java":        "javac -Xlint:all {file}",
		"cpp":         "g++ -fsyntax-only -Wall -Werror {file}",
		"c":           "gcc -fsyntax-only -Wall -Werror {file}",
		"rust":        "rustc --crate-type lib {file}",
		"ruby":        "rubocop {file}",
		"php":         "php -l {file}",
	}
}

// CheckToolAvailability checks if a linter tool is available
func (v *Verifier) CheckToolAvailability(language string) bool {
	toolCmd, ok := v.config.Tools[language]
	if !ok {
		return false
	}

	parts := strings.Fields(toolCmd)
	if len(parts) == 0 {
		return false
	}

	_, err := exec.LookPath(parts[0])
	return err == nil
}

// GetAvailableLanguages returns list of languages with available linters
func (v *Verifier) GetAvailableLanguages() []string {
	var languages []string
	for lang := range v.config.Tools {
		if v.CheckToolAvailability(lang) {
			languages = append(languages, lang)
		}
	}
	return languages
}
