# Semerge

AI-powered git merge conflict resolution tool using Large Language Models to understand code semantics and resolve conflicts intelligently.

## Features

- **Semantic Understanding**: Uses LLMs to analyze code semantics rather than simple text merging
- **Multi-Language Support**: Works with Go, Python, JavaScript, TypeScript, Java, C/C++, and more
- **AST-Based Context**: Leverages Tree-sitter for accurate code context extraction
- **Flexible LLM Providers**: Supports Claude, OpenAI, or custom API endpoints
- **Optional Verification**: Integrates with language-specific linters for code quality checks
- **Git Integration**: Can be installed as a git merge driver for automatic conflict resolution

## Installation

### From Source

```bash
git clone https://github.com/novahe/semerge.git
cd semerge
make build
```

The binary will be created at `./bin/semerge`. You can move it to your PATH:

```bash
sudo mv bin/semerge /usr/local/bin/
```

## Quick Start

### 1. Initialize Configuration

```bash
semerge init
```

This creates a configuration file at `~/.semerge.yaml` (or `.semerge.yaml` in your project).

Edit the configuration to set your API key:

```yaml
llm:
  provider: claude              # or "openai"
  api_key: sk-ant-xxx          # Your API key
  model: claude-sonnet-4-5-20250514
  timeout: 60
```

### 2. Verify Installation

```bash
semerge health
```

### 3. Resolve Conflicts

**Manual resolution** (three files):

```bash
semerge resolve ancestor.txt current.txt incoming.txt
```

**As git merge driver** (automatic):

```bash
# Install as git merge driver
semerge install

# Configure in .gitattributes
echo "*.go merge=semerge" >> .gitattributes
echo "*.py merge=semerge" >> .gitattributes
```

Now when you run `git merge` and conflicts occur, semerge will automatically resolve them!

## How It Works

1. **Conflict Extraction**: Uses `git merge-file` to generate conflict markers
2. **Context Analysis**: Parses AST with Tree-sitter to extract imports, functions, classes
3. **Semantic Resolution**: Sends conflicts and context to LLM for intelligent merging
4. **Verification**: Optionally validates resolved code with linters (gofmt, pylint, etc.)
5. **Result Merging**: Writes resolved content back to the working directory

## Architecture

```
cmd/semerge/          # CLI application
  ├── resolve         # Resolve conflicts in files
  ├── init            # Initialize configuration
  ├── health          # Check configuration and LLM connectivity
  └── install         # Install as git merge driver

pkg/
  ├── core/           # Core data types (Hunk, Context, MergeResult)
  ├── hunk/           # Conflict extraction from git merge-file
  ├── llm/            # LLM client (Claude, OpenAI)
  ├── context/        # Tree-sitter AST-based context extraction
  └── verifier/       # Code verification with linters

e2e/                  # End-to-end tests with corpus-based approach
```

## Configuration

Example `~/.semerge.yaml`:

```yaml
llm:
  provider: claude              # "claude" or "openai"
  api_key: sk-ant-xxx          # Or set ANTHROPIC_API_KEY env var
  model: claude-3-5-sonnet-20241022
  timeout: 60
  base_url:                     # Optional: custom API endpoint

verifier:
  enabled: false                # Run linters after resolution
  tools: {}                     # language -> command mapping
  retries: 1

context:
  use_tree_sitter: true         # Use AST parsing for context
  max_lines: 50                 # Max context lines to extract
  include_imports: true         # Include import statements

git:
  merge_strategy: semantic      # "auto", "semantic", or "text"
```

## Testing

### Unit Tests

```bash
make test
```

### E2E Tests

```bash
# Run all E2E tests
make e2e

# Run specific language tests
make e2e-test TEST=go

# Run specific test case
make e2e-test TEST=go/rename_struct

# Run with debug logging
DEBUG=1 make e2e-test TEST=go/rename_struct
```

## Cherry-Pick Conflicts

Semerge can also resolve cherry-pick conflicts! The semantic understanding works the same way:

```bash
# When cherry-picking causes conflicts
git cherry-pick <commit-hash>

# Resolve with semerge
semerge resolve .ancestor.tmp . conflicted-file.txt .incoming.tmp

git add conflicted-file.txt
git cherry-pick --continue
```

## Limitations

- **Binary files**: Only works with text-based source files
- **Large conflicts**: Very large conflict regions may hit LLM token limits
- **Complex conflicts**: Some conflicts may require manual review and adjustment

## Contributing

Contributions are welcome! Please see `e2e/README.md` for test case contribution guidelines.

## License

MIT License - see LICENSE file for details

## Acknowledgments

Built with:
- [go-git](https://github.com/go-git/go-git) - Git operations in Go
- [Tree-sitter](https://tree-sitter.github.io/tree-sitter/) - AST parsing
- [Cobra](https://github.com/spf13/cobra) - CLI framework
- [Anthropic Claude](https://www.anthropic.com/) - LLM provider
