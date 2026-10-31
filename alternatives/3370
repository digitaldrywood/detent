package web

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeIssueExplainer struct {
	server   *Server
	fallback operatortool.Explainer
}

func (r nativeIssueExplainer) Explain(ctx context.Context, query explain.Query) (explain.IssueExplanation, error) {
	s := r.server
	if s.registry != nil {
		if tracked, ok := s.registry.Get(project.ID(query.ProjectID)); ok {
			if provider, ok := tracked.Connector().(nativeClientSource); ok && provider.NativeClient() != nil {
				var err error
				ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: query.ProjectID})
				if err != nil {
					return explain.IssueExplanation{}, err
				}
				client := provider.NativeClient()
				id := tracker.NativeWorkItemID(query.Reference)
				if !strings.HasPrefix(query.Reference, "wi_") {
					snapshot, err := operatortool.ProjectSnapshot(ctx, s.chatSnapshot(ctx))
					if err != nil {
						return explain.IssueExplanation{}, err
					}
					selected, err := explain.ResolveSnapshotIssue(snapshot, query, explain.SnapshotIssueScope{IncludeCompleted: true})
					if err != nil {
						return explain.IssueExplanation{}, err
					}
					id = tracker.NativeWorkItemID(selected.Issue.ID)
				}
				var admission []tracker.NativeAdmissionContext
				if client.HasRegisteredRunner() {
					if orch := tracked.Orchestrator(); orch != nil {
						current, observed := orch.NativeAdmissionContext()
						if observed && current.PolicyID != "" {
							admission = append(admission, current)
						}
					}
				}
				evidence, err := client.RuntimeEvidence(ctx, id, "", admission...)
				if err != nil {
					return explain.IssueExplanation{}, nativeWorkReadError(err)
				}
				if evidence.Issue.ProjectID != client.ProjectID() || evidence.Issue.WorkItemID != id {
					return explain.IssueExplanation{}, operatortool.ErrAccessDenied
				}
				evidence.Issue, err = s.projectNativeWork(ctx, client, evidence.Issue)
				if err != nil {
					return explain.IssueExplanation{}, err
				}
				result := explain.FromNativeEvidence(evidence)
				result.Identity.ProjectID = query.ProjectID
				return result, nil
			}
		}
	}
	if r.fallback == nil {
		return explain.IssueExplanation{}, operatortool.ErrReadUnavailable
	}
	return r.fallback.Explain(ctx, query)
}
