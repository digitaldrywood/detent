package runworkspace

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type PhaseReceiptExecution interface {
	PhaseReceipts(context.Context, string) (*tracker.NativePhaseContinuation, error)
	VerifyPhaseAuthority(context.Context) error
	RecordPhaseReceipt(context.Context, string, string, string, string, string) error
}

type PhasePublisher interface {
	PushItemPhase(context.Context, workspace.Info, workspace.Issue, func(context.Context) error) (string, string, string, error)
	ItemPhaseHead(context.Context, workspace.Info, workspace.Issue) (string, string, error)
	PreserveItemPhaseHistory(context.Context, workspace.Info, workspace.Issue, tracker.NativePhaseReceipt, func(context.Context) error) error
}

func PublishPhase(ctx context.Context, execution PhaseReceiptExecution, backend workspace.Backend, info workspace.Info, issue workspace.Issue, carry *tracker.NativePhaseContinuation, phase, input string) error {
	publisher, ok := backend.(PhasePublisher)
	if !ok {
		return errors.New("workspace cannot publish an item phase")
	}
	if carry != nil {
		for _, receipt := range carry.Receipts {
			if receipt.Phase != phase {
				continue
			}
			head, tree, err := publisher.ItemPhaseHead(ctx, info, issue)
			if err != nil {
				return err
			}
			if receipt.HeadSHA == head && receipt.TreeSHA == tree {
				return nil
			}
		}
	}
	branch, head, tree, err := publisher.PushItemPhase(ctx, info, issue, execution.VerifyPhaseAuthority)
	if err != nil {
		return err
	}
	return execution.RecordPhaseReceipt(ctx, phase, branch, head, tree, input)
}
