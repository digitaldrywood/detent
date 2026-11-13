package hubserver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func independentRepairRequest(failure failureattribution.Evidence, source string) tracker.CreateIssue {
	diagnostic := failure.Diagnostic
	fingerprint := issueorigin.DefectFingerprint(diagnostic)
	if fingerprint == "" {
		fingerprint = issueorigin.Fingerprint(failure.Command + "\n" + diagnostic)
	}
	body := fmt.Sprintf("## Acceptance criteria\n\nRepair the source failure reproduced on base `%s` by `%s` and add a focused regression.\n\n```text\n%s\n```\n\n## Must not break\n\nPreserve existing intended behavior, source verification and validation.\n\n## How we know it worked\n\nReproduce with the recorded command on the implementation-start base, then verify the regression and the recorded command pass.\n\nObserved change: %s\nHead: %s\nHead output digest: %s\nBase output digest: %s\n\n## Invariants touched\n\nPreserve the project invariants, source verification, and this report's fingerprint and origin. Repair the cause without adding a protection mechanism.\n\n## Safety regression\n\nPreserve existing safety regression tables and boundary seeds when the repair touches a safety-critical path.", failure.BaseSHA, failure.Command, diagnostic, source, failure.HeadSHA, failure.HeadOutputDigest, failure.BaseOutputDigest)
	body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "worker", Instance: "outcome", Source: source, Fingerprint: fingerprint})
	return tracker.CreateIssue{Title: "Repair independent validation failure", Body: body, Type: "bug", State: "Todo", Priority: new(1)}
}

type independentRepair struct {
	record      scheduledAttributionRecord
	priority    *scheduledAttributionRecord
	selection   postgres.DoctorProposalIdentityResult
	target      corestore.WorkItem
	policy      policystore.Revision
	policyFound bool
}

type preparedIndependentRepairs struct {
	source       hubrecords.WorkItemRecordPlan
	repairs      []independentRepair
	dependencies []tracker.NativeWorkItemID
	refusal      string
	update       *scheduledAttributionRecord
}

func independentRunFailures(issue tracker.NativeIssue, data tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint) []failureattribution.Evidence {
	if data.Outcome != "failed" || data.Runtime == nil || data.Runtime.Validation == nil || data.Runtime.Landing != nil || data.Runtime.Validation.ExitCode == 0 || data.Runtime.Validation.BaseReproduction == nil || issue.Archived || issue.Terminal {
		return nil
	}
	command := data.Runtime.Validation
	if checkpoint == nil || checkpoint.HeadSHA != command.HeadSHA || checkpoint.UncertainForgeEffect() {
		return nil
	}
	result := &gate.ValidatorResult{Verdict: gate.ValidatorVerdictRework, BaseSHA: command.BaseReproduction.HeadSHA, HeadSHA: command.HeadSHA, Commands: []gate.CommandResult{*command}}
	gate.NormalizeAttribution(result)
	return tracker.IndependentGateFailures(result)
}

