package prompt_test

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

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
			prompt, err := runner.BuildPrompt(workflow, connector.Issue{ID: "wi_1", State: "Rework"}, runner.PromptOptions{PlanOnly: test.planOnly, ReviewConversationRefusal: runner.NativeReviewConversationRefusal(test.recovery)})
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
