package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildBlockReadsSourcesInOrderAndIgnoresMissingFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	globalPath := filepath.Join(root, "global.md")
	projectPath := filepath.Join(root, "project.md")
	if err := os.WriteFile(globalPath, []byte("Global standard\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(global) error = %v", err)
	}
	if err := os.WriteFile(projectPath, []byte("Project rule\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(project) error = %v", err)
	}

	block, err := BuildBlock([]Source{
		{Name: "Global", Path: globalPath},
		{Name: "Missing", Path: filepath.Join(root, "missing.md")},
		{Name: "Project", Path: projectPath},
	}, Options{})
	if err != nil {
		t.Fatalf("BuildBlock() error = %v", err)
	}

	for _, want := range []string{
		"## Team knowledge",
		"### Global",
		"Global standard",
		"### Project",
		"Project rule",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "### Missing") {
		t.Fatalf("block includes missing source:\n%s", block)
	}
	if strings.Index(block, "Global standard") > strings.Index(block, "Project rule") {
		t.Fatalf("block order = %q, want global before project", block)
	}
}

func TestBuildBlockReturnsEmptyForNoReadableKnowledge(t *testing.T) {
	t.Parallel()

	block, err := BuildBlock([]Source{{Name: "Missing", Path: filepath.Join(t.TempDir(), "missing.md")}}, Options{})
	if err != nil {
		t.Fatalf("BuildBlock() error = %v", err)
	}
	if block != "" {
		t.Fatalf("BuildBlock() = %q, want empty", block)
	}
}

func TestBuildBlockFailsForUnreadableConfiguredSource(t *testing.T) {
	t.Parallel()

	_, err := BuildBlock([]Source{{Name: "Directory", Path: t.TempDir()}}, Options{})
	if err == nil {
		t.Fatal("BuildBlock() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "read shared knowledge") {
		t.Fatalf("BuildBlock() error = %v, want shared knowledge context", err)
	}
}

func TestBuildBlockCapsRenderedOutput(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "large.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 4096)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	block, err := BuildBlock([]Source{{Name: "Large", Path: path}}, Options{MaxBytes: 512})
	if err != nil {
		t.Fatalf("BuildBlock() error = %v", err)
	}

	if len(block) > 512 {
		t.Fatalf("len(block) = %d, want <= 512", len(block))
	}
	if !strings.Contains(block, "[truncated to first") {
		t.Fatalf("block missing truncation marker:\n%s", block)
	}
}