func (s *Service) prepareIndependentRepairs(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, failures []failureattribution.Evidence, source string, now time.Time) (*preparedIndependentRepairs, error) {
	if len(failures) == 0 {
		return nil, nil
	}
	plan, err := s.loadWorkItemPlan(ctx, scope, string(item), "", nil)
	if err != nil {
		return nil, err
	}
	if plan.Issue.Archived || hubrecords.TenantProjectStateTerminal(plan.Project, plan.Issue.State) {
		return nil, nil
	}
	out := &preparedIndependentRepairs{source: plan}
	seen := map[string]bool{}
	unique := make([]failureattribution.Evidence, 0, min(len(failures), corestore.MaxPageSize))
	for _, failure := range failures {
		origin, _ := issueorigin.Parse(independentRepairRequest(failure, source).Body)
		if !seen[origin.Fingerprint] {
			seen[origin.Fingerprint] = true
			unique = append(unique, failure)
		}
	}
	if len(unique) > corestore.MaxPageSize {
		out.refusal = "Independent repair batch exceeds the request limit"
		return out, nil
	}
	nextNumber := 0
	for _, failure := range unique {
		request := independentRepairRequest(failure, source)
		origin, _ := issueorigin.Parse(request.Body)
		var selection postgres.DoctorProposalIdentityResult
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
				return err
			}
			var err error
			selection, err = r.LookupDoctorProposalIdentity(ctx, postgres.DoctorProposalIdentitySelection{ProjectID: scope.Project, Identity: postgres.DoctorProposalIdentity{Kind: "fingerprint", Value: origin.Fingerprint}})
			return err
		})
		if errors.Is(err, postgres.ErrDoctorProposalIdentitiesPending) {
			out.repairs, out.dependencies = nil, nil
			out.refusal = "Canonical repair identity coverage is pending"
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var repairPlan hubrecords.WorkItemRecordPlan
		var original corestore.WorkItem
		var priority *scheduledAttributionRecord
		if selection.WorkItemID != "" {
			repairPlan, err = s.loadWorkItemPlan(ctx, scope, string(selection.WorkItemID), "", nil)
			if err != nil {
				return nil, err
			}
			existing, stamped := issueorigin.Parse(repairPlan.Issue.Body)
			if !stamped || existing.Fingerprint != origin.Fingerprint || repairPlan.Current.Revision != selection.Revision {
				return nil, corestore.ErrSerializationFailure
			}
			original = repairPlan.Current
			repairPlan.Issue.Terminal = selection.Terminal
			if repairPlan.Issue.WorkItemID == item {
				out.refusal = "Dependencies cannot form a cycle"
			} else if repairPlan.Issue.Terminal && (repairPlan.Issue.State == "Cancelled" || strings.Contains(strings.ToLower(repairPlan.Issue.Body), "detent:no-dispatch")) {
				out.refusal = "Repair owner is terminal and cannot accept new work"
			} else if !repairPlan.Issue.Terminal {
				if request.Priority != nil && (repairPlan.Issue.Priority == nil || *request.Priority < *repairPlan.Issue.Priority) {
					planned, err := s.prepareWorkItemMachinePriority(ctx, scope, repairPlan, request.Priority)
					if err != nil {
						return nil, err
					}
					priority = &scheduledAttributionRecord{plan: planned.plan, fields: planned.fields, version: planned.version, event: planned.event, search: planned.document}
				}
				repairPlan, err = s.planWorkItemRecord(ctx, scope, repairPlan, tracker.CreateComment{Body: issueorigin.Occurrence(request.Body)})
				if err != nil {
					return nil, err
				}
			} else {
				original = corestore.WorkItem{}
			}
		}
		var policy policystore.Revision
		var policyFound bool
		if original.NativeID == "" {
			repairPlan, err = s.loadWorkItemPlan(ctx, scope, "", "", request)
			if err != nil {
				return nil, err
			}
			if nextNumber == 0 {
				nextNumber = repairPlan.Issue.Number
			}
			repairPlan.Issue.Number = nextNumber
			nextNumber++
			repairPlan, err = s.planWorkItemRecord(ctx, scope, repairPlan, request)
			if err != nil {
				return nil, err
			}
			repairPlan.Current.NativeID = repairPlan.Issue.WorkItemID
			policy, policyFound, err = s.validateWorkItemRecordFilingTenant(ctx, scope, repairPlan)
			if err != nil {
				return nil, err
			}
		}
		repair := independentRepair{priority: priority, selection: selection, target: original, policy: policy, policyFound: policyFound}
		if out.refusal == "" {
			repair.record, err = s.prepareScheduledAttributionRecord(ctx, scope, repairPlan, tracker.CollaborationData{})
			if err != nil {
				return nil, err
			}
			out.dependencies = append(out.dependencies, repairPlan.Issue.WorkItemID)
		}
		out.repairs = append(out.repairs, repair)
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		refusal, err := s.previewIndependentRepairs(ctx, r, scope, out)
		if err == nil && refusal != "" {
			out.refusal = refusal
		}
		return err
	}); err != nil {
		return nil, err
	}
	if out.refusal != "" {
		out.dependencies = nil
		return out, nil
	}
	slices.Sort(out.dependencies)
	update := plan
	update.Issue.Dependencies = slices.Clone(plan.Issue.Dependencies)
	for _, dependency := range out.dependencies {
		if !slices.Contains(update.Issue.Dependencies, dependency) {
			update.Issue.Dependencies = append(update.Issue.Dependencies, dependency)
		}
	}
	slices.Sort(update.Issue.Dependencies)
	update.Issue.Revision++
	update.Issue.UpdatedAt, update.Issue.LastActivityAt = now, now
	update.Fields, update.EventType = []string{"dependencies"}, "dependency.changed"
	data := tracker.CollaborationData{Operation: "add"}
	prepared, err := s.prepareScheduledAttributionRecord(ctx, scope, update, data)
	if err != nil {
		return nil, err
	}
	out.update = &prepared
	return out, nil
}

