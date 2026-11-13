package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) updateNativeDependencyReasonTenant(ctx context.Context, scope hubscope.NativeScope, anchor string, request tracker.UpdateDependencyReason) (json.RawMessage, error) {
	if !validDependencyReason(request.Text) || (request.Kind != "blocked" && request.Kind != "unblocks") || request.ExpectedRevision < 0 || request.ExpectedRevision == math.MaxInt64 {
		return nil, hubapi.NativePublicInvalid("Reason must be plain text of at most 140 characters and kind blocked or unblocks")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if s.config.AccessRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	operation := "dependency-reason/" + anchor
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return content.Receipt(ctx, receipt)
	}
	var pinned policystore.Revision
	var requirements policy.Requirements
	if scope.Credential.Scope == hubapi.APIScopeWorker && request.LeaseID != "" {
		pinned, requirements, err = s.dependencyReasonPolicy(ctx, scope, request.LeaseID)
		if err != nil {
			return nil, err
		}
	}
	row, version, err := s.readWorkItemRecord(ctx, scope, string(request.WorkItemID))
	if err != nil {
		return nil, err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return nil, err
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return loadDependencyReasonRecords(ctx, r, scope.Project, []*tracker.NativeIssue{&issue})
	}); err != nil {
		return nil, err
	}
	reason := issue.BlockedReason
	if request.Kind == "unblocks" {
		reason = issue.UnblocksReason
	}
	if reason.Revision != request.ExpectedRevision {
		return nil, hubapi.NativeConflict(tracker.Revision(reason.Revision))
	}
	result := tracker.DependencyReason{Text: request.Text, Basis: request.Basis, CurrentBasis: request.Basis, Revision: request.ExpectedRevision + 1}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			replay = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		ids := []tracker.NativeWorkItemID{tracker.NativeWorkItemID(anchor), request.WorkItemID}
		slices.Sort(ids)
		ids = slices.Compact(ids)
		var anchorRow, target corestore.WorkItem
		for _, id := range ids {
			current, err := w.LockWorkItem(ctx, scope.Project, id)
			if err != nil {
				return err
			}
			if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
				return corestore.ErrNotFound
			}
			if id == tracker.NativeWorkItemID(anchor) {
				anchorRow = current
			}
			if id == request.WorkItemID {
				target = current
			}
		}
		if target.ID != row.ID || target.Revision != row.Revision || target.EventSequence != row.EventSequence {
			return hubapi.NativeConflict(tracker.Revision(request.ExpectedRevision))
		}
		now := s.config.now().UTC()
		if scope.Credential.Scope == hubapi.APIScopeWorker {
			if err := s.authorizeDependencyReasonLease(ctx, w, scope, anchorRow, request.Mutation, pinned, requirements, now); err != nil {
				return err
			}
		}
		writer, ok := w.(corestore.DependencyReasonWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		edges, err := writer.DependencyReasonEdges(ctx, scope.Project, request.WorkItemID, request.Kind)
		if err != nil {
			return err
		}
		related := make([]string, 0, len(edges))
		for _, edge := range edges {
			if edge.ProjectID != scope.Project || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(edge.ActorPrincipalID)}) {
				return hubapi.NativeConflict(tracker.Revision(reason.Revision))
			}
			related = append(related, string(edge.WorkItemID))
		}
		if request.Basis != dependencyBasis(related) {
			return hubapi.NativeConflict(tracker.Revision(reason.Revision))
		}
		if s.database.plansService().Plans() != nil {
			if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", now)); err != nil {
				return err
			}
		}
		if err := writer.CompareAndSwapDependencyReason(ctx, corestore.DependencyReason{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: request.WorkItemID, Kind: request.Kind, Reason: result}, request.ExpectedRevision); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, prepared)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nil, hubapi.NativeConflict(tracker.Revision(request.ExpectedRevision))
	}
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}

