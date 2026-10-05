package hubclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectSetupFailurePrecedesNativeClaim(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("fixture runs POSIX setup hooks")
	}
	h := newNativeChangeHub(t, true)
	issue, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "setup failure must not claim this", State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	selected := globalconfig.Project{ID: string(h.project), Workdir: root, Workflow: filepath.Join(root, "WORKFLOW.md")}
	if err := os.WriteFile(selected.Workflow, []byte("---\ntracker:\n  kind: memory\nhooks:\n  runner_setup: setup.sh\n  shell: sh\n---\nRun work.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSetup := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "setup.sh"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	assertRuns := func(want int) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(root, "trace"))
		if err != nil || strings.Count(string(content), "\n") != want {
			t.Fatalf("setup runs = %q, error = %v; want %d", content, err, want)
		}
	}
	localStore, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runner.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := localStore.Close(); err != nil {
			t.Error(err)
		}
	})
	setup := project.NewRunnerSetup("runner-one", localStore, nil)
	writeSetup("printf 'failure\\n' >> trace\nexit 7\n")
	h.scheduler.prepareProject = func(ctx context.Context, name string) error {
		if name == "local" {
			return setup.Prepare(ctx, selected)
		}
		if name != "other" {
			t.Errorf("unexpected setup project %q", name)
		}
		return nil
	}
	h.scheduler.localChecks = map[string]runnerauth.LocalChecks{
		"local": {Checkout: "passed", Doctor: "passed", Provider: "passed"},
		"other": {Checkout: "passed", Doctor: "passed", Provider: "passed"},
	}
	request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo"}}
	for range 2 {
		candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
		if !errors.Is(err, orchestrator.ErrSchedulingUnavailable) || len(candidates) != 0 || len(h.scheduler.nativeClaims) != 0 {
			t.Fatalf("failed setup candidates = %v, error = %v, claims = %v", candidates, err, h.scheduler.nativeClaims)
		}
		current, err := h.admin.Issue(t.Context(), issue.WorkItemID)
		if err != nil || current.State != "Todo" || current.Revision != issue.Revision {
			t.Fatalf("issue changed by instance failure: %+v, error = %v", current, err)
		}
	}
	assertRuns(2)
	if hash, err := localStore.ProjectRunnerSetupHash(t.Context(), "runner-one", selected.ID); err != nil || hash != "" {
		t.Fatalf("failed setup hash = %q, error = %v", hash, err)
	}
	if h.scheduler.localChecks["local"].Setup != "failed" {
		t.Fatal("failed project setup was not reported")
	}
	if err := h.scheduler.PrepareProject(t.Context(), "other"); err != nil || !h.scheduler.localChecks["other"].Passed() {
		t.Fatalf("other project was blocked: %v", err)
	}
	writeSetup("printf 'run\\n' >> trace\n")
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 1 || candidates[0].ID != string(issue.WorkItemID) || h.scheduler.localChecks["local"].Setup != "passed" {
		t.Fatalf("repaired setup candidates = %v, error = %v", candidates, err)
	}
	assertRuns(3)
	for range 2 {
		if err := h.scheduler.PrepareProject(t.Context(), "local"); err != nil {
			t.Fatal(err)
		}
	}
	assertRuns(3)
	hash, err := localStore.ProjectRunnerSetupHash(t.Context(), "runner-one", selected.ID)
	if err != nil || hash == "" {
		t.Fatalf("successful native setup hash = %q, error = %v", hash, err)
	}
	writeSetup("printf 'changed\\n' >> trace\n")
	for range 2 {
		if err := h.scheduler.PrepareProject(t.Context(), "local"); err != nil {
			t.Fatal(err)
		}
	}
	assertRuns(4)
	changed, err := localStore.ProjectRunnerSetupHash(t.Context(), "runner-one", selected.ID)
	if err != nil || changed == "" || changed == hash {
		t.Fatalf("changed native setup hash = %q, previous = %q, error = %v", changed, hash, err)
	}
}