func (s *Service) previewIndependentRepairs(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, plan *preparedIndependentRepairs) (string, error) {
	cycles, ok := r.(corestore.DependencyCycleReader)
	if !ok {
		return "", hubapi.NativeRunEventStorageUnavailable()
	}
	if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
		return "", err
	}
	project, err := r.ProjectRecord(ctx, scope.Project)
	if err != nil {
		return "", err
	}
	if project.Core.DeletedAt != nil || !hubrecords.SameWorkItemProjectPlan(plan.source.Project, project) {
		return "", corestore.ErrSerializationFailure
	}
	current, err := r.WorkItem(ctx, scope.Project, plan.source.Current.NativeID)
	if err != nil {
		return "", err
	}
	if current.Revision != plan.source.Current.Revision || current.Archived || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
		return "", corestore.ErrSerializationFailure
	}
	refusal := plan.refusal
	for _, repair := range plan.repairs {
		if repair.target.NativeID == "" {
			continue
		}
		target, err := r.WorkItem(ctx, scope.Project, repair.target.NativeID)
		if err != nil {
			return "", err
		}
		if !reflect.DeepEqual(target, repair.target) {
			return "", corestore.ErrSerializationFailure
		}
		cycle, err := cycles.DependencyCreatesCycle(ctx, corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: current.NativeID, RelatedProjectID: scope.Project, RelatedWorkItemID: target.NativeID, Type: "dependency"})
		if err != nil {
			return "", err
		}
		if cycle {
			refusal = "Dependencies cannot form a cycle"
		}
	}
	return refusal, nil
}

func (s *Service) checkIndependentRepairs(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, plan *preparedIndependentRepairs) (string, error) {
	if plan == nil {
		return "", nil
	}
	if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
		return "", err
	}
	if err := w.LockRelationshipGraph(ctx); err != nil {
		return "", err
	}
	if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
		return "", err
	}
	project, err := w.ProjectRecord(ctx, scope.Project)
	if err != nil {
		return "", err
	}
	if project.Core.DeletedAt != nil || !hubrecords.SameWorkItemProjectPlan(plan.source.Project, project) {
		return "", corestore.ErrSerializationFailure
	}
	current, err := w.LockWorkItem(ctx, scope.Project, plan.source.Current.NativeID)
	if err != nil {
		return "", err
	}
	if current.Revision != plan.source.Current.Revision || current.Archived || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
		return "", corestore.ErrSerializationFailure
	}
	refusal := plan.refusal
	for _, repair := range plan.repairs {
		if err := w.ValidateDoctorProposalIdentitySelection(ctx, repair.selection); err != nil {
			return "", err
		}
		if repair.target.NativeID == "" {
			continue
		}
		target, err := w.LockWorkItem(ctx, scope.Project, repair.target.NativeID)
		if err != nil {
			return "", err
		}
		if !reflect.DeepEqual(target, repair.target) {
			return "", corestore.ErrSerializationFailure
		}
		if target.NativeID == current.NativeID {
			refusal = "Dependencies cannot form a cycle"
			continue
		}
		edge := corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: current.NativeID, RelatedProjectID: scope.Project, RelatedWorkItemID: target.NativeID, Type: "dependency"}
		if err := w.CheckDependency(ctx, edge); errors.Is(err, corestore.ErrDependencyCycle) {
			refusal = "Dependencies cannot form a cycle"
		} else if err != nil {
			return "", err
		}
	}
	return refusal, nil
}

func (s *Service) applyIndependentRepairs(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, plan *preparedIndependentRepairs) error {
	if plan == nil || plan.refusal != "" {
		return nil
	}
	for _, repair := range plan.repairs {
		p := repair.record
		if p.plan.Create && p.plan.Comment.ID == "" {
			current, err := w.ProjectPolicy(ctx, scope.Project)
			if errors.Is(err, corestore.ErrNotFound) && !repair.policyFound {
				err = nil
			} else if err == nil && (!repair.policyFound || !hubrecords.SameNativeClaimProjectPolicy(current, repair.policy)) {
				return corestore.ErrSerializationFailure
			}
			if err != nil {
				return err
			}
		}
		if err := s.applyIndependentRecord(ctx, w, scope, p); err != nil {
			return err
		}
		if repair.priority != nil {
			if err := s.applyIndependentRecord(ctx, w, scope, *repair.priority); err != nil {
				return err
			}
		}
		edge := corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.source.Current.NativeID, RelatedProjectID: scope.Project, RelatedWorkItemID: p.plan.Issue.WorkItemID, Type: "dependency"}
		if err := w.SetRelationship(ctx, edge, "add", p.plan.Issue.UpdatedAt); err != nil {
			return err
		}
	}
	if plan.update != nil {
		return s.applyIndependentRecord(ctx, w, scope, *plan.update)
	}
	return nil
}

func (s *Service) applyIndependentRecord(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p scheduledAttributionRecord) error {
	if err := hubrecords.WriteWorkItemRecord(ctx, w, p.plan, p.fields); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, p.version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, p.event); err != nil {
		return err
	}
	expected := p.plan.Current.Revision
	if p.plan.Comment.ID != "" {
		expected = 0
	}
	if err := w.PutSearch(ctx, p.search, expected); err != nil {
		return err
	}
	if p.plan.Comment.ID == "" {
		return w.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: p.version.WorkItemID, Revision: p.version.Revision, VersionSHA256: p.version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(p.plan.Issue.Body)})
	}
	return nil
}
