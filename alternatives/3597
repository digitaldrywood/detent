package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gobudget"
)

func TestReviewCommandSharesGoBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell subprocess integration")
	}
	for _, test := range []struct {
		name, flags, procs, wantFlags, wantProcs string
		enabled                                  bool
	}{
		{name: "nested module", enabled: true, wantFlags: "-p=2 -trimpath", wantProcs: "2"},
		{name: "operator limits", enabled: true, flags: "-p=1 -trimpath=false", procs: "1", wantFlags: "-p=1 -trimpath=false", wantProcs: "1"},
		{name: "disabled", flags: "-p=1", procs: "3", wantFlags: "-p=1", wantProcs: "3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GOFLAGS", test.flags)
			t.Setenv("GOMAXPROCS", test.procs)
			if test.procs == "" {
				if err := os.Unsetenv("GOMAXPROCS"); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(gobudget.DirEnvironment, "")
			t.Setenv(gobudget.SlotsEnvironment, "")
			source := initSourceRepo(t)
			if err := os.MkdirAll(filepath.Join(source, "apps", "api"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "apps", "api", "go.mod"), []byte("module example.com/api\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "add", "apps")
			runGit(t, source, "commit", "-m", "nested module fixture")
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "nested-module-validation"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			head, err := backend.Head(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			issue.PullRequestHeadSHA = strings.TrimSpace(head)
			ctx := t.Context()
			budget := gobudget.New(8, filepath.Join(t.TempDir(), "budget"), "/opt/example/detent")
			wantFlags := test.wantFlags
			wantDir, wantSlots := "", ""
			if test.enabled {
				ctx = gobudget.WithContext(ctx, budget)
				wantFlags += " -toolexec=" + budget.WrapperPath()
				wantDir, wantSlots = budget.Dir, "8"
			}
			command := `cd apps/api && printf '%s\n' "$GOFLAGS" "$GOMAXPROCS" "$DETENT_GO_BUDGET_DIR" "$DETENT_GO_BUDGET_SLOTS"`
			receipt, err := backend.RunReviewCommand(ctx, info, issue, command)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Join([]string{wantFlags, test.wantProcs, wantDir, wantSlots, ""}, "\n")
			if receipt.ExitCode != 0 || receipt.Output != want {
				t.Fatalf("child environment: exit=%d output=%q, want %q", receipt.ExitCode, receipt.Output, want)
			}
		})
	}
}
