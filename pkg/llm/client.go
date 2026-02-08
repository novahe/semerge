package llm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"text/template"
	"time"

	anthropic "github.com/liushuangls/go-anthropic/v2"
	"github.com/semerge/semerge/pkg/core"
)

var (
	// Global logger for the llm package
	logger *slog.Logger
)

func init() {
	// Initialize logger with text handler
	logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)
}

// Provider represents an LLM provider interface
type Provider interface {
	Resolve(ctx context.Context, request *MergeRequest) (*core.MergeResult, error)
	HealthCheck() error
}

// MergeRequest contains all information needed for LLM merge resolution
type MergeRequest struct {
	FileName          string        `json:"file_name"`
	Language          string        `json:"language"`
	Hunks             []core.Hunk   `json:"hunks"`
	Context           *core.Context `json:"context"`
	Config            *LLMConfig    `json:"config"`
	Attempt           int           `json:"attempt,omitempty"`
	RetryFeedback     string        `json:"retry_feedback,omitempty"`
	PreviousCandidate string        `json:"previous_candidate,omitempty"`
}

// LLMConfig represents LLM configuration
type LLMConfig struct {
	Provider string `json:"provider"` // "claude" or "openai"
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
	Timeout  int    `json:"timeout"` // seconds
	BaseURL  string `json:"base_url,omitempty"`
}

// Client handles LLM API interactions
type Client struct {
	provider Provider
	config   *LLMConfig
}

// NewClient creates a new LLM client
func NewClient(config *LLMConfig) (*Client, error) {
	var provider Provider
	var err error

	switch config.Provider {
	case "claude":
		provider, err = NewClaudeClient(config)
	case "openai":
		provider, err = NewOpenAIClient(config)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", config.Provider)
	}

	if err != nil {
		return nil, err
	}

	return &Client{
		provider: provider,
		config:   config,
	}, nil
}

// Resolve resolves merge conflicts using the LLM
func (c *Client) Resolve(ctx context.Context, request *MergeRequest) (*core.MergeResult, error) {
	return c.provider.Resolve(ctx, request)
}

// HealthCheck checks if the LLM API is accessible
func (c *Client) HealthCheck() error {
	return c.provider.HealthCheck()
}

// ClaudeClient implements the Provider interface for Claude API
type ClaudeClient struct {
	client *anthropic.Client
	model  anthropic.Model
}

// NewClaudeClient creates a new Claude API client
func NewClaudeClient(config *LLMConfig) (*ClaudeClient, error) {
	apiKey := config.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
		}
	}

	model := anthropic.Model(config.Model)
	if config.Model == "" {
		model = anthropic.Model("glm4.7")
	}

	// Create client with optional base URL
	var client *anthropic.Client
	if config.BaseURL != "" {
		baseURL := normalizeAnthropicBaseURL(config.BaseURL)
		client = anthropic.NewClient(apiKey,
			anthropic.WithBaseURL(baseURL),
		)
	} else {
		client = anthropic.NewClient(apiKey)
	}

	return &ClaudeClient{
		client: client,
		model:  model,
	}, nil
}

func normalizeAnthropicBaseURL(raw string) string {
	baseURL := strings.TrimSpace(raw)
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return ""
	}

	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL
	}

	return baseURL + "/v1"
}

