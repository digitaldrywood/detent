package web_test

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/web"
)

func newServerWithLaneWriter(cfg web.Config, deps web.Dependencies) (*web.Server, error) {
	previous := deps.OperatorMoves
	deps.OperatorMoves = testLaneWriter{deps: deps, previous: previous}
	return web.NewServer(cfg, deps)
}

type testLaneWriter struct {
	deps     web.Dependencies
	previous web.OperatorMoveReconciler
}

func (w testLaneWriter) ReconcileOperatorMove(ctx context.Context, request orchestrator.OperatorMoveRequest) (orchestrator.OperatorMoveResult, error) {
	if !request.WriteTracker {
		if w.previous != nil {
			return w.previous.ReconcileOperatorMove(ctx, request)
		}
		return orchestrator.OperatorMoveResult{}, nil
	}
	tracker := w.deps.Connector
	if w.deps.Registry != nil {
		if selected, ok := w.deps.Registry.Get(project.ID(request.ProjectID)); ok {
			tracker = selected.Connector()
		}
	}
	var err error
	if request.StateFieldID > 0 {
		setter, ok := tracker.(connector.IssueFieldSetter)
		if !ok {
			return orchestrator.OperatorMoveResult{}, connector.ErrNotImplemented
		}
		err = setter.SetIssueField(ctx, request.IssueID, request.StateFieldID, request.StateFieldValue)
	} else {
		err = tracker.UpdateIssueState(ctx, request.IssueID, request.ToState)
	}
	if err == nil && strings.EqualFold(request.ToState, "Done") && w.deps.Hub != nil {
		snapshot, _ := w.deps.Hub.Latest()
		for _, issue := range append(snapshot.BoardIssues, snapshot.Pipeline...) {
			if issue.ID == request.IssueID && issue.PullRequest != nil && strings.EqualFold(issue.PullRequest.State, "merged") {
				if closer, ok := tracker.(connector.IssueCloser); ok {
					err = closer.CloseIssue(ctx, issue.ID)
				}
			}
		}
	}
	return orchestrator.OperatorMoveResult{}, err
}
