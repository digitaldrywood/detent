//go:build !windows

package validationcmd_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
	"github.com/digitaldrywood/detent/internal/workspace/internal/gitfixture"
)

func TestMain(m *testing.M) {
	if gotestreceipt.IsWrapperInvocation(os.Args[0]) {
		os.Exit(gotestreceipt.RunWrapper(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
	}
	hookproc.ReapProcessScanner = func(context.Context, string) ([]int, error) { return nil, nil }
	os.Exit(gitfixture.RunTests(m))
}

func TestLocalGitVersionReceiptsDoNotBindShellStartup(t *testing.T) {

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("Bash is required for the shell startup fixture")
	}
	startup := filepath.Join(t.TempDir(), "startup")
	if err := os.WriteFile(startup, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", startup)
	source := gitfixture.SourceRepo(t)
	options := workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true}
	options.Hooks.Shell = bash
	backend, err := workspace.NewLocalGit(options)
	if err != nil {
		t.Fatal(err)
	}
	issue := workspace.Issue{Identifier: "shell-startup"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	head, err := backend.Head(t.Context(), info, issue)
	if err != nil {
		t.Fatal(err)
	}
	issue.PullRequestHeadSHA = strings.TrimSpace(head)
	first, err := backend.RunReviewCommand(t.Context(), info, issue, "go version")
	if err != nil || first.ExitCode != 0 || first.Evidence == nil || first.Evidence.InputsSHA != "" {
		t.Fatalf("version observation was failed or reusable: %+v %v", first, err)
	}
	if err := os.WriteFile(startup, []byte("if [ \"$BASH_EXECUTION_STRING\" = \"go version\" ]; then exit 23; fi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preflight := backend.ReviewCommandEvidence(t.Context(), info, issue, first.Command)
	if preflight.InputsSHA != "" || preflight.Environment != first.Evidence.Environment {
		t.Fatalf("unchanged probes did not remain observations: %+v", preflight)
	}
	second, err := backend.RunReviewCommand(t.Context(), info, issue, first.Command)
	if err != nil || second.ExitCode != 23 || second.HeadSHA != first.HeadSHA || second.TreeSHA != first.TreeSHA {
		t.Fatalf("unchanged probes hid failing startup: %+v %v", second, err)
	}
}

func TestLocalGitReviewCommandRerunsExternalRuntimeInputs(t *testing.T) {

	t.Setenv("CGO_ENABLED", "0")
	fixture := filepath.Join(t.TempDir(), "runtime-input")
	t.Setenv("DETENT_RUNTIME_FIXTURE", fixture)
	if err := os.WriteFile(fixture, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := gitfixture.SourceRepo(t)
	for name, content := range map[string]string{
		"go.mod": "module example.test/runtime\n\ngo 1.26.0\n",
		"runtime_test.go": `package runtimefixture
import ("os"; "testing")
func TestExternalFixture(t *testing.T) {
 data, err := os.ReadFile(os.Getenv("DETENT_RUNTIME_FIXTURE"))
 if err != nil || string(data) != "ready" { t.Fatalf("external fixture unavailable: %v", err) }
}
`,
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitfixture.Git(t, source, "add", "go.mod", "runtime_test.go")
	gitfixture.Git(t, source, "commit", "-m", "external runtime input fixture")
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := workspace.Issue{Identifier: "runtime-input"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	head, err := backend.Head(t.Context(), info, issue)
	if err != nil {
		t.Fatal(err)
	}
	issue.PullRequestHeadSHA = strings.TrimSpace(head)
	first, err := backend.RunReviewCommand(t.Context(), info, issue, "go test ./... -count=1")
	if err != nil || first.ExitCode != 0 {
		t.Fatalf("initial fixture failed: %+v %v", first, err)
	}
	if first.Evidence == nil || first.Evidence.InputsSHA != "" {
		t.Fatal("test runtime received a reusable source/tool identity")
	}
	if err := os.WriteFile(fixture, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	preflight := backend.ReviewCommandEvidence(t.Context(), info, issue, first.Command)
	if preflight.InputsSHA != "" {
		t.Fatal("external runtime inputs were considered bound")
	}
	second, err := backend.RunReviewCommand(t.Context(), info, issue, first.Command)
	if err != nil || second.ExitCode == 0 || second.HeadSHA != first.HeadSHA || second.TreeSHA != first.TreeSHA || !strings.Contains(second.Output, "external fixture unavailable") {
		t.Fatalf("unchanged source did not observe changed runtime: %+v %v", second, err)
	}
}
