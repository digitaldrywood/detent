package checkpointtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace/checkpoint"
)

func PublishPaused(t *testing.T, path, branch, ref string) (string, string) {
	t.Helper()
	run := func(args ...string) string {
		t.Helper()
		out, err := checkpoint.RefGit(path)(t.Context(), args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	tx, err := checkpoint.CaptureTransaction(t.Context(), path, branch, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	manifest := filepath.Join(scratch, "transaction.json")
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := run("hash-object", "-w", manifest)
	indexEnv := "GIT_INDEX_FILE=" + filepath.Join(scratch, "index")
	git := checkpoint.RefGit(path, indexEnv)
	if _, err := git(t.Context(), "read-tree", "--empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(t.Context(), "update-index", "--add", "--cacheinfo", "100644,"+digest+",transaction.json"); err != nil {
		t.Fatal(err)
	}
	for i, entry := range tx.Index {
		if _, err := git(t.Context(), "update-index", "--add", "--cacheinfo", fmt.Sprintf("100644,%s,alternatives/%d", entry.Blob, i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git(t.Context(), "read-tree", "--prefix=worktree/", tx.Worktree); err != nil {
		t.Fatal(err)
	}
	wrapper, err := git(t.Context(), "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	commit := run("commit-tree", strings.TrimSpace(wrapper), "-p", tx.Head, "-p", tx.Original, "-m", "legacy paused rebase")

	run("push", "origin", commit+":"+ref)
	return commit, digest
}
