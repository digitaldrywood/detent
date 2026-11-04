package hubserver

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/policy"
)

func (s *Service) authorizeNativeWorkflowScope(ctx context.Context, reader accessstore.Reader, scope nativeScope) error {
	if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
		return err
	}
	if scope.credential.Hosted == nil {
		return nil
	}
	member, err := reader.AuthorizeAccess(ctx, s.accessAuthority(scope, true), s.config.now())
	if err != nil {
		return err
	}
	if member.UserID != "" && lesserHostedRole(member.Role, scope.credential.HostedRole) != scope.credential.HostedRole {
		return auth.ErrHostedIdentity
	}
	return nil
}

func validateNativeWorkflowPolicy(project projectstore.Record, descriptor policy.Descriptor, found bool) error {
	if project.Core.WorkflowSource == "" {
		return nil
	}
	if !found || descriptor.Workflow == nil || descriptor.Validate() != nil {
		return policyMismatch("The configured workflow requires its approved policy")
	}
	workflowRevision := descriptor.Workflow.Revision
	if workflowRevision == "" {
		workflowRevision = descriptor.SourceRevision
	}
	if descriptor.Workflow.Source != project.Core.WorkflowSource || workflowRevision != project.Core.WorkflowSourceRevision || !slices.EqualFunc(descriptor.Workflow.States, project.States, func(a, b policy.State) bool {
		return a.Name == b.Name && a.Terminal == b.Terminal && a.Dispatchable == b.Dispatchable && a.OperatorOnly == b.OperatorOnly && slices.Equal(a.Transitions, b.Transitions)
	}) {
		return policyMismatch("The configured workflow differs from its approved policy")
	}
	return nil
}