func (s *Service) authorizeDependencyReasonLease(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, anchor corestore.WorkItem, command tracker.Mutation, pinned policystore.Revision, requirements policy.Requirements, now time.Time) error {
	credential, err := w.AccessCredential(ctx, scope.Credential.ID)
	if err != nil {
		return err
	}
	identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
	if err != nil {
		return err
	}
	var lease leasestore.Lease
	var assigned runnerstore.Identity
	if command.LeaseID != "" {
		assigned, err = w.RunnerForLease(ctx, string(command.LeaseID))
		if err != nil {
			return err
		}
		locker, ok := w.(leasestore.FencedLeaseLocker)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		lease, err = locker.LockLease(ctx, string(command.LeaseID))
		if err != nil {
			return err
		}
		now = s.config.now().UTC()
	}
	if scope.Credential.EntryKey != nil || identity.OrganizationID != scope.Organization || identity.RunnerID != scope.Credential.Runner.RunnerID || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) || identity.State == "disabled" || credential.Scope != "worker" || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, now) {
		return hubscope.RunnerUnauthorized()
	}
	allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return hubapi.NativeNotFound()
	}
	if command.LeaseID == "" {
		_, err := w.LatestAttempt(ctx, anchor.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return tracker.ErrStaleFencingToken
	}
	if lease.OrganizationID != scope.Organization || lease.IssueID != anchor.ID || lease.FencingToken != int64(command.FencingToken) || lease.ReleasedAt != nil || lease.RenewedAt.After(now) || !lease.ExpiresAt.After(now) {
		return tracker.ErrStaleFencingToken
	}
	if !runnerIdentityMatchesHeldLease(assigned, scope.Organization, identity.RunnerID, credential.ID, identity.MachineID, now) || !slices.Contains(identity.Operations, runnerauth.Claim) {
		return hubscope.RunnerUnauthorized()
	}
	currentPolicy, err := w.LeasePolicy(ctx, command.LeaseID)
	if err != nil {
		return err
	}
	revision, err := w.ProjectPolicyRevision(ctx, scope.Project, currentPolicy.PolicyID)
	if err != nil {
		return err
	}
	if currentPolicy.OrganizationID != scope.Organization || currentPolicy.ProjectID != scope.Project || currentPolicy.LeaseID != command.LeaseID || revision.OrganizationID != pinned.OrganizationID || revision.ProjectID != pinned.ProjectID || revision.PolicyID != pinned.PolicyID || revision.PayloadSHA256 != pinned.PayloadSHA256 || revision.PayloadObjectKey != pinned.PayloadObjectKey || revision.PayloadBytes != pinned.PayloadBytes || revision.PayloadContentBytes != pinned.PayloadContentBytes {
		return hubapi.PolicyMismatch("Lease policy changed during the request")
	}
	runner := runnerauth.Runner{Binding: runnerauth.Binding{RunnerID: identity.RunnerID, MachineID: identity.MachineID}, Operations: identity.Operations, Routing: runnerauth.Routing{Scope: identity.Scope, ProjectIDs: []tracker.ProjectID{scope.Project}, Tags: identity.Tags, State: identity.State}}
	return runnerExcluded(runner.Exclusions(scope.Project, requirements, true))
}

func (s *Service) dependencyReasonPolicy(ctx context.Context, scope hubscope.NativeScope, lease tracker.LeaseID) (policystore.Revision, policy.Requirements, error) {
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		pinned, err := r.LeasePolicy(ctx, lease)
		if err != nil {
			return err
		}
		if pinned.OrganizationID != scope.Organization || pinned.ProjectID != scope.Project || pinned.LeaseID != lease || pinned.PolicyID == "" {
			return hubapi.PolicyMismatch("Lease policy is outside the authorized project")
		}
		revision, err = r.ProjectPolicyRevision(ctx, scope.Project, pinned.PolicyID)
		if err != nil {
			return err
		}
		if revision.OrganizationID != scope.Organization || revision.ProjectID != scope.Project || revision.PolicyID != pinned.PolicyID {
			return hubapi.PolicyMismatch("Lease policy is outside the authorized project")
		}
		return nil
	})
	if err != nil {
		return revision, policy.Requirements{}, err
	}
	descriptor, err := eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, revision)
	return revision, descriptor.Requirements, err
}

func loadDependencyReasonRecords(ctx context.Context, r tenantstore.Reader, project tracker.ProjectID, issues []*tracker.NativeIssue) error {
	reader, ok := r.(corestore.DependencyReasonReader)
	if !ok {
		return nil
	}
	byID := map[tracker.NativeWorkItemID][]*tracker.NativeIssue{}
	ids := []tracker.NativeWorkItemID{}
	for _, issue := range issues {
		if _, ok := byID[issue.WorkItemID]; !ok {
			ids = append(ids, issue.WorkItemID)
		}
		byID[issue.WorkItemID] = append(byID[issue.WorkItemID], issue)
	}
	for start := 0; start < len(ids); start += corestore.MaxRelationshipBatch {
		records, err := reader.DependencyReasonRecords(ctx, project, ids[start:min(start+corestore.MaxRelationshipBatch, len(ids))])
		if err != nil {
			return err
		}
		for _, record := range records {
			for _, issue := range byID[record.WorkItemID] {
				switch record.Kind {
				case "blocked":
					issue.BlockedReason = record.Reason
				case "unblocks":
					issue.UnblocksReason = record.Reason
				}
			}
		}
	}
	return nil
}
