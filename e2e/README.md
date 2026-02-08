# E2E Tests

End-to-end tests for semerge using a corpus-based approach.

## Directory Structure

```
e2e/
├── runner/           # Test executor (Go test code)
│   └── e2e_test.go  # Main test file
├── corpus/          # Test corpus (Git repository)
│   ├── .git/        # Git repository
│   └── go/          # Language-specific test cases
│       └── rename_struct/
│           ├── base/           # Base code files
│           │   └── main.go
│           ├── patches/        # Patch files that create conflicts
│           │   ├── remote-add-field.patch
│           │   └── local-refactor.patch
│           └── expect/         # Expected resolved output
│               └── main.go
├── go.mod
└── README.md
```

## How It Works

1. **Test Discovery**: The runner automatically discovers all test cases in `corpus/*/`

2. **Repository Cloning**: For each test, the corpus Git repository is cloned to a temporary directory

3. **Branch Creation**: Two branches are created to simulate conflicting changes:
   - `remote` branch: Applies `remote-*.patch`
   - `local` branch: Applies `local-*.patch`

4. **Merge Conflict**: The `remote` branch is merged into `local`, creating merge conflicts

5. **Resolution**: Semerge is used to resolve the conflicts

6. **Verification**: The resolved output is compared against the expected output in `expect/`

## Adding New Test Cases

Create a new test case directory under `corpus/<lang>/`:

```
corpus/
└── <lang>/
    └── <test_name>/
        ├── base/           # Starting code state
        │   └── main.go
        ├── patches/        # Patches that create conflict
        │   ├── remote-<change>.patch
        │   └── local-<change>.patch
        └── expect/         # Expected result after merge
            └── main.go
```

### Test Case Structure

- **base/**: Contains the original source files before any changes
- **patches/**: Contains patch files prefixed by branch name
  - `remote-*.patch`: Applied to `remote` branch
  - `local-*.patch`: Applied to `local` branch
  - When merged, these patches should create conflicts
- **expect/**: Contains the expected resolved files

### Creating Patches

Use `git diff` to generate patches:

```bash
# Initialize a test git repo
cd /tmp/test && git init
git add base/main.go
git commit -m "base"

# Create remote branch change
git checkout -b remote
# ... edit file ...
git diff master > remote-change.patch

# Create local branch change
git checkout master
git checkout -b local
# ... edit file differently ...
git diff master > local-change.patch
```

## Running Tests

```bash
# Run all tests
make e2e-test

# Run specific language tests
make e2e-test TEST=go

# Run specific test case
make e2e-test TEST=go/rename_struct

# Run with debug logging (shows LLM prompts/responses)
DEBUG=1 make e2e-test TEST=go/rename_struct
```

## Debug Mode

Setting `DEBUG=1` enables verbose logging:
- Shows created temporary directories (not cleaned up)
- Displays ancestor/current/incoming file contents
- Shows full LLM prompts and responses
- Preserves test artifacts for inspection

## Implementation Details

- **Go-git Library**: All Git operations use the `go-git/v5` library
- **Parallel Execution**: Tests run in parallel using `t.Parallel()`
- **Isolation**: Each test runs in a cloned repository to ensure isolation
- **Conflict Detection**: Uses Git index stages (1=ancestor, 2=current, 3=incoming)
- **Branch-based Patches**: Patch files are named with branch prefix (`remote-*` or `local-*`)
