package planphase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner/runworkspace"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type revisionSource struct {
	workspace.Backend
	runworkspace.PhasePublisher
}

func (revisionSource) ItemPhaseHead(context.Context, workspace.Info, workspace.Issue) (string, string, error) {
	return "head", "tree", nil
}

func TestHumanRevisionIncludesStoredDraft(t *testing.T) {
	t.Parallel()
	stop := errors.New("stop after inspecting the revision prompt")
	_, _, err := Run(t.Context(), Request{
		Backend: revisionSource{},
		Continuation: &tracker.NativePhaseContinuation{Plan: &tracker.NativePlanState{
			Draft: "Add a README example.", Answer: "Include the empty input case.", AnswerID: "answer",
		}},
		HumanReview: true,
		Prompt:      func() (string, error) { return "Draft the implementation plan.", nil },
		OpenAgent: func() (func(context.Context, string) (string, error), error) {
			return func(_ context.Context, prompt string) (string, error) {
				if !strings.Contains(prompt, "Add a README example.") || !strings.Contains(prompt, "Include the empty input case.") {
					t.Fatalf("revision is missing the bound draft or reply: %q", prompt)
				}
				return "", stop
			}, nil
		},
	})
	if !errors.Is(err, stop) {
		t.Fatalf("revision did not reach the planner: %v", err)
	}
}
