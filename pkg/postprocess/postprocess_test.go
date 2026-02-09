package postprocess

import (
	"strings"
	"testing"

	"github.com/semerge/semerge/pkg/core"
)

func TestBuildResolvedBlocks_UserServiceSnippetNoMutation(t *testing.T) {
	conflicts := []core.Hunk{{FileName: "service.go", Language: "go"}}
	resolvedMap := map[int]string{
		0: "func (s *UserService) CreateUser(user *User) error {\n\ts.userCount++\n\tuser.ID = s.userCount\n\ts.nextID++\n\treturn nil\n}",
	}

	blocks, err := BuildResolvedBlocks(conflicts, resolvedMap)
	if err != nil {
		t.Fatalf("BuildResolvedBlocks returned error: %v", err)
	}

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}

	got := blocks[0]
	if !strings.Contains(got, "s.userCount++") {
		t.Fatalf("expected s.userCount++ to be preserved, got: %q", got)
	}
	if !strings.Contains(got, "user.ID = s.userCount") {
		t.Fatalf("expected user.ID assignment to be preserved, got: %q", got)
	}
	if !strings.Contains(got, "s.nextID++") {
		t.Fatalf("expected s.nextID++ to be preserved, got: %q", got)
	}
}

func TestPostProcessMergedContent_GoOnlyFormatsNoSemanticRewrite(t *testing.T) {
	input := "package demo\n\nfunc f() {\n\tuser.ID = s.userCount\n\ts.nextID++\n}\n"

	got := PostProcessMergedContent("go", input)

	if !strings.Contains(got, "user.ID = s.userCount") {
		t.Fatalf("expected user.ID assignment to remain, got: %q", got)
	}
	if !strings.Contains(got, "s.nextID++") {
		t.Fatalf("expected s.nextID++ to remain, got: %q", got)
	}
}

func TestApplyResolutionsToConflictOutput_DoesNotAutoTrimTrailingBrace(t *testing.T) {
	conflictOutput := strings.Join([]string{
		"func f() {",
		"<<<<<<< current",
		"a()",
		"=======",
		"b()",
		">>>>>>> incoming",
		"}",
		"",
	}, "\n")

	resolutions := []string{"if cond {\n\twork()\n}"}

	got, err := ApplyResolutionsToConflictOutput(conflictOutput, resolutions)
	if err != nil {
		t.Fatalf("ApplyResolutionsToConflictOutput returned error: %v", err)
	}

	if strings.Count(got, "}") != 2 {
		t.Fatalf("expected both braces to be preserved, got: %q", got)
	}
}
