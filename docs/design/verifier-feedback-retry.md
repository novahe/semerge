# Verifier-Feedback-Driven Conflict Retry Design

## Background
In real merge-conflict scenarios, an LLM-generated resolution may be semantically correct but fail to compile, or compile successfully but still fail static checks. Relying only on "first-pass generation + text diff" leads to:

- Low stability (minor output format changes can fail the check)
- Misaligned success criteria (the real goal is runnable/verifiable code)
- Poor recoverability after failure (the model does not know what to fix)

To address this, Verifier results should be structured and fed back to the LLM, forming a closed loop:
"generate -> verify -> retry with corrective feedback".

## Goals
- Support file-level retries up to 3 attempts.
- Feed key Verifier errors back to the LLM after each failed attempt.
- Fail strictly when retries are exhausted (do not write unreliable output).
- Reuse the feedback structure in both E2E and real runtime (git merge driver) scenarios.

## Non-Goals
- This design does not introduce an automatic AST rewrite fixer.
- This design does not change the current OpenAI provider implementation state.
- This design does not require exact textual equality; it targets verifiable correctness.

## Solution Overview

### 1) Structured Feedback Model
Add `core.VerifierFeedback`:

- `Attempt`
- `Tool`
- `ExitCode`
- `TopErrors`
- `TopWarnings`
- `PreviousCandidate`

This structure bridges runtime verification and prompt construction without binding to a specific language.

### 2) Resolve Retry Orchestration (File-Level)
`runResolve` main flow:

1. Extract conflicts and context.
2. Call LLM for each conflict and produce resolved blocks.
3. Reconstruct full `mergedContent`.
4. Run Verifier.
5. If pass: write file and return.
6. If fail: build `VerifierFeedback`, format it, and inject into next prompt.
7. If still failing at max attempts: return error and terminate (strict mode).

### 3) Prompt Feedback Injection Strategy
When `attempt > 1`, append to prompt:

- Previous attempt verification summary (tool / exit code / top errors / top warnings)
- Previous failed candidate content
- Explicit instruction: must fix verification failures before returning resolution

This shifts the model from blind retry to targeted repair.

## Key Data Flow

```text
LLM(Attempt=1) -> mergedContent -> Verifier
         | pass                    | fail
         v                         v
      write file         build VerifierFeedback
                                  |
                                  v
          formatVerifierFeedbackForPrompt + previous candidate
                                  |
                                  v
                       LLM(Attempt=2/3)
```

## Abstraction Boundaries

- `buildVerifierFeedback`: `VerificationResult -> VerifierFeedback`
- `formatVerifierFeedbackForPrompt`: `VerifierFeedback -> prompt text + previous candidate`
- `verifyMergedContent`: unified verifier invocation entry (easy future replacement/extension)

This split ensures:

- Runtime orchestration and prompt formatting can evolve independently.
- Multi-stage verifier combinations (`syntax/test/lint`) can be added later.

## Failure Policy
- Do not write file immediately after a failed verification.
- Return error if max retries are exhausted.
- Include structured feedback summary in the final error for diagnostics and logging.

## Value in E2E and Real Runtime

### E2E
- Accepts non-identical text if syntax/behavior passes, reducing fragility.
- Allows observing whether retry feedback leads to convergence.

### Real Runtime (merge driver)
- A single failure does not immediately fall back to manual resolution.
- Feedback can be exposed in logs, improving explainability.
- Can evolve into a staged strategy: "staged verification + staged retries".

## Future Enhancements
- Add `Kind` (`syntax` / `test` / `lint`) and `Stage` to `VerifierFeedback`.
- Support layered verifier pipelines (e.g., syntax first, then tests).
- Add language-specific Prepare hooks (e.g., Go `go mod init`, Python venv).
- Add runtime metrics: retry count, success rate, and failure-type distribution.

## Acceptance Criteria
- `runResolve` supports up to 3 file-level retries.
- Verification failures are fed back into subsequent prompts.
- Exceeding retry limit returns error and does not write target file.
- Related unit tests and key E2E scenarios pass.
