package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/semerge/semerge/internal/config"
	"github.com/semerge/semerge/pkg/llm"
	"github.com/semerge/semerge/pkg/verifier"
	"github.com/semerge/semerge/pkg/workflow"

	"github.com/spf13/cobra"
)

var (
	cfgFile string
	version = "0.2.1"
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

	var initCmd = &cobra.Command{
		Use:   "init",
		Short: "Initialize semerge configuration",
		RunE:  runInit,
	}

	var resolveCmd = &cobra.Command{
		Use:   "resolve [ancestor] [current] [incoming]",
		Short: "Resolve merge conflicts in a file",
		Args:  cobra.ExactArgs(3),
		RunE:  runResolve,
	}

	var installCmd = &cobra.Command{
		Use:   "install",
		Short: "Install semerge as a git merge driver",
		RunE:  runInstall,
	}

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
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.LLM.Timeout)*time.Second)
	defer cancel()

	result, err := workflow.ResolveAndWrite(ctx, cfg, args[0], args[1], args[2], ".", version)
	if err != nil {
		return err
	}

	if result.ConflictCount == 0 {
		fmt.Println("No conflicts found.")
		return nil
	}

	fmt.Printf("\nResolved file written to: %s\n", result.FilePath)
	if len(result.Trailers) > 0 {
		fmt.Println("Conflict trailer metadata:")
		for _, trailer := range result.Trailers {
			fmt.Println(trailer)
		}
	}

	return nil
}

func runInstall(cmd *cobra.Command, args []string) error {
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

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration invalid: %w", err)
	}

	fmt.Println("✓ Configuration is valid")

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
