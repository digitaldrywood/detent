package runner

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRoundThreeLegacyPublishedRecapture(t *testing.T) {
	root := t.TempDir()
	checkout := func(source string) *workspace.LocalGit {
		t.Helper()
		local, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true, ProcessSource: emptyProcessSource})
		if err != nil {
			t.Fatal(err)
		}
		return local
	}
	local := checkout(initRunnerSourceRepo(t))
	issue := workspace.Issue{ProjectID: "local", ID: "wi_example", Identifier: "prj_example#3"}
	info, err := local.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))
	attemptDiffFile(t, info.Path, "review.txt", "verified source\n")
	runRunnerGit(t, info.Path, "add", "review.txt")
	runRunnerGit(t, info.Path, "commit", "-m", "publish source")
	head := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))
	published, err := workspace.CaptureChangeSource(t.Context(), info.Path, base, head)
	if err != nil {
		t.Fatal(err)
	}
	issue.AttemptWorkspace = workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
	issue.RecoveryAttemptWorkspace = issue.AttemptWorkspace
	issue.RecoveryAttemptWorkspace.Attempt = "attempt_22222222222222222222222222222222"
	recapture := func(backend *workspace.LocalGit) error {
		t.Helper()
		execution := &resolutionExecution{}
		if err := recoverRunChangeSource(t.Context(), RunRequest{Execution: execution}, backend, &issue, false, true); err != nil {
			return err
		}
		if execution.capture == nil {
			t.Fatal("missing published-source recapture callback")
		}
		got, err := execution.capture(t.Context(), base, head)
		if err != nil {
			return err
		}
		if got.Source.DiffSHA256 != published.Source.DiffSHA256 {
			t.Fatal("recaptured diff differs from published version")
		}
		return got.Source.Validate(base, head, got.Bundle)
	}
	if err := recapture(local); err != nil {
		t.Fatal(err)
	}
	if err := recapture(checkout(initRunnerSourceRepo(t))); err == nil {
		t.Fatal("accepted a checkout from a different repository")
	}
	runRunnerGit(t, info.Path, "commit", "--allow-empty", "-m", "advance source")
	if err := recapture(local); err == nil {
		t.Fatal("accepted a checkout with a different head")
	}
}
