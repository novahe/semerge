package context

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	"github.com/tree-sitter/tree-sitter-go/bindings/go"
	"github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	"github.com/tree-sitter/tree-sitter-python/bindings/go"
	"github.com/tree-sitter/tree-sitter-typescript/bindings/go"
	"github.com/tree-sitter/tree-sitter-java/bindings/go"
	"github.com/tree-sitter/tree-sitter-cpp/bindings/go"

	"github.com/semerge/semerge/pkg/core"
)

// Engine handles AST parsing and context extraction
type Engine struct {
	parser   *sitter.Parser
	maxLines int
}

// NewEngine creates a new context engine
func NewEngine() *Engine {
	return &Engine{
		parser:   sitter.NewParser(),
		maxLines: 1000,
	}
}

// SetMaxLines sets the maximum number of lines to process
func (e *Engine) SetMaxLines(max int) {
	e.maxLines = max
}

// GetLanguage returns the tree-sitter language for a given language string
func (e *Engine) GetLanguage(lang string) (*sitter.Language, error) {
	switch strings.ToLower(lang) {
	case "python", "py":
		return sitter.NewLanguage(tree_sitter_python.Language()), nil
	case "go":
		return sitter.NewLanguage(tree_sitter_go.Language()), nil
	case "javascript", "js":
		return sitter.NewLanguage(tree_sitter_javascript.Language()), nil
	case "typescript", "ts":
		return sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript()), nil
	case "tsx":
		return sitter.NewLanguage(tree_sitter_typescript.LanguageTSX()), nil
	case "java":
		return sitter.NewLanguage(tree_sitter_java.Language()), nil
	case "c":
		return sitter.NewLanguage(tree_sitter_cpp.Language()), nil
	case "cpp", "cc", "cxx":
		return sitter.NewLanguage(tree_sitter_cpp.Language()), nil
	default:
		return nil, fmt.Errorf("unsupported language: %s", lang)
	}
}

// ExtractContext extracts context from a file for given conflict hunks
func (e *Engine) ExtractContext(filePath string, hunks []core.Hunk) (*core.Context, error) {
	// Read file content
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Detect language
	language := e.detectLanguageFromFile(filePath)

	// Get tree-sitter language
	tsLang, err := e.GetLanguage(language)
	if err != nil {
		// Fallback to text-based extraction if tree-sitter doesn't support the language
		return e.extractTextContext(string(content), filePath, language, hunks)
	}

	// Set language and parse
	e.parser.SetLanguage(tsLang)
	tree := e.parser.Parse(content, nil)
	if tree == nil {
		return e.extractTextContext(string(content), filePath, language, hunks)
	}
	defer tree.Close()

	rootNode := tree.RootNode()

	// Extract context
	ctx := &core.Context{
		FileName: filePath,
		Language: language,
		Symbols:  make(map[string]string),
	}

	// Extract imports
	ctx.Imports = e.extractImports(rootNode, content, language)

	// Extract functions and classes related to conflicts
	for _, hunk := range hunks {
		// Find the function/class containing this hunk
		functions := e.extractFunctionsAtLine(rootNode, content, hunk.StartLine, hunk.EndLine, language)
		ctx.Functions = append(ctx.Functions, functions...)

		classes := e.extractClassesAtLine(rootNode, content, hunk.StartLine, hunk.EndLine, language)
		ctx.Classes = append(ctx.Classes, classes...)

		// Extract variables
		variables := e.extractVariablesInScope(rootNode, content, hunk.StartLine, language)
		ctx.Variables = append(ctx.Variables, variables...)
	}

	// Generate skeleton code
	ctx.Skeleton = e.generateSkeleton(rootNode, content, hunks, language)

	// Build symbol table
	e.buildSymbolTable(rootNode, content, ctx)

	return ctx, nil
}

// extractImports extracts import statements from the AST
func (e *Engine) extractImports(node *sitter.Node, content []byte, language string) []string {
	var imports []string

	queryStr := e.getImportQuery(language)
	if queryStr == "" {
		return imports
	}

	query, err := sitter.NewQuery(e.parser.Language(), queryStr)
	if err != nil {
		return imports
	}
	defer query.Close()

	cursor := sitter.NewQueryCursor()
	defer cursor.Close()

	matches := cursor.Matches(query, node, content)

	for {
		match := matches.Next()
		if match == nil {
			break
		}

		for _, capture := range match.Captures {
			start := capture.Node.StartByte()
			end := capture.Node.EndByte()
			importText := strings.TrimSpace(string(content[start:end]))
			imports = append(imports, importText)
		}
	}

	return imports
}

