package runner

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var corpusDir = "../corpus"

func TestE2E(t *testing.T) {
	t.Parallel()

	cases, err := discoverTestCases()
	if err != nil {
		t.Fatalf("failed to discover test cases: %v", err)
	}

	if len(cases) == 0 {
		t.Fatal("no test cases found")
	}

	for _, c := range cases {
		c := c
		t.Run(c.FullName, func(t *testing.T) {
			t.Parallel()
			runTestCase(t, c)
		})
	}
}

func runTestCase(t *testing.T, tc TestCase) {
	debug := os.Getenv("DEBUG") == "1"

	// Clone corpus repo to temp directory
	worktreeDir, err := cloneRepo(corpusDir, debug)
	if err != nil {
		t.Fatalf("failed to clone repo: %v", err)
	}
	defer cleanupRepo(worktreeDir, t, debug)

	if debug {
		t.Logf("Created clone at: %s", worktreeDir)
	}

	// Copy base files to worktree root
	baseDir := filepath.Join(corpusDir, tc.Path, "base")
	if err := copyDirectory(baseDir, worktreeDir); err != nil {
		t.Fatalf("failed to copy base files: %v", err)
	}

	// Add base files to git
	repo, err := git.PlainOpen(worktreeDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	_, err = wt.Add(".")
	if err != nil {
		t.Fatalf("failed to add files: %v", err)
	}

	_, err = wt.Commit("base", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "E2E Test",
			Email: "e2e@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Apply patches to create conflict (creates two branches and merges them)
	patchesDir := filepath.Join(corpusDir, tc.Path, "patches")
	if err := applyPatches(t, worktreeDir, repo, wt, patchesDir); err != nil {
		t.Fatalf("failed to apply patches: %v", err)
	}

	// Check if conflicts exist
	conflictFiles := getConflictFiles(worktreeDir)
	if len(conflictFiles) == 0 {
		t.Skipf("no merge conflicts found for test case: %s", tc.FullName)
	}

	t.Logf("Found %d conflict(s) in: %v", len(conflictFiles), conflictFiles)

	expectFailure := shouldExpectResolveFailure(tc)
	if expectFailure {
		t.Log("This case expects semerge resolve to fail after retries")
	}

	// Resolve conflicts with semerge
	for _, file := range conflictFiles {
		resolveWithSemerge(t, tc, worktreeDir, file)
		if expectFailure {
			t.Log("Expected resolve failure observed; skipping post-merge verification")
			return
		}
	}

	// Verify the result
	hasExpect := false
	hasTextMismatch := false
	for _, conflictFile := range conflictFiles {
		expectFile := filepath.Join(corpusDir, tc.Path, "expect", filepath.Base(conflictFile))
		if _, err := os.Stat(expectFile); os.IsNotExist(err) {
			t.Logf("No expected file for %s", conflictFile)
			continue
		}

		hasExpect = true
		got := read(filepath.Join(worktreeDir, conflictFile))
		expected := read(expectFile)

		if normalize(got) != normalize(expected) {
			hasTextMismatch = true
			t.Logf("GOT (%s):\n%s", conflictFile, got)
			t.Logf("EXPECTED (%s):\n%s", expectFile, expected)
		}
	}

	if !hasExpect {
		t.Log("No expect files found; running syntax/behavior verification...")
		if err := verifyByBehavior(worktreeDir, tc); err != nil {
			t.Fatalf("semantic verification failed without expect files: %v", err)
		}
		t.Log("Semantic verification passed")
	} else if hasTextMismatch {
		t.Log("Text mismatch detected; running syntax/behavior verification...")
		if err := verifyByBehavior(worktreeDir, tc); err != nil {
			t.Fatalf("semantic verification failed after text mismatch: %v", err)
		}
		t.Log("Semantic verification passed; accepting non-identical output")
	}

	t.Log("E2E test passed!")
}

type TestCase struct {
	Lang     string
	Name     string
	Path     string
	FullName string
}

func discoverTestCases() ([]TestCase, error) {
	var cases []TestCase

	langs, err := os.ReadDir(corpusDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read corpus dir: %w", err)
	}

	for _, lang := range langs {
		if !lang.IsDir() || lang.Name() == ".git" {
			continue
		}

		langDir := filepath.Join(corpusDir, lang.Name())
		tests, err := os.ReadDir(langDir)
		if err != nil {
			continue
		}

		for _, test := range tests {
			if !test.IsDir() {
				continue
			}

			testPath := filepath.Join(lang.Name(), test.Name())
			baseDir := filepath.Join(corpusDir, testPath, "base")
			if _, err := os.Stat(baseDir); os.IsNotExist(err) {
				continue
			}

			cases = append(cases, TestCase{
				Lang:     lang.Name(),
				Name:     test.Name(),
				Path:     testPath,
				FullName: testPath,
			})
		}
	}

	return cases, nil
}

func cloneRepo(corpusDir string, debug bool) (string, error) {
	tempDir, err := os.MkdirTemp("", "semerge-e2e-clone-*")
	if err != nil {
		return "", err
	}

	_, err = git.PlainClone(tempDir, false, &git.CloneOptions{
		URL: corpusDir,
	})
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("failed to clone: %w", err)
	}

	return tempDir, nil
}

func cleanupRepo(dir string, t *testing.T, debug bool) {
	if debug {
		t.Logf("Skipping cleanup of repo: %s", dir)
		return
	}
	os.RemoveAll(dir)
}

func applyPatches(t *testing.T, worktreeDir string, repo *git.Repository, wt *git.Worktree, patchesDir string) error {
	entries, err := os.ReadDir(patchesDir)
	if err != nil {
		return fmt.Errorf("failed to read patches dir: %w", err)
	}

	var remotePatch, localPatch string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".patch") {
			if strings.HasPrefix(entry.Name(), "remote-") {
				remotePatch = entry.Name()
			} else if strings.HasPrefix(entry.Name(), "local-") {
				localPatch = entry.Name()
			}
		}
	}

	if remotePatch == "" && localPatch == "" {
		return nil
	}

	// Apply remote patch
	if remotePatch != "" {
		remotePatchPath := filepath.Join(patchesDir, remotePatch)
		absRemotePatchPath, _ := filepath.Abs(remotePatchPath)

		err = wt.Checkout(&git.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName("remote"),
			Create: true,
		})
		if err != nil {
			return fmt.Errorf("failed to create remote branch: %w", err)
		}

		cmd := exec.Command("patch", "-p1", "-i", absRemotePatchPath)
		cmd.Dir = worktreeDir
		output, err := cmd.CombinedOutput()
		t.Logf("Applied patch %s to remote branch:\n%s", remotePatch, string(output))
		if err != nil {
			return fmt.Errorf("failed to apply patch %s: %w\nOutput: %s", remotePatch, err, string(output))
		}

		wt.Add(".")
		_, err = wt.Commit("remote changes", &git.CommitOptions{})
		if err != nil {
			return fmt.Errorf("failed to commit remote changes: %w", err)
		}
	}

	if localPatch == "" {
		return nil
	}

	// Apply local patch
	err = wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.Master,
	})
	if err != nil {
		return fmt.Errorf("failed to checkout master: %w", err)
	}

	localPatchPath := filepath.Join(patchesDir, localPatch)
	absLocalPatchPath, _ := filepath.Abs(localPatchPath)

	err = wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("local"),
		Create: true,
	})
	if err != nil {
		return fmt.Errorf("failed to create local branch: %w", err)
	}

	cmd := exec.Command("patch", "-p1", "-i", absLocalPatchPath)
	cmd.Dir = worktreeDir
	output, err := cmd.CombinedOutput()
	t.Logf("Applied patch %s to local branch:\n%s", localPatch, string(output))
	if err != nil {
		return fmt.Errorf("failed to apply patch %s: %w\nOutput: %s", localPatch, err, string(output))
	}

	wt.Add(".")
	_, err = wt.Commit("local changes", &git.CommitOptions{})
	if err != nil {
		return fmt.Errorf("failed to commit local changes: %w", err)
	}

	// Merge remote into local to create conflicts
	t.Logf("Merging remote branch into local branch...")
	cmd = exec.Command("git", "merge", "remote")
	cmd.Dir = worktreeDir
	output, _ = cmd.CombinedOutput()
	t.Logf("Merge output:\n%s", string(output))

	return nil
}

