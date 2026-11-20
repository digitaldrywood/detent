package planphase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner/runworkspace"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
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

type planPublication struct {
	Execution
	plan  tracker.NativeStepReceipt
	phase string
}

func (p *planPublication) RecordStep(_ context.Context, plan tracker.NativeStepReceipt) error {
	p.plan = plan
	return nil
}

func (p *planPublication) RecordPhaseReceipt(_ context.Context, phase, _, _, _, _ string) error {
	p.phase = phase
	return nil
}

func (revisionSource) PushItemPhase(context.Context, workspace.Info, workspace.Issue, func(context.Context) error) (string, string, string, error) {
	return "branch", "head", "tree", nil
}

func TestPlanQuestionOwnsTheDraft(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"replacement question", "draft contains status", "draft contains unfinished status", "review turn limit"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const draft = "Add a README example."
			plan := &tracker.NativePlanState{}
			output := draft
			stop := errors.New("agent session turn limit exceeded")
			if name == "replacement question" {
				plan.Draft = draft
			}
			if name == "draft contains status" {
				output = workpad.BlockedDetail(draft, "What file should I change?")
			}
			if name == "draft contains unfinished status" {
				output = draft + "\n\n```detent-status\nstatus: blocked"
			}
			publication := &planPublication{}
			turns := 0
			request := Request{
				Execution: publication, Backend: revisionSource{}, Now: time.Now,
				Continuation: &tracker.NativePhaseContinuation{Plan: plan},
				HumanReview:  true, AutomatedReview: name == "review turn limit",
				Prompt: func() (string, error) { return "Draft the plan.", nil },
				OpenAgent: func() (func(context.Context, string) (string, error), error) {
					return func(context.Context, string) (string, error) {
						turns++
						if turns > 1 {
							return "", stop
						}
						return output, nil
					}, nil
				},
			}
			_, question, err := Run(t.Context(), request)
			if name == "review turn limit" {
				if !errors.Is(err, stop) || publication.phase != "plan_review" || publication.plan.CompletionBody != draft {
					t.Fatalf("the next attempt cannot review this draft: phase=%q draft=%q err=%v", publication.phase, publication.plan.CompletionBody, err)
				}
				request.Continuation = &tracker.NativePhaseContinuation{Plan: &tracker.NativePlanState{Draft: publication.plan.CompletionBody, ReviewPending: true}}
				request.OpenAgent = func() (func(context.Context, string) (string, error), error) {
					return func(_ context.Context, prompt string) (string, error) {
						if !strings.HasPrefix(prompt, "Independently review") || !strings.Contains(prompt, draft) {
							t.Fatalf("replacement regenerated the draft: %q", prompt)
						}
						return "APPROVED", nil
					}, nil
				}
				approved, question, err := Run(t.Context(), request)
				if err != nil || question != "" || approved != draft || publication.phase != "plan" {
					t.Fatalf("replacement did not finish review: %q, %q, %q, %v", approved, question, publication.phase, err)
				}
				return
			}
			signal, ok := workpad.SignalFromComment(question, "", "")
			if err != nil || !ok || signal.Invalid != nil || signal.HumanAction != tracker.PlanApprovalQuestion {
				t.Fatalf("completion did not ask the runner-owned question: %q, %v", question, err)
			}
			if publication.phase != "plan_draft" || publication.plan.CompletionBody != draft {
				t.Fatalf("question lacks its own draft binding: phase=%q draft=%q", publication.phase, publication.plan.CompletionBody)
			}
		})
	}
}

type changingPlanSource struct {
	revisionSource
	head, tree             string
	pushedHead, pushedTree string
	pushes                 int
}

func (s *changingPlanSource) ItemPhaseHead(context.Context, workspace.Info, workspace.Issue) (string, string, error) {
	return s.head, s.tree, nil
}

func (s *changingPlanSource) PushItemPhase(context.Context, workspace.Info, workspace.Issue, func(context.Context) error) (string, string, string, error) {
	s.pushes++
	return "branch", s.pushedHead, s.pushedTree, nil
}

func TestPlanReviewFailureKeepsPinnedSource(t *testing.T) {
	t.Parallel()
	for _, changed := range []string{"head during review", "tree during review", "pushed head", "pushed tree"} {
		t.Run(changed, func(t *testing.T) {
			t.Parallel()
			stop := errors.New("provider failed during review")
			publication := &planPublication{}
			source := &changingPlanSource{head: "head", tree: "tree", pushedHead: "head", pushedTree: "tree"}
			_, _, err := Run(t.Context(), Request{
				Execution: publication, Backend: source, Now: time.Now, AutomatedReview: true,
				Continuation: &tracker.NativePhaseContinuation{Plan: &tracker.NativePlanState{Draft: "Test empty input.", ReviewPending: true}},
				Prompt:       func() (string, error) { return "Draft the plan.", nil },
				OpenAgent: func() (func(context.Context, string) (string, error), error) {
					return func(context.Context, string) (string, error) {
						switch changed {
						case "head during review":
							source.head = "changed"
						case "tree during review":
							source.tree = "changed"
						case "pushed head":
							source.pushedHead = "changed"
						case "pushed tree":
							source.pushedTree = "changed"
						}
						return "", stop
					}, nil
				},
			})
			if !errors.Is(err, stop) || publication.phase != "" {
				t.Fatalf("recorded a review of changed source: phase=%q error=%v", publication.phase, err)
			}
			if strings.HasSuffix(changed, "during review") && (source.pushes != 0 || publication.plan.Step != "") {
				t.Fatalf("published pending progress for changed source: pushes=%d step=%q", source.pushes, publication.plan.Step)
			}
		})
	}
}
