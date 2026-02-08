package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semerge/semerge/pkg/core"
)

func TestResolveImportPath_Go(t *testing.T) {
	workDir := t.TempDir()
	packageDir := filepath.Join(workDir, "pkg", "util")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatalf("failed to create package dir: %v", err)
	}

	goFile := filepath.Join(packageDir, "util.go")
	content := "package util\n\nfunc Exported() {}\n"
	if err := os.WriteFile(goFile, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write go file: %v", err)
	}

	engine := NewEngine()
	testCases := []struct {
		name       string
		importStmt string
		wantPath   string
	}{
		{
			name:       "quoted import path",
			importStmt: `"pkg/util"`,
			wantPath:   goFile,
		},
		{
			name:       "plain import path",
			importStmt: "pkg/util",
			wantPath:   goFile,
		},
		{
			name:       "missing import path",
			importStmt: `"pkg/not_found"`,
			wantPath:   "",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resolved := engine.resolveImportPath(workDir, tc.importStmt, "go")
			if resolved != tc.wantPath {
				t.Fatalf("resolveImportPath(%q) = %q, want %q", tc.importStmt, resolved, tc.wantPath)
			}
		})
	}
}

func TestEnrichContextWithRelatedFiles_AppendsExternalSignatures(t *testing.T) {
	workDir := t.TempDir()
	packageDir := filepath.Join(workDir, "pkg", "util")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatalf("failed to create package dir: %v", err)
	}

	goFile := filepath.Join(packageDir, "util.go")
	content := strings.Join([]string{
		"package util",
		"",
		"func Exported() {}",
		"func hidden() {}",
	}, "\n")
	if err := os.WriteFile(goFile, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write go file: %v", err)
	}

	ctx := &core.Context{
		Language: "go",
		Imports:  []string{`"pkg/util"`},
		Skeleton: "func main() {}",
	}

	engine := NewEngine()
	engine.EnrichContextWithRelatedFiles(ctx, workDir)

	if !strings.Contains(ctx.Skeleton, "External Context") {
		t.Fatalf("expected external context marker, got:\n%s", ctx.Skeleton)
	}
	if !strings.Contains(ctx.Skeleton, "func Exported() {}") {
		t.Fatalf("expected exported signature, got:\n%s", ctx.Skeleton)
	}
	if strings.Contains(ctx.Skeleton, "hidden") {
		t.Fatalf("expected private signature filtered out, got:\n%s", ctx.Skeleton)
	}
}

func TestGetLanguage_Bash(t *testing.T) {
	engine := NewEngine()

	if _, err := engine.GetLanguage("bash"); err != nil {
		t.Fatalf("GetLanguage(bash) returned error: %v", err)
	}

	if _, err := engine.GetLanguage("sh"); err != nil {
		t.Fatalf("GetLanguage(sh) returned error: %v", err)
	}
}

func TestDetectLanguageFromFile_BashExtensions(t *testing.T) {
	engine := NewEngine()

	testCases := []struct {
		name     string
		filePath string
		want     string
	}{
		{name: "sh extension", filePath: "script.sh", want: "bash"},
		{name: "bash extension", filePath: "script.bash", want: "bash"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := engine.detectLanguageFromFile(tc.filePath)
			if got != tc.want {
				t.Fatalf("detectLanguageFromFile(%q) = %q, want %q", tc.filePath, got, tc.want)
			}
		})
	}
}