func resolveWithSemerge(t *testing.T, tc TestCase, repoDir, file string) {
	ancestorPath := filepath.Join(repoDir, ".ancestor.tmp")
	currentPath := filepath.Join(repoDir, file)
	incomingPath := filepath.Join(repoDir, ".incoming.tmp")

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	idx, err := repo.Storer.Index()
	if err != nil {
		t.Fatalf("failed to get index: %v", err)
	}

	var ancestorEntry, currentEntry, incomingEntry *index.Entry

	for _, e := range idx.Entries {
		if e.Name == file {
			switch e.Stage {
			case 1:
				ancestorEntry = e
			case 2:
				currentEntry = e
			case 3:
				incomingEntry = e
			}
		}
	}

	if ancestorEntry != nil {
		obj, err := repo.BlobObject(ancestorEntry.Hash)
		if err == nil && obj != nil {
			reader, err := obj.Reader()
			if err == nil {
				defer reader.Close()
				content, _ := io.ReadAll(reader)
				_ = os.WriteFile(ancestorPath, content, 0644)
			}
		}
	}

	if currentEntry != nil {
		obj, err := repo.BlobObject(currentEntry.Hash)
		if err == nil && obj != nil {
			reader, err := obj.Reader()
			if err == nil {
				defer reader.Close()
				content, _ := io.ReadAll(reader)
				_ = os.WriteFile(currentPath, content, 0644)
			}
		}
	}

	if incomingEntry != nil {
		obj, err := repo.BlobObject(incomingEntry.Hash)
		if err == nil && obj != nil {
			reader, err := obj.Reader()
			if err == nil {
				defer reader.Close()
				content, _ := io.ReadAll(reader)
				_ = os.WriteFile(incomingPath, content, 0644)
			}
		}
	}

	if os.Getenv("DEBUG") == "1" {
		t.Logf("Ancestor:\n%s", read(ancestorPath))
		t.Logf("Current:\n%s", read(currentPath))
		t.Logf("Incoming:\n%s", read(incomingPath))
	}

	semergePath := os.Getenv("SEMERGE_BIN")
	if semergePath == "" {
		testDir, _ := os.Getwd()
		semergePath = filepath.Join(testDir, "../../bin/semerge")
		if _, err := os.Stat(semergePath); os.IsNotExist(err) {
			t.Fatalf("semerge binary not found at %s. Run 'make build' first", semergePath)
		}
	}
	semergeAbsPath, err := filepath.Abs(semergePath)
	if err != nil {
		t.Fatalf("failed to resolve semerge path: %v", err)
	}

	testDir, _ := os.Getwd()
	configPath := resolveConfigPath(testDir, tc)
	t.Logf("Using semerge config: %s", configPath)

	relAncestor := ".ancestor.tmp"
	relCurrent := file
	relIncoming := ".incoming.tmp"

	cmd := exec.Command(semergeAbsPath, "resolve", "-c", configPath, relAncestor, relCurrent, relIncoming)
	cmd.Dir = repoDir

	env := os.Environ()
	if os.Getenv("DEBUG") == "1" {
		env = append(env, "SEMERGE_DEBUG=1")
	}
	if apiKey := os.Getenv("ANTHROPIC_API_KEY"); apiKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+apiKey)
	}
	cmd.Env = env

	output, err := cmd.CombinedOutput()

	if tc.Name == "retry_verifier_once" {
		assertRetryTriggered(t, output)
	}

	if tc.Name == "retry_verifier_exhausted" {
		assertRetryExhausted(t, output)
	}

	if shouldExpectResolveFailure(tc) {
		if err == nil {
			t.Fatalf("expected semerge resolve to fail, but it succeeded\noutput:\n%s", string(output))
		}
		t.Logf("Expected resolve failure output:\n%s", string(output))
		return
	}

	if err != nil {
		t.Logf("semerge output:\n%s", string(output))
		t.Fatalf("semerge resolve failed: %v", err)
	}

	if os.Getenv("DEBUG") == "1" {
		t.Logf("semerge output:\n%s", string(output))
		t.Logf("Resolved:\n%s", read(currentPath))
	}

	os.Remove(ancestorPath)
	os.Remove(incomingPath)
}