// Resolve resolves conflicts using Claude API
func (c *ClaudeClient) Resolve(ctx context.Context, request *MergeRequest) (*core.MergeResult, error) {
	prompt, err := BuildPrompt(request)
	if err != nil {
		return nil, fmt.Errorf("failed to build prompt: %w", err)
	}

	if isDebugEnabled() {
		slog.Debug("=== LLM Request (Prompt) ===")
		slog.Debug("prompt", "content", prompt)
		slog.Debug("=== End of LLM Request ===")
	}

	// Create the message
	messages := []anthropic.Message{
		anthropic.NewUserTextMessage(prompt),
	}

	// Create request
	req := anthropic.MessagesRequest{
		Model:     c.model,
		MaxTokens: 8192,
		Messages:  messages,
	}

	// Call Claude API
	resp, err := c.client.CreateMessages(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("Claude API error: %w", err)
	}

	if isDebugEnabled() {
		slog.Debug("=== API Raw Response ===",
			"id", resp.ID,
			"type", resp.Type,
			"role", resp.Role,
			"content_blocks", len(resp.Content))
		for i, block := range resp.Content {
			text := ""
			if block.Text != nil {
				text = *block.Text
			}
			slog.Debug("content block",
				"index", i,
				"type", block.Type,
				"text", text)
		}
		slog.Debug("=== End API Raw Response ===")
	}

	// Extract the response text
	var responseText string
	for _, content := range resp.Content {
		if content.Type == anthropic.MessagesContentTypeText && content.Text != nil {
			responseText += *content.Text
		}
	}

	if isDebugEnabled() {
		slog.Debug("=== LLM Response ===")
		slog.Debug("response", "content", responseText)
		slog.Debug("=== End of LLM Response ===")
	}

	// Parse the response to extract resolution
	result, err := ParseLLMResponse(responseText)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return result, nil
}

// HealthCheck checks if Claude API is accessible
func (c *ClaudeClient) HealthCheck() error {
	if c.client == nil {
		return fmt.Errorf("client not initialized")
	}
	return nil
}

// OpenAIClient implements the Provider interface for OpenAI API
type OpenAIClient struct {
	apiKey  string
	model   string
	baseURL string
}

// NewOpenAIClient creates a new OpenAI API client
func NewOpenAIClient(config *LLMConfig) (*OpenAIClient, error) {
	apiKey := config.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY not set")
		}
	}

	model := config.Model
	if model == "" {
		model = "gpt-4o"
	}

	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1/chat/completions"
	}

	return &OpenAIClient{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
	}, nil
}

// Resolve resolves conflicts using OpenAI API
func (c *OpenAIClient) Resolve(ctx context.Context, request *MergeRequest) (*core.MergeResult, error) {
	// TODO: Implement OpenAI API integration
	return &core.MergeResult{
		Success:   false,
		Errors:    []string{"OpenAI integration not yet implemented"},
		Timestamp: time.Now(),
	}, nil
}

// HealthCheck checks if OpenAI API is accessible
func (c *OpenAIClient) HealthCheck() error {
	if c.apiKey == "" {
		return fmt.Errorf("API key not set")
	}
	return nil
}

