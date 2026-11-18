//go:build !windows

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLandingRemovalGitFailure(t *testing.T) {
	testLandingRemovalFailure(t, []string{"git removal fails"})
}

func TestLocalGitRebasedNativeLanding(t *testing.T) {

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, mode := range []string{gate.LandingPerChange, gate.LandingRollingBarrier} {
		t.Run(mode, func(t *testing.T) {
			f := newLandingFixture(t)
			for name, content := range map[string]string{"go.mod": "module native-landing.test\n\ngo 1.26\n", "calc.go": "package calc\nfunc Value() int { return 1 }\n"} {
				if err := os.WriteFile(filepath.Join(f.info.Path, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, f.info.Path, "add", "go.mod", "calc.go")
			runGit(t, f.info.Path, "commit", "-m", "add calculator")
			first := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(f.info.Path, "calc.go"), []byte("package calc\nfunc Value() int { return 2 }\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.info.Path, "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value()!=2 { t.Fatal(Value()) } }\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, f.info.Path, "add", "calc.go", "calc_test.go")
			runGit(t, f.info.Path, "commit", "-m", "change calculator")
			head := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
			runGit(t, f.source, "cherry-pick", first)
			runGit(t, f.source, "push", "origin", "main")
			lint := t.TempDir()
			lintLog := filepath.Join(lint, "arguments")
			if err := os.WriteFile(filepath.Join(lint, "golangci-lint"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > '"+lintLog+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", lint+string(os.PathListSeparator)+os.Getenv("PATH"))
			tree := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD^{tree}"))
			baseBefore := f.remoteMain(t)
			options := LandOptions{LandingMode: mode, Native: true, HeadSHA: head, Method: "squash", TargetBranch: "main", Message: "rebased", ValidationCommand: "exit 73", Validation: &gate.CommandResult{Command: "exit 73", HeadSHA: head, TreeSHA: tree, DurationNS: int64(time.Second)}, Repository: RepositoryURL(t.Context(), f.info.Path), SecurityAuditRequired: true}
			options.SecurityAudit = func(_ context.Context, diff FileDiffs) (*tracker.SecurityAuditReview, error) {
				if diff.HeadSHA == head || diff.BaseSHA != baseBefore || diff.Truncated {
					t.Errorf("rebase reused published source identity: %+v", diff)
				}
				now := time.Now()
				return &tracker.SecurityAuditReview{Landing: true, Verdict: "pass", Run: securityaudit.Run{ID: 1, InvocationID: "rebase-audit", ProjectID: "project", Repository: options.Repository, BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), ExitStatus: securityaudit.ExitStatusSuccess, OutputDigest: securityaudit.OutputDigest("pass"), OutputBytes: 4, Verdict: "pass", Summary: "No findings", WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now, CompletedAt: now}}, nil
			}
			result, err := f.backend.LandChange(t.Context(), f.info, f.issue, options)
			if err == nil && (result.SecurityAudit == nil || result.SecurityAudit.Run.BaseSHA != result.BaseBefore || result.SecurityAudit.Run.HeadSHA != result.MergeSHA) {
				t.Fatalf("rebased landing has no exact audit: %+v", result)
			}
			if len(result.Pipeline) < 1 {
				t.Fatalf("missing rebase timing: %+v", result)
			}
			found := false
			for _, timing := range result.Pipeline {
				if timing.Stage == "rebase" {
					found = true
					if timing.StartedAt.IsZero() || timing.FinishedAt.Before(timing.StartedAt) || timing.Outcome != "clean" {
						t.Fatalf("rebase=%+v", timing)
					}
				}
			}
			if !found {
				t.Fatalf("missing rebase=%+v", result.Pipeline)
			}
			if err != nil || !result.Rebased || result.Path != "rebase_short_validation" || !reflect.DeepEqual(result.Packages, []string{"."}) || result.Gate.HeadSHA != result.MergeSHA || !strings.Contains(result.Gate.Command, "go test -p ${TEST_PROCS:-4} -short -timeout=60s '.'") || !strings.Contains(result.Gate.Output, "native-landing.test") || f.remoteMain(t) != result.MergeSHA {
				t.Fatalf("rebase outcome = %#v, %v", result, err)
			}
			data, err := os.ReadFile(lintLog)
			if err != nil || !strings.Contains(string(data), "--new-from-rev="+result.BaseBefore+" --whole-files .") {
				t.Fatalf("lint scope = %q, %v", data, err)
			}
		})
	}
}
