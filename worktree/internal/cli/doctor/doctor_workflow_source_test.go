package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initDoctorWorkflowSourceRepository(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	t.Cleanup(func() { cleanupDoctorWorkflowSourceRepository(t, root) })
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	repo := filepath.Join(root, "checkout")
	runDoctorWorkflowSourceGit(t, "", "init", "--bare", "--initial-branch=main", remote)
	runDoctorWorkflowSourceGit(t, "", "init", "--initial-branch=main", seed)
	runDoctorWorkflowSourceGit(t, seed, "config", "user.name", "Detent Test")
	runDoctorWorkflowSourceGit(t, seed, "config", "user.email", "detent@example.com")
	writeDoctorWorkflowSourceFile(t, filepath.Join(seed, "WORKFLOW.md"), "Agent direction.\n")
	writeDoctorWorkflowSourceFile(t, filepath.Join(seed, "detent.yaml"), "schema: 1\ntracker:\n  kind: hub_native\n")
	runDoctorWorkflowSourceGit(t, seed, "add", "WORKFLOW.md", "detent.yaml")
	runDoctorWorkflowSourceGit(t, seed, "commit", "-m", "initial workflow policy")
	runDoctorWorkflowSourceGit(t, seed, "remote", "add", "origin", remote)
	runDoctorWorkflowSourceGit(t, seed, "push", "-u", "origin", "main")
	runDoctorWorkflowSourceGit(t, "", "clone", remote, repo)
	runDoctorWorkflowSourceGit(t, repo, "config", "user.name", "Detent Test")
	runDoctorWorkflowSourceGit(t, repo, "config", "user.email", "detent@example.com")
	return repo, remote
}

func cleanupDoctorWorkflowSourceRepository(t *testing.T, root string) {
	t.Helper()

	err := os.RemoveAll(root)
	if err == nil {
		return
	}

	remaining := make([]string, 0)
	walkErr := filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relativeErr := filepath.Rel(root, path)
		if relativeErr != nil {
			return relativeErr
		}
		remaining = append(remaining, relative)
		return nil
	})
	t.Errorf("RemoveAll(%s) error = %v; remaining paths = %q; inspect error = %v", root, err, remaining, walkErr)
}

func writeDoctorWorkflowSourceFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", filepath.Base(path), err)
	}
}

func runDoctorWorkflowSourceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	gitArgs := append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)
	cmd := exec.CommandContext(t.Context(), "git", gitArgs...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s error = %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
