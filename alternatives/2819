package runner

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeValidatorReadsPinnedSourceWithoutRemote(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	source := initRunnerSourceRepo(t)
	base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	attemptDiffFile(t, source, "source.txt", strings.Repeat("pinned source\n", 100))
	runRunnerGit(t, source, "add", "source.txt")
	runRunnerGit(t, source, "commit", "-m", "reviewed source")
	head := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	attemptDiffFile(t, source, "unreviewed.txt", "unreviewed source\n")
	runRunnerGit(t, source, "add", "unreviewed.txt")
	runRunnerGit(t, source, "commit", "-m", "later branch tip")
	other := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	runRunnerGit(t, source, "checkout", "--detach", head)
	patch := runRunnerGit(t, source, "diff", "--no-ext-diff", "--no-textconv", base, head, "--")
	limit := 64
	if len(patch) <= limit || strings.TrimSpace(runRunnerGit(t, source, "remote")) != "" {
		t.Fatal("fixture must exceed the inline budget and have no remote")
	}
	for _, test := range []struct {
		name        string
		omitted     bool
		wrongHead   bool
		wrongDiff   bool
		wrongBase   bool
		missingDiff bool
		missingID   bool
	}{
		{name: "patch exceeds inline budget"},
		{name: "source owner omitted patch", omitted: true},
		{name: "wrong workspace head", wrongHead: true},
		{name: "wrong diff head", wrongDiff: true},
		{name: "wrong diff base", wrongBase: true},
		{name: "source evidence unavailable", missingDiff: true},
		{name: "version identity unavailable", missingID: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := (&attachmentValidatorExecution{}).ValidatorVersion(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			input.Version.BaseSHA, input.Version.HeadSHA = base, head
			input.Diff.BaseSHA, input.Diff.HeadSHA, input.Diff.Patch = base, head, patch
			input.Diff.Files = []string{"source.txt"}
			observed := head
			if test.omitted {
				input.Diff.Patch = ""
			}
			if test.wrongHead {
				observed = other
			}
			if test.wrongDiff {
				input.Diff.HeadSHA = other
			}
			if test.wrongBase {
				input.Diff.BaseSHA = other
			}
			if test.missingDiff {
				input.Diff = nil
			}
			if test.missingID {
				input.Version.ID = ""
			}
			backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: `{"verdict":"pass","score":1,"summary":"Reviewed pinned source"}`}}}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{Config: config.Config{
					Tracker: config.Tracker{Kind: config.TrackerHubNative},
					Gate:    gate.Config{Validator: gate.ValidatorConfig{Enabled: true, MaxInlineDiffBytes: &limit}},
					Agents:  config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{{Name: "validator", Role: RoleValidator, Backend: "codex", Model: "example-model"}}},
				}, Prompt: "Review"},
				Workspace:    &attachmentValidatorWorkspace{reviewMutationWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: source}}, heads: []string{observed}}},
				AgentBackend: backend,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Validate(t.Context(), ValidatorRequest{Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1"}, NativeVersion: input.Version, Diff: input.Diff})
			if test.wrongHead || test.wrongDiff || test.wrongBase || test.missingDiff || test.missingID {
				if !errors.Is(err, ErrValidatorInfrastructure) || result.Submitted || backend.calls != 0 {
					t.Fatalf("unbound source reached review: result=%+v err=%v calls=%d", result, err, backend.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.VersionID != input.Version.ID || result.BaseSHA != base || result.HeadSHA != head || result.DiffDigest != input.Diff.Digest {
				t.Fatalf("review lost immutable evidence identity: %+v", result)
			}
			prompt := backend.request.Prompt
			if !backend.request.ReadOnly || strings.Contains(prompt, "+pinned source") || strings.Contains(prompt, "use the named GitHub PR diff") || !strings.Contains(prompt, "Full diff omitted") {
				t.Fatal("native omitted diff did not receive a read-only source fallback")
			}
			commands := regexp.MustCompile("(?m)^`(git [^`]+)`$").FindAllStringSubmatch(prompt, -1)
			if len(commands) != 3 {
				t.Fatalf("native source fallback supplied %d commands, want two object checks and a comparison", len(commands))
			}
			for i, command := range commands {
				args := strings.Fields(command[1])
				output := runRunnerGit(t, backend.request.Workspace, args[1:]...)
				switch i {
				case 0:
					if strings.TrimSpace(output) != base {
						t.Fatal("source fallback did not resolve the pinned base")
					}
				case 1:
					if strings.TrimSpace(output) != head {
						t.Fatal("source fallback did not resolve the pinned head")
					}
				case 2:
					if !strings.Contains(output, "+pinned source") || strings.Contains(output, "unreviewed source") {
						t.Fatal("source fallback did not compare only the intended immutable change")
					}
				}
			}
		})
	}
}
