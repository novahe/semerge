package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/semerge/semerge/internal/config"
	contextpkg "github.com/semerge/semerge/pkg/context"
	"github.com/semerge/semerge/pkg/core"
	"github.com/semerge/semerge/pkg/hunk"
	"github.com/semerge/semerge/pkg/llm"
	"github.com/semerge/semerge/pkg/verifier"
)

var (
	cfgFile string
	version = "0.1.0"
)

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
	if cfg.Context.UseTreeSitter {
		ctxEngine = contextpkg.NewEngine()
		ctxEngine.SetMaxLines(cfg.Context.MaxLines)

		mergeCtx, err := ctxEngine.ExtractContext(current, mergeRequest.Conflicts)
		if err != nil {
			fmt.Printf("Warning: Failed to extract context: %v\n", err)
		} else {
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

	// Create LLM request
	llmRequest := &llm.MergeRequest{
		FileName: mergeRequest.FileName,
		Language: mergeRequest.Language,
		Hunks:    mergeRequest.Conflicts,
		// Context will be added if available
	}

	// Resolve conflicts
	fmt.Println("\nResolving conflicts with AI...")
	result, err := llmClient.Resolve(ctx, llmRequest)
	if err != nil {
		return fmt.Errorf("failed to resolve conflicts: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("resolution failed: %v", result.Errors)
	}

	fmt.Println("Conflicts resolved successfully!")

	// Merge the resolved hunk back into the original file
	mergedContent, err := mergeHunkIntoFile(current, mergeRequest.Conflicts, result.Content)
	if err != nil {
		return fmt.Errorf("failed to merge hunk into file: %w", err)
	}

	// Verify the result if enabled
	if cfg.Verifier.Enabled {
		fmt.Println("\nVerifying resolution...")

		// Convert config.VerifierConfig to verifier.VerifierConfig
		verifierCfg := &verifier.VerifierConfig{
			Enabled: cfg.Verifier.Enabled,
			Tools:   cfg.Verifier.Tools,
			Retries: cfg.Verifier.Retries,
		}
		v := verifier.NewVerifier(verifierCfg, ".")

		// Verify the merged file
		verifyResult, err := v.Verify(ctx, mergedContent, current, mergeRequest.Language)
		if err != nil {
			fmt.Printf("Warning: Verification failed: %v\n", err)
		} else if !verifyResult.Success {
			fmt.Printf("Warning: Linter found issues:\n")
			for _, e := range verifyResult.Errors {
				fmt.Printf("  - %s\n", e)
			}
		} else {
			fmt.Println("Verification passed!")
		}
	}

	// Write the merged content to the current file
	if err := os.WriteFile(current, []byte(mergedContent), 0644); err != nil {
		return fmt.Errorf("failed to write resolved file: %w", err)
	}

	fmt.Printf("\nResolved file written to: %s\n", current)
	return nil
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

// mergeHunkIntoFile merges the resolved hunk back into the original file
// If conflict markers exist in the file, it replaces them with the resolved content
// Otherwise, it uses the conflict's Current field to guide the replacement
func mergeHunkIntoFile(filePath string, conflicts []core.Hunk, resolvedHunk string) (string, error) {
	// Read the original file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	fileStr := string(content)

	if len(conflicts) == 0 {
		return fileStr, nil
	}

	// For now, handle single conflict (most common case)
	conflict := conflicts[0]

	// Try to find conflict markers first
	startMarker := "<<<<<<<"
	endMarker := ">>>>>>>"

	startIdx := strings.Index(fileStr, startMarker)
	if startIdx != -1 {
		// Found conflict markers - replace them
		endIdx := strings.Index(fileStr[startIdx:], endMarker)
		if endIdx == -1 {
			return "", fmt.Errorf("found start marker but no end marker")
		}
		endIdx += startIdx + len(endMarker)

		var merged strings.Builder
		merged.WriteString(fileStr[:startIdx])
		merged.WriteString(resolvedHunk)
		merged.WriteString(fileStr[endIdx:])

		return merged.String(), nil
	}

	// No conflict markers found - try to find and replace the Current section
	// This handles the case where the file is clean (from git show :2:path)
	currentStr := conflict.Current
	if currentStr == "" {
		// No current content to guide us, just append at end
		return fileStr + "\n" + resolvedHunk, nil
	}

	// Try to find the Current content in the file and replace it
	currentIdx := strings.Index(fileStr, currentStr)
	if currentIdx != -1 {
		endIdx := currentIdx + len(currentStr)

		var merged strings.Builder
		merged.WriteString(fileStr[:currentIdx])
		merged.WriteString(resolvedHunk)
		merged.WriteString(fileStr[endIdx:])

		result := merged.String()

		// Fix common issue: if resolved hunk ends with } and next line also starts with },
		// remove the duplicate closing brace
		// This happens when the Current hunk didn't include the closing brace,
		// but the LLM resolution did
		result = strings.ReplaceAll(result, "}\n}", "}")

		return result, nil
	}

	// Can't find where to merge, just return the resolved hunk
	// (This shouldn't happen in normal operation)
	return resolvedHunk, nil
}

func loadConfig() (*config.Config, error) {
	if cfgFile != "" {
		return config.Load(cfgFile)
	}
	return config.LoadConfig()
}