// BuildPrompt builds the prompt for the LLM
func BuildPrompt(request *MergeRequest) (string, error) {
	promptTemplate := `
You are an expert code merge conflict resolver. Analyze the following merge conflict and provide a semantic merge solution.

File: {{.FileName}}
Language: {{.Language}}

{{if .Context}}
=== Code Context ===
{{- if .Context.Imports}}
Imports:
{{range .Context.Imports}}  {{.}}
{{end}}
{{- end}}

{{- if .Context.Functions}}
Related Functions:
{{range .Context.Functions}}  - {{.Name}}: {{.Signature}}
{{end}}
{{- end}}

{{- if .Context.Classes}}
Related Classes:
{{range .Context.Classes}}  - {{.Name}}
{{end}}
{{- end}}

{{- if .Context.Skeleton}}
Code Skeleton:
{{.Context.Skeleton}}
{{- end}}
{{- end}}

=== Conflicts to Resolve ===
{{range $i, $h := .Hunks}}
--- Conflict ID: {{$i}} (lines {{$h.StartLine}}-{{$h.EndLine}}) ---

<<<<<<< CURRENT
{{$h.Current}}
=======
{{$h.Incoming}}
>>>>>>> INCOMING

Context around conflict:
{{range $h.ContextLines}}  {{.}}
{{end}}

---
{{end}}

=== Instructions ===

{{if gt .Attempt 1}}
=== Previous Attempt Failed Verification ===
Attempt: {{.Attempt}}

{{if .RetryFeedback}}Verification feedback:
{{.RetryFeedback}}
{{end}}

{{if .PreviousCandidate}}Previous failed candidate:
{{.PreviousCandidate}}
{{end}}

You MUST fix the verification failures and return a corrected resolution for the same conflict.
{{end}}

1. Analyze the semantic intent of both branches
2. Identify if changes are compatible or conflicting
3. Provide a merged solution that preserves both intents
4. IMPORTANT: When there are naming conflicts (e.g., variable names, function names), prefer the CURRENT branch's naming over the INCOMING branch
5. Preserve behavior from both branches; do not remove critical assignments, validations, or state updates unless truly contradictory
6. Prefer minimal, compositional merges: keep existing statements and ordering when possible; avoid inventing new conditions or side effects not present in either branch
7. CRITICAL: Return ALL resolutions using XML tags with conflict IDs:
   - Format: <resolution id="0">...</resolution>
   - Return one <resolution id="N"> block for each conflict ID above
   - Do NOT return the whole file
   - Do NOT include package declarations, imports, or unrelated code
   - Do NOT include conflict markers like <<<<<<<, =======, >>>>>>>

8. Return your response in the following XML format:

<analysis>
Your detailed analysis of the conflict, including:
- What each branch is trying to achieve
- Whether the changes are compatible
- Any dependencies or relationships affected
</analysis>

<resolution id="0">
Return ONLY the resolved code for that conflict region.
Do NOT include package/imports/other functions.
</resolution>

<confidence>
A number from 0.0 to 1.0 indicating your confidence in this resolution
</confidence>

<reasoning>
Step-by-step reasoning of your merge decision
</reasoning>
`

	tmpl, err := template.New("merge").Parse(promptTemplate)
	if err != nil {
		return "", err
	}

	// Use a buffer to build the prompt
	var buf strings.Builder
	if err := tmpl.Execute(&buf, request); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// ParseLLMResponse parses the LLM response into a MergeResult
func ParseLLMResponse(text string) (*core.MergeResult, error) {
	re := regexp.MustCompile(`(?s)<resolution\s+id="(\d+)">(.*?)</resolution>`)
	matches := re.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return &core.MergeResult{
			Success:   false,
			Errors:    []string{"No resolution blocks with id provided by LLM"},
			Timestamp: time.Now(),
		}, nil
	}

	resolvedMap := make(map[int]string, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}

		var id int
		if _, err := fmt.Sscanf(match[1], "%d", &id); err != nil {
			return &core.MergeResult{
				Success:   false,
				Errors:    []string{fmt.Sprintf("Invalid resolution id: %q", match[1])},
				Timestamp: time.Now(),
			}, nil
		}

		if _, exists := resolvedMap[id]; exists {
			return &core.MergeResult{
				Success:   false,
				Errors:    []string{fmt.Sprintf("Duplicate resolution id: %d", id)},
				Timestamp: time.Now(),
			}, nil
		}

		content := trimOuterNewlines(match[2])
		if strings.TrimSpace(content) == "" {
			return &core.MergeResult{
				Success:   false,
				Errors:    []string{fmt.Sprintf("Empty resolution for id: %d", id)},
				Timestamp: time.Now(),
			}, nil
		}

		resolvedMap[id] = content
	}

	return &core.MergeResult{
		Success:     true,
		ResolvedMap: resolvedMap,
		Timestamp:   time.Now(),
	}, nil
}

func extractFencedCodeBlock(text string) string {
	re := regexp.MustCompile("(?s)```(?:[a-zA-Z0-9_+-]+)?\\n(.*?)```")
	matches := re.FindStringSubmatch(text)
	if len(matches) > 1 {
		return trimOuterNewlines(matches[1])
	}
	return ""
}

func trimOuterNewlines(text string) string {
	return strings.Trim(text, "\n\r")
}

// isDebugEnabled checks if SEMERGE_DEBUG environment variable is set
// and dynamically adjusts the log level
func isDebugEnabled() bool {
	if os.Getenv("SEMERGE_DEBUG") == "true" || os.Getenv("SEMERGE_DEBUG") == "1" {
		// Enable debug logging
		opts := &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}
		logger = slog.New(slog.NewTextHandler(os.Stdout, opts))
		slog.SetDefault(logger)
		return true
	}
	return false
}
