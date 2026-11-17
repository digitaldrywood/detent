package runner

import (
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
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

func TestReviewConversationRefusalReachesNativeRework(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	finding := "- review-bot on internal/app.go:12: handle the empty case"
	refusal := workspace.ReviewConversationsRefusalText + " Address each finding in Rework, then resolve its conversation with evidence of the fix or dismissal.\n" + finding
	recovery := func(version, exitDetail string) tracker.NativeRecovery {
		return tracker.NativeRecovery{
			Change:   &tracker.NativeChangeReference{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head},
			Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{ChangeID: "change_1", VersionID: version, HeadSHA: head, RefusalKind: workspace.LandRefusalProtected}}}}},
			History: []tracker.CollaborationEvent{
				{Type: "workflow.transitioned", Data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", ReasonDetail: refusal}},
				{Type: "workflow.transitioned", Data: tracker.CollaborationData{FromState: "Rework", ToState: "Merging", ReasonDetail: "The run succeeded"}},
				{Type: "workflow.transitioned", Data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", ReasonDetail: exitDetail}},
			},
		}
	}
	for _, test := range []struct {
		name     string
		recovery tracker.NativeRecovery
		planOnly bool
		want     bool
	}{
		{name: "latest landing refusal names review conversations", recovery: recovery("version_1", refusal), want: true},
		{name: "a later different landing refusal supersedes it", recovery: recovery("version_1", "GitHub refused the pull request operation: Pull Request is still a draft")},
		{name: "a refusal of an earlier version does not apply", recovery: recovery("version_0", refusal)},
		{name: "planning does not receive the review handoff", recovery: recovery("version_1", refusal), planOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workflow := config.Workflow{Config: config.Config{Tracker: config.Tracker{Kind: config.TrackerHubNative}}, Prompt: "Implement the issue"}
			prompt, err := BuildPrompt(workflow, connector.Issue{ID: "wi_1", State: "Rework"}, PromptOptions{PlanOnly: test.planOnly, ReviewConversationRefusal: NativeReviewConversationRefusal(test.recovery)})
			if err != nil {
				t.Fatal(err)
			}
			index := strings.Index(prompt, "## Landing refused: unresolved review conversations")
			if (index >= 0) != test.want {
				t.Fatalf("review conversation handoff present = %t, want %t", index >= 0, test.want)
			}
			if !test.want {
				return
			}
			for _, required := range []string{"detent_read_review_threads", "detent_resolve_review_thread", finding} {
				if !strings.Contains(prompt[index:], required) {
					t.Errorf("review conversation handoff missing %q", required)
				}
			}
			if strings.Contains(prompt[index:], "human review") {
				t.Error("review conversation handoff routes work by Go policy")
			}
		})
	}
}
