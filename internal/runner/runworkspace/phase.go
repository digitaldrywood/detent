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
	PreserveItemPhaseHistory(context.Context, workspace.Info, workspace.Issue, workspace.PhaseSource, func(context.Context) error) (string, error)
}

func PublishPhase(ctx context.Context, execution PhaseReceiptExecution, backend workspace.Backend, info workspace.Info, issue workspace.Issue, carry *tracker.NativePhaseContinuation, phase, input string) (workspace.PhaseSource, error) {
	publisher, ok := backend.(PhasePublisher)
	if !ok {
		return workspace.PhaseSource{}, errors.New("workspace cannot publish an item phase")
	}
	if carry != nil {
		if issue.PublicationRepository == "" {
			issue.PublicationRepository = carry.RepositoryURL
		}
		if issue.ExpectedPhaseRemoteHead == "" && info.PhaseRemoteHead == nil && carry.LastPushed != nil {
			info.PhaseRemoteHead = &carry.LastPushed.HeadSHA
		}
	}
	branch, head, tree, err := publisher.PushItemPhase(ctx, info, issue, execution.VerifyPhaseAuthority)
	if err != nil {
		return workspace.PhaseSource{}, err
	}
	if carry != nil && issue.ExpectedPhaseRemoteHead == "" {
		for _, receipt := range carry.Receipts {
			if receipt.Phase == phase && receipt.Branch == branch && receipt.HeadSHA == head && receipt.TreeSHA == tree {
				return workspace.PhaseSource{}, nil
			}
		}
	}
	if err := execution.RecordPhaseReceipt(ctx, phase, branch, head, tree, input); err != nil {
		return workspace.PhaseSource{}, err
	}
	return workspace.PhaseSource{Branch: branch, HeadSHA: head, TreeSHA: tree}, nil
}
