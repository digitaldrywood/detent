package hubserver

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readHealthContractProject(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID) (projectstore.Record, policy.Descriptor, error) {
	var record projectstore.Record
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		record, err = r.ProjectRecord(ctx, project)
		if err != nil {
			return err
		}
		revision, err = r.ProjectPolicy(ctx, project)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) && record.Core.ID != "" {
		return record, policy.Descriptor{}, nil
	}
	if err != nil {
		return record, policy.Descriptor{}, err
	}
	descriptor, err := eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, revision)
	return record, descriptor, err
}

func (c *healthContractCache) readRecords(ctx context.Context, s *Service, project projectstore.Record, item tracker.NativeWorkItemID, revision tracker.Revision, descriptor policy.Descriptor) (*healthWait, error) {
	key := healthContractKey{organization: project.Core.OrganizationID, project: project.Core.ID, item: string(item)}
	if c != nil {
		c.mu.Lock()
		cached, found := c.entries[key]
		c.mu.Unlock()
		if found && cached.revision == revision && cached.policyID == descriptor.ID {
			return cached.wait, nil
		}
	}
	scope := nativeScope{organization: project.Core.OrganizationID, project: project.Core.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	row, version, err := s.readWorkItemRecord(ctx, scope, string(item))
	if err != nil {
		return nil, err
	}
	if tracker.Revision(row.Revision) != revision {
		return nil, corestore.ErrRevisionConflict
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return nil, err
	}
	wait, err := s.evaluateHealthContractRecord(ctx, scope, project, descriptor, issue)
	if err != nil {
		return nil, err
	}
	if c == nil || issue.LinkedSource != nil || issue.IssueContract != nil && issue.IssueContract.ReleaseDelivery != nil {
		return wait, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[healthContractKey]healthContractResult{}
	}
	if _, exists := c.entries[key]; !exists {
		if len(c.order) == healthReadLimit {
			delete(c.entries, c.order[c.oldest])
			c.order[c.oldest] = key
			c.oldest = (c.oldest + 1) % healthReadLimit
		} else {
			c.order = append(c.order, key)
		}
	}
	c.entries[key] = healthContractResult{revision: revision, policyID: descriptor.ID, wait: wait}
	return wait, nil
}

func (s *Service) evaluateHealthContractRecord(ctx context.Context, scope nativeScope, project projectstore.Record, descriptor policy.Descriptor, issue tracker.NativeIssue) (*healthWait, error) {
	if strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") || issue.LinkedSource != nil && issue.LinkedSource.Status != "complete" || strings.EqualFold(issue.State, "Triage") || issue.IssueContract != nil && issue.IssueContract.Exempt {
		return nil, nil
	}
	workflow := config.Workflow{Config: config.Default()}
	if descriptor.ID != "" {
		var err error
		workflow, err = config.ApplyNativePolicy(workflow, descriptor)
		if err != nil {
			return nil, err
		}
	}
	target := workflow.Config.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !strings.EqualFold(issue.State, target) {
		return nil, nil
	}
	contract, err := config.ResolvePolicyIssueContract(descriptor)
	if err != nil {
		return nil, err
	}
	wait := &healthWait{Item: string(issue.WorkItemID), Project: string(scope.project), Since: issue.UpdatedAt, Active: true}
	if valid, err := s.healthReleaseContractCurrent(ctx, scope, project, descriptor, issue, contract); err != nil {
		return nil, err
	} else if !valid {
		wait.Reason = "The verified release delivery contract changed; current approved policy and exact associated delivery are required"
		return wait, nil
	}
	evaluation := contract.Evaluate(nativeContractIssue(issue))
	if evaluation.Satisfied() {
		return nil, nil
	}
	var reasons []string
	if len(evaluation.Missing) > 0 {
		reasons = append(reasons, "Add or complete these required issue sections: "+strings.Join(evaluation.Missing, ", "))
	}
	if evaluation.NeedsConfirmation {
		reasons = append(reasons, "A human must confirm the success criteria by editing or saving the issue body")
	}
	wait.Reason = strings.Join(reasons, "; ")
	return wait, nil
}

func (s *Service) healthReleaseContractCurrent(ctx context.Context, scope nativeScope, project projectstore.Record, descriptor policy.Descriptor, issue tracker.NativeIssue, contract config.IssueContract) (bool, error) {
	if issue.IssueContract == nil || issue.IssueContract.ReleaseDelivery == nil {
		return true, nil
	}
	delegated := issue.IssueContract.ReleaseDelivery
	if descriptor.Authored == nil || descriptor.ID != delegated.PolicyID || policy.Digest([]byte(issue.Body)) != delegated.BodySHA256 || !releaseDeliverySectionsMatch(issue.Body, contract.ReleaseDeliveryBody()) || issue.IgnoreDependencies || !project.Core.RequireDependencies {
		return false, nil
	}
	valid := false
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Background, func(r tenantstore.Reader) error {
		delivery, err := r.ReleaseDelivery(ctx, scope.project, issue.WorkItemID)
		if err != nil {
			return err
		}
		facts, ok := r.(eventstore.ReleaseDeliveryFactsReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		dependency, err := facts.ReleaseDeliveryFacts(ctx, scope.project, issue.WorkItemID, delivery.ReleasePickInput)
		if err != nil {
			return err
		}
		if !dependency.DependsOnSource {
			return nil
		}
		source, err := r.WorkItem(ctx, scope.project, delivery.SourceIssueID)
		if err != nil {
			return err
		}
		terminal := false
		for _, state := range project.States {
			if state.Name == stringValue(source.WorkflowStateName) {
				terminal = state.Terminal
				break
			}
		}
		if !terminal {
			return nil
		}
		change, err := r.Change(ctx, delivery.SourceChangeID)
		if err != nil {
			return err
		}
		valid = change.ProjectID == scope.project && change.WorkItemID == delivery.SourceIssueID && change.CurrentVersionID != nil && change.LandedVersionID != nil && *change.CurrentVersionID == *change.LandedVersionID && change.LandedBaseRef != nil && *change.LandedBaseRef == "develop" && change.LandedMergeSHA != nil && *change.LandedMergeSHA == delivery.SourceMergeSHA
		return nil
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	return valid, err
}
