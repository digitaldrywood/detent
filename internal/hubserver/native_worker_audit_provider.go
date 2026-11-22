package hubserver

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeAuditProviderPlan struct {
	event            *eventstore.Event
	observation      attachment.ArchiveReference
	predecessor      *runnersession.Record
	policyID         string
	requirement      providercapacity.Requirement
	primary          providercapacity.Requirement
	modelRevision    int64
	orgModelRevision int64
}

func (s *Service) prepareNativeAuditProvider(ctx context.Context, scope hubscope.NativeScope, leaseID tracker.LeaseID, phase *tracker.NativeProviderPhaseMutation) (*nativeAuditProviderPlan, error) {
	if phase == nil || phase.Requirement.Role != runner.RoleSecurityAudit && phase.Expected.Role != runner.RoleSecurityAudit {
		return nil, nil
	}
	var plan nativeAuditProviderPlan
	var revision policystore.Revision
	var settings projectstore.Settings
	var orgModels policystore.OrganizationModels
	var event eventstore.Event
	restore := phase.Requirement.Role != runner.RoleSecurityAudit
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		runner, err := r.RunnerForLease(ctx, string(leaseID))
		if err != nil {
			return err
		}
		if runner.RunnerID != scope.Credential.Runner.RunnerID {
			return tracker.ErrStaleFencingToken
		}
		pinned, err := r.LeasePolicy(ctx, leaseID)
		if err != nil {
			return err
		}
		if pinned.ProjectID != scope.Project {
			return tracker.ErrStaleFencingToken
		}
		plan.policyID = pinned.PolicyID
		if restore {
			lease, err := r.Lease(ctx, string(leaseID))
			if err != nil {
				return err
			}
			attempt, err := r.LatestAttempt(ctx, lease.IssueID)
			if err != nil {
				return err
			}
			event, err = r.LatestRunEvent(ctx, scope.Project, attempt.ID)
			return err
		}
		revision, err = r.ProjectPolicyRevision(ctx, scope.Project, pinned.PolicyID)
		if err != nil {
			return err
		}
		project, err := r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		settings = project.Settings
		plan.observation, err = landingAuditRouteObservation(ctx, r, scope)
		if err != nil {
			return err
		}
		if phase.AuditPredecessor > 0 {
			reader, ok := r.(runnersession.Reader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			session, err := reader.RunnerSession(ctx, scope.Project, phase.AuditPredecessor)
			if err != nil {
				return err
			}
			plan.predecessor = &session
		}
		orgModels, err = r.OrganizationModels(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if restore {
		decoded, err := eventstore.NewContent(s.runDetailStore()).Event(ctx, event)
		if err != nil {
			return nil, err
		}
		data := decoded.Data.Run
		if data == nil || data.Identity == nil || data.LeaseID != leaseID || data.Sequence != phase.Sequence {
			return nil, tracker.ErrStaleFencingToken
		}
		identity := data.Identity
		if identity.Role != runner.RoleMerge && identity.Role != runner.RoleCode && identity.Role != runner.RoleRework {
			return nil, hubapi.PolicyMismatch("The audit can only restore the existing landing attempt's provider")
		}
		plan.requirement = providercapacity.Requirement{Role: identity.Role, Backend: identity.Backend, Model: identity.Model}
		plan.event = &event
		return &plan, nil
	}

	approval, err := s.decodeTenantProjectPolicy(ctx, scope.Organization, scope.Project, revision)
	if err != nil {
		return nil, err
	}
	workflow, err := hubrecords.ProjectPolicyWorkflow(ctx, string(scope.Organization)+"/"+string(scope.Project), approval.Policy)
	if err != nil {
		return nil, err
	}
	models, err := s.projectRecordModel(ctx, settings, orgModels)
	if err != nil {
		return nil, err
	}
	workflow.Config.Agents.ModelSelection = models.Effective
	var route *policy.AuditRoute
	var backendKinds map[string]string
	if plan.observation.Key != "" {
		var observed runnerauth.ProjectConfiguration
		if err := s.readConfigurationPayload(ctx, scope.Organization, plan.observation, &observed); err != nil {
			return nil, err
		}
		if observed.ProjectID == string(scope.Project) && observed.RuntimeRegistered &&
			observed.EffectivePolicy != nil && observed.EffectivePolicy.Match(approval.Policy) == nil {
			route, backendKinds = observed.LandingAuditRoute, observed.LandingAuditBackendKinds
		}
	}
	primary, err := workflow.Config.LandingAuditIdentity(route, backendKinds, false)
	if err != nil {
		return nil, hubapi.PolicyMismatch("The configured audit backend kind has not been observed under the pinned policy")
	}
	if primary.Backend == "" {
		return nil, hubapi.PolicyMismatch("The runner's configured audit route has not been observed under the pinned policy")
	}
	plan.primary = providercapacity.Requirement{Role: runner.RoleSecurityAudit, Backend: primary.Backend, Model: cmp.Or(primary.Model, "provider_default")}
	plan.requirement = plan.primary
	if plan.predecessor != nil {
		snapshot, err := s.readRunnerSessionPayload(ctx, *plan.predecessor)
		if err != nil {
			return nil, err
		}
		if snapshot.AuditPredecessor == nil || *snapshot.AuditPredecessor != 0 {
			return nil, hubapi.PolicyMismatch("Audit verification requires a primary audit session admitted under this attempt")
		}
		if err := s.verifyRunnerSessionFinalization(ctx, scope, *plan.predecessor, snapshot); err != nil {
			if errors.Is(err, runnersession.ErrFinalizationIncomplete) {
				return nil, hubapi.PolicyMismatch("Audit verification requires a finalized primary audit")
			}
			return nil, err
		}
		if plan.primary.Model != "provider_default" {
			for _, observed := range snapshot.FinalizedModels() {
				if observed != "" && observed != plan.primary.Model && !slices.Contains(snapshot.AuditModelAliases, observed) {
					return nil, hubapi.PolicyMismatch("The primary audit used a different model than reserved")
				}
			}
		}
		plan.requirement.Model = models.Effective.Model("complex")
	}
	plan.modelRevision, plan.orgModelRevision = settings.ModelRevision, orgModels.Revision
	return &plan, nil
}

func (p *nativeAuditProviderPlan) authorize(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, attempt leasestore.Attempt, pinned policystore.LeasePolicy, phase tracker.NativeProviderPhaseMutation) error {
	if pinned.PolicyID != p.policyID || phase.Requirement != p.requirement || p.requirement.Validate() != nil || phase.AuditPredecessor < 0 {
		return hubapi.PolicyMismatch("Provider phase differs from the authorized audit or landing identity")
	}
	if p.event != nil {
		current, err := w.LatestRunEvent(ctx, scope.Project, attempt.ID)
		if err != nil {
			return err
		}
		if phase.AuditPredecessor != 0 || !reflect.DeepEqual(current, *p.event) {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	project, err := w.ProjectRecord(ctx, scope.Project)
	if err != nil {
		return err
	}
	if project.Settings.ModelRevision != p.modelRevision {
		return corestore.ErrRevisionConflict
	}
	if err := w.RequireOrganizationModels(ctx, p.orgModelRevision); err != nil {
		return err
	}
	observation, err := landingAuditRouteObservation(ctx, w, scope)
	if err != nil {
		return err
	}
	if observation != p.observation {
		return corestore.ErrRevisionConflict
	}
	if p.predecessor == nil {
		return nil
	}
	reader, ok := w.(runnersession.Reader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	session, err := reader.RunnerSession(ctx, scope.Project, phase.AuditPredecessor)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(session, *p.predecessor) {
		return corestore.ErrRevisionConflict
	}
	if session.AttemptID != attempt.ID || session.PolicyID != pinned.PolicyID || session.CompletedAt == nil ||
		session.FinalState != "completed" || session.Role != p.primary.Role || session.BackendID != p.primary.Backend ||
		cmp.Or(session.Model, "provider_default") != p.primary.Model {
		return hubapi.PolicyMismatch("Audit verification requires a completed primary audit on this attempt")
	}
	return nil
}

func landingAuditRouteObservation(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope) (attachment.ArchiveReference, error) {
	owner, ok := r.(postgres.ConfigurationReader)
	if !ok {
		return attachment.ArchiveReference{}, hubapi.ErrProjectServiceUnavailable
	}
	command, err := owner.ConfigurationCommand(ctx, scope.Credential.Runner.RunnerID, scope.Project)
	if errors.Is(err, corestore.ErrNotFound) {
		return attachment.ArchiveReference{}, nil
	}
	return command.Observation, err
}
