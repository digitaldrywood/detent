//go:build !windows

package workspacefiles_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestNamesWithSurroundingWhitespaceAreServedAsListed(t *testing.T) {
	t.Parallel()
	service, worktree, _ := newWorktree(t)
	tests := []struct {
		name string
		file string
	}{
		{name: "leading and trailing spaces", file: " report "},
		{name: "trailing space", file: "notes.md "},
		{name: "leading tab", file: "\tdata"},
	}
	for _, test := range tests {
		if err := os.WriteFile(filepath.Join(worktree, test.file), []byte(test.file), 0o600); err != nil {
			t.Fatalf("create whitespace filename fixture %q: %v", test.file, err)
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stat, err := service.Stat(t.Context(), workspacesession.FilesRequest{Path: test.file})
			if err != nil || stat.Path != test.file {
				t.Fatalf("Stat(%q) = %+v, %v", test.file, stat, err)
			}
			content, err := service.Read(t.Context(), workspacesession.FilesRequest{Path: test.file})
			if err != nil || content.Data != test.file {
				t.Fatalf("Read(%q) = %q, %v; want the file's own bytes", test.file, content.Data, err)
			}
		})
	}
}
