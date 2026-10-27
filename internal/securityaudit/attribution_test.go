package securityaudit_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_API_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CODEX_API_KEY"} {
		if err := os.Unsetenv(key); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

func TestFindingAttributionRequiresExactBaseReproduction(t *testing.T) {
	for _, test := range []struct {
		name, command, want string
		missingBase         bool
	}{
		{name: "same diagnostic on exact base", command: "sh check.sh existing", want: "independent"},
		{name: "untouched failure caused by changed caller", command: "sh check.sh caller", want: "change"},
		{name: "base cannot run command", command: "sh check.sh unavailable", want: "unknown"},
		{name: "base command modifies source", command: "sh check.sh mutates", want: "unknown"},
		{name: "base commit unavailable", command: "sh check.sh existing", missingBase: true, want: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory, base, head := attributionRepository(t)
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: t.TempDir(), SourceRoot: directory})
			if err != nil {
				t.Fatal(err)
			}
			info := workspace.Info{Path: directory}
			headResult, err := backend.RunBaseCommand(t.Context(), info, workspace.Issue{}, head, test.command)
			if err != nil {
				t.Fatal(err)
			}
			reproductionBase := base
			if test.missingBase {
				reproductionBase = strings.Repeat("f", 40)
			}
			baseResult, baseErr := backend.RunBaseCommand(t.Context(), info, workspace.Issue{}, reproductionBase, test.command)
			observation := gate.CommandObservation(baseResult)
			observation.Complete = observation.Complete && baseErr == nil
			headResult.BaseReproduction = &observation
			diagnostic := strings.TrimSpace(headResult.Output)
			findings := []securityaudit.Finding{{ID: "defect", Body: "defect", Path: "untouched.go", CheckCommand: test.command, CheckDiagnostic: diagnostic}}
			got := securityaudit.AttributeFindings(base, head, findings, []securityaudit.CheckComparison{{Head: gate.CommandObservation(headResult), Base: observation}})
			if got != test.want || findings[0].Attribution != test.want {
				t.Fatalf("attribution = %s, finding = %#v; want %s", got, findings[0], test.want)
			}
			if test.want != "unknown" {
				e := findings[0].AttributionEvidence
				if e == nil || e.BaseSHA != base || e.HeadSHA != head || e.Command != test.command || e.Diagnostic != diagnostic || e.BaseOutputDigest == "" {
					t.Fatalf("missing source-bound evidence: %#v", e)
				}
			}
			if got := attributionGit(t, directory, "rev-parse", "HEAD"); got != head {
				t.Fatalf("reproduction changed head: %s", got)
			}
			if got := attributionGit(t, directory, "diff", "--name-only", base, head); got != "caller" {
				t.Fatalf("fixture must change only caller: %s", got)
			}
		})
	}
}

func TestMixedFindingAttributionPreservesEveryFinding(t *testing.T) {
	directory, base, head := attributionRepository(t)
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: t.TempDir(), SourceRoot: directory})
	if err != nil {
		t.Fatal(err)
	}
	commandNames := []string{"sh check.sh existing", "sh check.sh caller"}
	commands := make([]gate.CommandResult, len(commandNames))
	for i, command := range commandNames {
		h, err := backend.RunBaseCommand(t.Context(), workspace.Info{Path: directory}, workspace.Issue{}, head, command)
		if err != nil {
			t.Fatal(err)
		}
		b, err := backend.RunBaseCommand(t.Context(), workspace.Info{Path: directory}, workspace.Issue{}, base, command)
		if err != nil {
			t.Fatal(err)
		}
		observation := gate.CommandObservation(b)
		h.BaseReproduction = &observation
		commands[i] = h
	}
	findings := []securityaudit.Finding{{ID: "existing", Body: "existing", CheckCommand: commands[0].Command, CheckDiagnostic: strings.TrimSpace(commands[0].Output)}, {ID: "caller", Body: "caller", CheckCommand: commands[1].Command, CheckDiagnostic: strings.TrimSpace(commands[1].Output)}, {ID: "unknown", Body: "unreproduced", Path: "untouched.go"}}
	checks := []securityaudit.CheckComparison{{Head: gate.CommandObservation(commands[0]), Base: *commands[0].BaseReproduction}, {Head: gate.CommandObservation(commands[1]), Base: *commands[1].BaseReproduction}}
	got := securityaudit.AttributeFindings(base, head, findings, checks)
	if got != "change" || len(findings) != 3 || findings[0].Attribution != "independent" || findings[1].Attribution != "change" || findings[2].Attribution != "unknown" {
		t.Fatalf("mixed attribution = %s, findings = %#v", got, findings)
	}
}

func attributionRepository(t *testing.T) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	attributionGit(t, directory, "init", "-b", "develop")
	attributionGit(t, directory, "config", "user.name", "Example User")
	attributionGit(t, directory, "config", "user.email", "user@example.com")
	attributionGit(t, directory, "config", "commit.gpgsign", "false")
	script := `case "$1" in
 existing) echo 'untouched.go:4: existing defect'; exit 1 ;;
 caller) if [ "$(cat caller)" = new ]; then echo 'untouched.go:9: changed caller defect'; exit 1; fi ;;
 mutates) if [ "$(cat caller)" = old ]; then echo modified > caller; fi; echo 'untouched.go:15: failure'; exit 1 ;;
 unavailable) if [ "$(cat caller)" = old ]; then exec detent-example-unavailable-command; fi; echo 'untouched.go:12: failure'; exit 1 ;;
esac
`
	for name, value := range map[string]string{"check.sh": script, "caller": "old"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	attributionGit(t, directory, "add", ".")
	attributionGit(t, directory, "commit", "-m", "base")
	base := attributionGit(t, directory, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(directory, "caller"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	attributionGit(t, directory, "add", "caller")
	attributionGit(t, directory, "commit", "-m", "change caller")
	return directory, base, attributionGit(t, directory, "rev-parse", "HEAD")
}

func attributionGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
