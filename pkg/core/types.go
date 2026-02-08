package core

import "time"

// Hunk represents a single conflict block
type Hunk struct {
	StartLine    int      `json:"start_line"`
	EndLine      int      `json:"end_line"`
	Ancestor     string   `json:"ancestor"`
	Current      string   `json:"current"`
	Incoming     string   `json:"incoming"`
	FileName     string   `json:"file_name"`
	Language     string   `json:"language"`
	ContextLines []string `json:"context_lines,omitempty"`
}

// Conflict represents a merge conflict in a file
type Conflict struct {
	FileName string `json:"file_name"`
	Language string `json:"language"`
	Hunks    []Hunk `json:"hunks"`
}

// MergeRequest represents a three-way merge request
type MergeRequest struct {
	FileName     string `json:"file_name"`
	Language     string `json:"language"`
	AncestorPath string `json:"ancestor_path"`
	CurrentPath  string `json:"current_path"`
	IncomingPath string `json:"incoming_path"`
	Conflicts    []Hunk `json:"conflicts"`
}

// MergeResult represents the result of a merge operation
type MergeResult struct {
	Success       bool           `json:"success"`
	Content       string         `json:"content"`
	ResolvedMap   map[int]string `json:"resolved_map,omitempty"`
	ResolvedHunks []Hunk         `json:"resolved_hunks"`
	Errors        []string       `json:"errors,omitempty"`
	Timestamp     time.Time      `json:"timestamp"`
}

// Context contains AST-derived context for a conflict
type Context struct {
	FileName  string            `json:"file_name"`
	Language  string            `json:"language"`
	Skeleton  string            `json:"skeleton"`  // Skeleton code with only key structures
	Imports   []string          `json:"imports"`   // Import statements
	Functions []FunctionContext `json:"functions"` // Related function signatures
	Classes   []ClassContext    `json:"classes"`   // Related class definitions
	Variables []VariableContext `json:"variables"` // Variable declarations
	Symbols   map[string]string `json:"symbols"`   // Symbol table
}

// FunctionContext represents a function definition context
type FunctionContext struct {
	Name       string   `json:"name"`
	Signature  string   `json:"signature"`
	Parameters []string `json:"parameters"`
	ReturnType string   `json:"return_type"`
	Body       string   `json:"body,omitempty"`
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
}

// ClassContext represents a class/struct definition context
type ClassContext struct {
	Name      string            `json:"name"`
	Fields    []string          `json:"fields"`
	Methods   []FunctionContext `json:"methods"`
	StartLine int               `json:"start_line"`
	EndLine   int               `json:"end_line"`
}

// VariableContext represents a variable declaration
type VariableContext struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Value     string `json:"value,omitempty"`
	StartLine int    `json:"start_line"`
}

// LLMResponse represents a response from the LLM
type LLMResponse struct {
	Analysis   string   `json:"analysis"`
	CodeBlock  string   `json:"code_block"`
	Confidence float64  `json:"confidence"`
	Reasoning  []string `json:"reasoning"`
	Resolution string   `json:"resolution"`
}

// VerificationResult represents the result of linting/compilation
type VerificationResult struct {
	Success  bool     `json:"success"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
	ExitCode int      `json:"exit_code"`
	Tool     string   `json:"tool"`
	Output   string   `json:"output"`
}

// VerifierFeedback represents structured verifier feedback for retrying LLM resolution
type VerifierFeedback struct {
	Attempt           int      `json:"attempt"`
	Tool              string   `json:"tool"`
	ExitCode          int      `json:"exit_code"`
	TopErrors         []string `json:"top_errors,omitempty"`
	TopWarnings       []string `json:"top_warnings,omitempty"`
	PreviousCandidate string   `json:"previous_candidate,omitempty"`
}

// Config represents the semerge configuration
type Config struct {
	LLM struct {
		Provider string `yaml:"provider"` // "claude" or "openai"
		APIKey   string `yaml:"api_key"`
		Model    string `yaml:"model"`
		Timeout  int    `yaml:"timeout"` // seconds
	} `yaml:"llm"`

	Verifier struct {
		Enabled bool              `yaml:"enabled"`
		Tools   map[string]string `yaml:"tools"` // language -> command
		Retries int               `yaml:"retries"`
	} `yaml:"verifier"`

	Context struct {
		MaxLines       int  `yaml:"max_lines"`
		IncludeImports bool `yaml:"include_imports"`
		UseTreeSitter  bool `yaml:"use_tree_sitter"`
	} `yaml:"context"`

	Git struct {
		MergeStrategy string `yaml:"merge_strategy"` // "auto", "semantic", "text"
		FallbackText  bool   `yaml:"fallback_text"`
	} `yaml:"git"`
}
