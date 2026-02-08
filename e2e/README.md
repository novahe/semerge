# E2E Tests

End-to-end tests for semerge using a corpus-based approach.

## Directory Structure

```text
e2e/
├── runner/                # Test executor (Go test code)
│   ├── .semerge.yaml      # Default semerge config for E2E
│   └── e2e_test.go        # Main test file
├── corpus/                # Test corpus (Git repository)
│   └── go/
│       └── <case>/
│           ├── base/                  # Base code files
│           ├── patches/               # Conflict-generating patches
│           │   ├── remote-*.patch
│           │   └── local-*.patch
│           ├── .semerge.yaml          # Optional case-level config (higher priority)
│           ├── .expect_resolve_failure# Optional: mark case expects resolve failure
│           └── expect/                # Optional: expected resolved output
├── go.mod
└── README.md
```

## How It Works

1. **Test discovery**: Runner discovers cases in `corpus/<lang>/`.
2. **Repository cloning**: Corpus repo is cloned into a temp directory.
3. **Patch application**: Applies `remote-*.patch` and `local-*.patch` on separate branches.
4. **Conflict creation**: Merges `remote` into `local` to create conflict files.
5. **Resolution**: Calls `semerge resolve` per conflicted file.
6. **Verification strategy**:
   - If `expect/` exists for conflicted files: compare text first.
   - If text mismatches: run syntax/behavior verification fallback.
   - If no `expect/` exists: directly run syntax/behavior verification.
7. **Expected-failure cases**:
   - If `.expect_resolve_failure` exists, runner expects `semerge resolve` to fail and validates failure logs.

## Adding New Test Cases

`e2e/corpus` is maintained as a separate Git repository (sub-repo).
When adding or updating corpus cases, commit changes inside `e2e/corpus` first.

Create a case under `corpus/<lang>/<test_name>/`:

```text
corpus/
└── <lang>/
    └── <test_name>/
        ├── base/
        ├── patches/
        │   ├── remote-<change>.patch
        │   └── local-<change>.patch
        ├── .semerge.yaml            # optional
        ├── .expect_resolve_failure  # optional
        └── expect/                  # optional
```

### Notes

- `base/`: Original source files before changes.
- `base/*_test.go` (recommended): Unit tests used by E2E behavior verification.
- `base/verify.sh` (optional, Bash): custom behavior assertions executed after merge.
- `patches/`: Patch files prefixed by branch name.
- `expect/` (optional): Golden outputs when strict text comparison is needed.
- No `expect/`: Case is validated by syntax/behavior checks only.
- Bash behavior verification: runs `bash -n` on shell files; if `verify.sh` exists, executes it for functional assertions.
- `.semerge.yaml` (optional): Overrides runner default config for this case.
- `.expect_resolve_failure` (optional): Marks negative cases where resolve should fail.
- Corpus contribution workflow:
  1. Add/update files under `e2e/corpus/<lang>/<case>/`.
  2. Commit in sub-repo: `git -C e2e/corpus add ... && git -C e2e/corpus commit -m "..."`.
  3. In parent repo, commit the sub-repo pointer update.

## Running Tests

```bash
# Run all
make e2e-test

# Run language subset
make e2e-test TEST=go

# Run single case
make e2e-test TEST=go/retry_verifier_once

# Debug mode (verbose logs + keep temp repos)
DEBUG=1 make e2e-test TEST=go/retry_verifier_once
```

## Implementation Details

- Uses `go-git/v5` for Git operations.
- Runs tests in parallel with `t.Parallel()`.
- Each case runs in an isolated cloned repo.
- Conflict detection is based on Git index stage entries (1/2/3).
- Supports case-level semerge config override.
- Default runner config enables verifier with Go syntax-level check: `gofmt -e {file}`.
- Supports positive and expected-failure E2E scenarios.