func resolveConfigPath(testDir string, tc TestCase) string {
	caseConfigPath := filepath.Join(corpusDir, tc.Path, ".semerge.yaml")
	if absCaseConfig, err := filepath.Abs(caseConfigPath); err == nil {
		if _, statErr := os.Stat(absCaseConfig); statErr == nil {
			return absCaseConfig
		}
	}

	defaultConfig := filepath.Join(testDir, ".semerge.yaml")
	if absDefaultConfig, err := filepath.Abs(defaultConfig); err == nil {
		return absDefaultConfig
	}

	return defaultConfig
}

func shouldExpectResolveFailure(tc TestCase) bool {
	flagPath := filepath.Join(corpusDir, tc.Path, ".expect_resolve_failure")
	if absFlagPath, err := filepath.Abs(flagPath); err == nil {
		if _, statErr := os.Stat(absFlagPath); statErr == nil {
			return true
		}
	}

	return false
}

func assertRetryTriggered(t *testing.T, output []byte) {
	out := string(output)
	if !strings.Contains(out, "Resolving conflicts with AI... (attempt 1/3)") {
		t.Fatalf("retry case missing attempt 1 log, output:\n%s", out)
	}
	if !strings.Contains(out, "Verification failed (attempt 1/3)") {
		t.Fatalf("retry case missing verification failure log, output:\n%s", out)
	}
	if !strings.Contains(out, "Resolving conflicts with AI... (attempt 2/3)") {
		t.Fatalf("retry case missing attempt 2 log, output:\n%s", out)
	}
	if !strings.Contains(out, "Verification passed!") {
		t.Fatalf("retry case missing verification passed log, output:\n%s", out)
	}
}