// extractFunctionsAtLine extracts functions that contain or are near the given lines
func (e *Engine) extractFunctionsAtLine(node *sitter.Node, content []byte, startLine, endLine int, language string) []core.FunctionContext {
	var functions []core.FunctionContext

	queryStr := e.getFunctionQuery(language)
	if queryStr == "" {
		return functions
	}

	query, err := sitter.NewQuery(e.parser.Language(), queryStr)
	if err != nil {
		return functions
	}
	defer query.Close()

	cursor := sitter.NewQueryCursor()
	defer cursor.Close()

	matches := cursor.Matches(query, node, content)

	for {
		match := matches.Next()
		if match == nil {
			break
		}

		for _, capture := range match.Captures {
			funcNode := capture.Node
			funcStart := funcNode.StartPosition().Row + 1
			funcEnd := funcNode.EndPosition().Row + 1

			// Check if function is near or contains the conflict
			if e.isNearConflict(int(funcStart), int(funcEnd), startLine, endLine) {
				funcCtx := e.buildFunctionContext(&funcNode, content)
				functions = append(functions, funcCtx)
			}
		}
	}

	return functions
}

// extractClassesAtLine extracts classes that contain or are near the given lines
func (e *Engine) extractClassesAtLine(node *sitter.Node, content []byte, startLine, endLine int, language string) []core.ClassContext {
	var classes []core.ClassContext

	queryStr := e.getClassQuery(language)
	if queryStr == "" {
		return classes
	}

	query, err := sitter.NewQuery(e.parser.Language(), queryStr)
	if err != nil {
		return classes
	}
	defer query.Close()

	cursor := sitter.NewQueryCursor()
	defer cursor.Close()

	matches := cursor.Matches(query, node, content)

	for {
		match := matches.Next()
		if match == nil {
			break
		}

		for _, capture := range match.Captures {
			classNode := capture.Node
			classStart := classNode.StartPosition().Row + 1
			classEnd := classNode.EndPosition().Row + 1

			// Check if class is near or contains the conflict
			if e.isNearConflict(int(classStart), int(classEnd), startLine, endLine) {
				classCtx := e.buildClassContext(&classNode, content)
				classes = append(classes, classCtx)
			}
		}
	}

	return classes
}

// extractVariablesInScope extracts variables in the scope of the conflict
func (e *Engine) extractVariablesInScope(node *sitter.Node, content []byte, line int, language string) []core.VariableContext {
	var variables []core.VariableContext

	// Find the scope containing the line
	scopeNode := e.findScopeAtLine(node, line)
	if scopeNode == nil {
		return variables
	}

	// Query for variable declarations
	queryStr := e.getVariableQuery(language)
	if queryStr == "" {
		return variables
	}

	query, err := sitter.NewQuery(e.parser.Language(), queryStr)
	if err != nil {
		return variables
	}
	defer query.Close()

	cursor := sitter.NewQueryCursor()
	defer cursor.Close()

	matches := cursor.Matches(query, scopeNode, content)

	for {
		match := matches.Next()
		if match == nil {
			break
		}

		for _, capture := range match.Captures {
			varNode := capture.Node
			varCtx := e.buildVariableContext(&varNode, content)
			if varCtx.Name != "" {
				variables = append(variables, varCtx)
			}
		}
	}

	return variables
}

