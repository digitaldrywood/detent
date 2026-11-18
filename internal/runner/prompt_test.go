package runner

import (
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeOrphanHandoffPreservesPlanOnly(t *testing.T) {
	t.Parallel()
	for _, planOnly := range []bool{false, true} {
		t.Run(strconv.FormatBool(planOnly), func(t *testing.T) {
			backend := &fakeCodexClient{}
			run, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource,
				Workflow:     config.Workflow{Config: config.Config{Tracker: config.Tracker{Kind: config.TrackerHubNative}, Agent: config.Agent{Followups: config.Followups{Enabled: true}}}, Prompt: "Implement the issue"},
				Workspace:    &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "wi_example", Branch: "detent/wi_example"}},
				AgentBackend: backend,
			})
			if err != nil {
				t.Fatal(err)
			}
			mode, role := RunModeImplement, RoleCode
			if planOnly {
				mode, role = RunModePlan, RolePlan
			}
			request := RunRequest{
				Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1", State: "In Progress", ModelOverride: "gpt-5.6-codex", Metadata: map[string]string{"hub_profile": "native", "hub_project_id": "prj_example"}},
				Mode:  mode, WorkAttemptID: 12, Generation: 2, RetryMode: RetryModeResume,
				ResumeState: store.AgentResumeState{DetentSessionID: 11, ProviderThreadID: "original-thread", RequestedModel: "gpt-5.6-codex", AgentBackendID: "codex", AgentBackendKind: "codex", AgentRole: role, Orphaned: true},
			}
			request.Execution = prepareOrphanResumeExecution(t, run, request.ResumeState)
			_, err = run.Run(t.Context(), request)

			if err != nil {
				t.Fatal(err)
			}
			prompt := backend.request.Prompt
			if !strings.Contains(prompt, "## Native completion contract") {
				t.Fatal("native orphan lost current completion ownership")
			}
			if !strings.HasPrefix(prompt, orphanRestartInstruction) || strings.Contains(prompt, repositoryHandoffContract) || strings.Contains(prompt, "## Blocked handoff") {
				t.Error("native orphan inherited the GitHub Workpad handoff")
			}
			if !strings.Contains(prompt, "## Completion handoff") || !strings.Contains(prompt, `completion_work_attempt_id: "12"`) {
				t.Error("native orphan lost the current attempt completion fields")
			}
			if strings.Contains(prompt, "## Out-of-scope discoveries") != !planOnly {
				t.Error("resumed plan-only permission differs for out-of-scope discoveries")
			}
		})
	}
}