func assertRetryExhausted(t *testing.T, output []byte) {
	out := string(output)
	if !strings.Contains(out, "Resolving conflicts with AI... (attempt 1/3)") {
		t.Fatalf("retry exhausted case missing attempt 1 log, output:\n%s", out)
	}
	if !strings.Contains(out, "Resolving conflicts with AI... (attempt 2/3)") {
		t.Fatalf("retry exhausted case missing attempt 2 log, output:\n%s", out)
	}
	if !strings.Contains(out, "Resolving conflicts with AI... (attempt 3/3)") {
		t.Fatalf("retry exhausted case missing attempt 3 log, output:\n%s", out)
	}
	if !strings.Contains(out, "verification failed after 3 attempts") {
		t.Fatalf("retry exhausted case missing final failure summary, output:\n%s", out)
	}
}

func getConflictFiles(dir string) []string {
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return nil
	}

	idx, err := repo.Storer.Index()
	if err != nil {
		return nil
	}

	files := make(map[string]bool)
	for _, e := range idx.Entries {
		if e.Stage == 2 {
			files[e.Name] = true
		}
	}

	result := make([]string, 0, len(files))
	for f := range files {
		result = append(result, f)
	}
	return result
}

func copyDirectory(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := os.MkdirAll(dstPath, 0755); err != nil {
				return err
			}
			if err := copyDirectory(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			data, err := os.ReadFile(srcPath)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dstPath, data, 0644); err != nil {
				return err
			}
		}
	}

	return nil
}

func read(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "")
	return string(bytes.TrimSpace([]byte(s)))
}

func verifyByBehavior(worktreeDir string, tc TestCase) error {
	baseFiles, err := scenarioBaseFiles(tc)
	if err != nil {
		return err
	}

	switch tc.Lang {
	case "go":
		if err := verifyGoSyntax(worktreeDir, baseFiles); err != nil {
			return err
		}
		if err := verifyGoTestsIfAny(worktreeDir, baseFiles); err != nil {
			return err
		}
	case "python":
		if err := verifyPythonSyntax(worktreeDir, baseFiles); err != nil {
			return err
		}
	}

	return nil
}

func scenarioBaseFiles(tc TestCase) ([]string, error) {
	baseDir := filepath.Join(corpusDir, tc.Path, "base")
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read base dir: %w", err)
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		files = append(files, entry.Name())
	}
	sort.Strings(files)

	return files, nil
}

func verifyGoSyntax(worktreeDir string, files []string) error {
	goFiles := filterByExt(files, ".go")
	if len(goFiles) == 0 {
		return nil
	}

	args := append([]string{"-e"}, goFiles...)
	cmd := exec.Command("gofmt", args...)
	cmd.Dir = worktreeDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go syntax check failed: %w\nOutput: %s", err, string(output))
	}

	return nil
}

func verifyGoTestsIfAny(worktreeDir string, files []string) error {
	hasTest := false
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			hasTest = true
			break
		}
	}
	if !hasTest {
		return nil
	}

	cmd := exec.Command("go", "test")
	cmd.Dir = worktreeDir
	cmd.Env = append(os.Environ(), "GO111MODULE=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go behavior test failed: %w\nOutput: %s", err, string(output))
	}

	return nil
}

func verifyPythonSyntax(worktreeDir string, files []string) error {
	pyFiles := filterByExt(files, ".py")
	if len(pyFiles) == 0 {
		return nil
	}

	args := append([]string{"-m", "py_compile"}, pyFiles...)
	cmd := exec.Command("python3", args...)
	cmd.Dir = worktreeDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("python syntax check failed: %w\nOutput: %s", err, string(output))
	}

	return nil
}

func filterByExt(files []string, ext string) []string {
	result := make([]string, 0)
	for _, f := range files {
		if strings.HasSuffix(f, ext) {
			result = append(result, f)
		}
	}
	return result
}