// generateSkeleton generates skeleton code with only key structures
func (e *Engine) generateSkeleton(node *sitter.Node, content []byte, hunks []core.Hunk, language string) string {
	lines := strings.Split(string(content), "\n")
	skeletonLines := make([]string, len(lines))

	// Keep imports
	for _, imp := range e.extractImports(node, content, language) {
		// Find line number and keep it
		for i, line := range lines {
			if strings.Contains(line, imp) {
				skeletonLines[i] = line
				break
			}
		}
	}

	// Keep functions/classes near conflicts
	for _, hunk := range hunks {
		functions := e.extractFunctionsAtLine(node, content, hunk.StartLine, hunk.EndLine, language)
		for _, fn := range functions {
			// Keep function signature
			for i := fn.StartLine - 1; i < fn.EndLine && i < len(skeletonLines); i++ {
				if i == fn.StartLine-1 || i == fn.EndLine-1 {
					skeletonLines[i] = lines[i]
				} else if i > fn.StartLine && i < fn.EndLine-1 {
					skeletonLines[i] = "// ..."
				}
			}
		}

		classes := e.extractClassesAtLine(node, content, hunk.StartLine, hunk.EndLine, language)
		for _, cls := range classes {
			for i := cls.StartLine - 1; i < cls.EndLine && i < len(skeletonLines); i++ {
				if i <= cls.StartLine+2 || i == cls.EndLine-1 {
					skeletonLines[i] = lines[i]
				} else {
					skeletonLines[i] = "// ..."
				}
			}
		}
	}

	// Filter out empty lines
	var result []string
	for _, line := range skeletonLines {
		if line != "" {
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}

// buildSymbolTable builds a symbol table from the AST
func (e *Engine) buildSymbolTable(node *sitter.Node, content []byte, ctx *core.Context) {
	// Add functions
	for _, fn := range ctx.Functions {
		ctx.Symbols[fn.Name] = "function:" + fn.ReturnType
	}

	// Add classes
	for _, cls := range ctx.Classes {
		ctx.Symbols[cls.Name] = "class"
	}

	// Add variables
	for _, v := range ctx.Variables {
		ctx.Symbols[v.Name] = "variable:" + v.Type
	}
}

// Helper functions

func (e *Engine) detectLanguageFromFile(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	langMap := map[string]string{
		".go":   "go",
		".py":   "python",
		".js":   "javascript",
		".ts":   "typescript",
		".tsx":  "typescript",
		".java": "java",
		".c":    "c",
		".cpp":  "cpp",
		".rs":   "rust",
	}
	if lang, ok := langMap[ext]; ok {
		return lang
	}
	return "text"
}

func (e *Engine) isNearConflict(funcStart, funcEnd, conflictStart, conflictEnd int) bool {
	// Check if function contains conflict
	if funcStart <= conflictStart && funcEnd >= conflictEnd {
		return true
	}
	// Check if function is near conflict (within 10 lines)
	if funcEnd >= conflictStart-10 && funcStart <= conflictEnd+10 {
		return true
	}
	return false
}

func (e *Engine) findScopeAtLine(node *sitter.Node, line int) *sitter.Node {
	// Find the smallest node that contains the line
	var result *sitter.Node
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		start := int(n.StartPosition().Row) + 1
		end := int(n.EndPosition().Row) + 1
		if start <= line && end >= line {
			if result == nil || n.EndByte()-n.StartByte() < result.EndByte()-result.StartByte() {
				result = n
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(node)
	return result
}

func (e *Engine) buildFunctionContext(node *sitter.Node, content []byte) core.FunctionContext {
	start := node.StartByte()
	end := node.EndByte()

	return core.FunctionContext{
		Name:      e.extractNodeName(node, content),
		Signature: strings.TrimSpace(string(content[start:end])),
		StartLine: int(node.StartPosition().Row) + 1,
		EndLine:   int(node.EndPosition().Row) + 1,
	}
}

func (e *Engine) buildClassContext(node *sitter.Node, content []byte) core.ClassContext {
	return core.ClassContext{
		Name:      e.extractNodeName(node, content),
		StartLine: int(node.StartPosition().Row) + 1,
		EndLine:   int(node.EndPosition().Row) + 1,
	}
}

func (e *Engine) buildVariableContext(node *sitter.Node, content []byte) core.VariableContext {
	start := node.StartByte()
	end := node.EndByte()

	return core.VariableContext{
		Name:      e.extractNodeName(node, content),
		StartLine: int(node.StartPosition().Row) + 1,
		Value:     strings.TrimSpace(string(content[start:end])),
	}
}

func (e *Engine) extractNodeName(node *sitter.Node, content []byte) string {
	// Try to find a 'name' child
	for i := uint(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.Kind() == "identifier" || child.Kind() == "name" {
			start := child.StartByte()
			end := child.EndByte()
			return string(content[start:end])
		}
	}
	return ""
}

// Query strings for different languages

func (e *Engine) getImportQuery(language string) string {
	queries := map[string]string{
		"python":     `(import_statement (dotted_name) @import) (import_from_statement (dotted_name) @import)`,
		"go":         `(import_spec name: (identifier) @import) (import_spec path: (interpreted_string_literal) @import)`,
		"javascript": `(import_statement (import_specifier (identifier) @import)) (import_statement (string) @import)`,
		"typescript": `(import_statement (import_specifier (identifier) @import)) (import_statement (string) @import)`,
		"java":       `(import_declaration name: (identifier) @import) (import_declaration name: (dotted_name) @import)`,
		"cpp":        `(include_directive) @import`,
	}
	return queries[language]
}

func (e *Engine) getFunctionQuery(language string) string {
	queries := map[string]string{
		"python":     `(function_definition name: (identifier) @func)`,
		"go":         `(function_declaration name: (identifier) @func) (method_declaration name: (field_identifier) @func)`,
		"javascript": `(function_declaration name: (identifier) @func) (method_definition name: (property_identifier) @func) (arrow_function) @func`,
		"typescript": `(function_declaration name: (identifier) @func) (method_definition name: (property_identifier) @func)`,
		"java":       `(method_declaration name: (identifier) @func)`,
		"cpp":        `(function_definition name: (identifier) @func)`,
	}
	return queries[language]
}

func (e *Engine) getClassQuery(language string) string {
	queries := map[string]string{
		"python":     `(class_definition name: (identifier) @class)`,
		"go":         `(type_declaration (type_spec name: (type_identifier) @class))`,
		"javascript": `(class_declaration name: (identifier) @class)`,
		"typescript": `(class_declaration name: (type_identifier) @class) (interface_declaration name: (type_identifier) @class)`,
		"java":       `(class_declaration name: (identifier) @class) (interface_declaration name: (identifier) @class)`,
		"cpp":        `(class_specifier name: (type_identifier) @class)`,
	}
	return queries[language]
}

func (e *Engine) getVariableQuery(language string) string {
	queries := map[string]string{
		"python":     `(assignment left: (identifier) @var)`,
		"go":         `(var_spec name: (identifier) @var) (short_var_declaration left: (identifier) @var)`,
		"javascript": `(variable_declarator name: (identifier) @var)`,
		"typescript": `(variable_declarator name: (identifier) @var)`,
		"java":       `(variable_declarator name: (identifier) @var)`,
		"cpp":        `(declaration name: (identifier) @var)`,
	}
	return queries[language]
}

// extractTextContext provides a fallback text-based context extraction
func (e *Engine) extractTextContext(content, filePath, language string, hunks []core.Hunk) (*core.Context, error) {
	ctx := &core.Context{
		FileName: filePath,
		Language: language,
		Symbols:  make(map[string]string),
	}

	lines := strings.Split(content, "\n")

	// Extract imports using regex patterns
	importPatterns := map[string]*regexp.Regexp{
		"python":     regexp.MustCompile(`^(?:import|from)\s+\w+`),
		"go":         regexp.MustCompile(`^import\s*\(?|\s*(?:\w+/\w+|\$\w+)`),
		"javascript": regexp.MustCompile(`^import\s+.*from`),
		"typescript": regexp.MustCompile(`^import\s+.*from`),
		"java":       regexp.MustCompile(`^import\s+`),
		"cpp":        regexp.MustCompile(`^#include\s+`),
	}

	if pattern, ok := importPatterns[language]; ok {
		for _, line := range lines {
			if pattern.MatchString(line) {
				ctx.Imports = append(ctx.Imports, strings.TrimSpace(line))
			}
		}
	}

	// Extract function signatures near conflicts
	funcPatterns := map[string]*regexp.Regexp{
		"python":     regexp.MustCompile(`^def\s+\w+\(`),
		"go":         regexp.MustCompile(`^func\s+(\w+\.)?\w+\(`),
		"javascript": regexp.MustCompile(`^(function\s+\w+\(|const\s+\w+\s*=\s*(?:async\s*)?\(|class\s+\w+)`),
		"typescript": regexp.MustCompile(`^(function\s+\w+\(|const\s+\w+\s*:\s*(?:\w+\()?)|class\s+\w+`),
		"java":       regexp.MustCompile(`^(public|private|protected)?\s*(static)?\s*\w+\s+\w+\(`),
		"cpp":        regexp.MustCompile(`^\w+(?:\s*::\s*\w+)?\s+\w+\s*\(`),
	}

	for _, hunk := range hunks {
		start := hunk.StartLine - 20
		if start < 0 {
			start = 0
		}
		end := hunk.EndLine + 20
		if end > len(lines) {
			end = len(lines)
		}

		if pattern, ok := funcPatterns[language]; ok {
			for i := start; i < end; i++ {
				if pattern.MatchString(lines[i]) {
					fn := core.FunctionContext{
						Name:      e.extractFuncName(lines[i]),
						Signature: strings.TrimSpace(lines[i]),
						StartLine: i + 1,
						EndLine:   i + 1,
					}
					ctx.Functions = append(ctx.Functions, fn)
				}
			}
		}
	}

	return ctx, nil
}

func (e *Engine) extractFuncName(line string) string {
	// Simple regex to extract function name
	re := regexp.MustCompile(`(?:def|func|function|class)\s+(\w+)`)
	matches := re.FindStringSubmatch(line)
	if len(matches) > 1 {
		return matches[1]
	}
	return "unknown"
}
