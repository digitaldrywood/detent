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
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) updateNativeDependencyReasonTenant(ctx context.Context, scope nativeScope, anchor string, request tracker.UpdateDependencyReason) (json.RawMessage, error) {
	if !validDependencyReason(request.Text) || (request.Kind != "blocked" && request.Kind != "unblocks") || request.ExpectedRevision < 0 || request.ExpectedRevision == math.MaxInt64 {
		return nil, nativePublicInvalid("Reason must be plain text of at most 140 characters and kind blocked or unblocks")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if s.config.AccessRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	operation := "dependency-reason/" + anchor
	hash, err := nativeCommandHash(request)
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
	if scope.credential.Scope == apiScopeWorker && request.LeaseID != "" {
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
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return loadDependencyReasonRecords(ctx, r, scope.project, []*tracker.NativeIssue{&issue})
	}); err != nil {
		return nil, err
	}
	reason := issue.BlockedReason
	if request.Kind == "unblocks" {
		reason = issue.UnblocksReason
	}
	if reason.Revision != request.ExpectedRevision {
		return nil, nativeConflict(tracker.Revision(reason.Revision))
	}
	result := tracker.DependencyReason{Text: request.Text, Basis: request.Basis, CurrentBasis: request.Basis, Revision: request.ExpectedRevision + 1}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return nativeConflict(0)
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
			current, err := w.LockWorkItem(ctx, scope.project, id)
			if err != nil {
				return err
			}
			if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(current.ActorPrincipalID)}) {
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
			return nativeConflict(tracker.Revision(request.ExpectedRevision))
		}
		now := s.config.now().UTC()
		if scope.credential.Scope == apiScopeWorker {
			if err := s.authorizeDependencyReasonLease(ctx, w, scope, anchorRow, request.Mutation, pinned, requirements, now); err != nil {
				return err
			}
		}
		writer, ok := w.(corestore.DependencyReasonWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		edges, err := writer.DependencyReasonEdges(ctx, scope.project, request.WorkItemID, request.Kind)
		if err != nil {
			return err
		}
		related := make([]string, 0, len(edges))
		for _, edge := range edges {
			if edge.ProjectID != scope.project || !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(edge.ActorPrincipalID)}) {
				return nativeConflict(tracker.Revision(reason.Revision))
			}
			related = append(related, string(edge.WorkItemID))
		}
		if request.Basis != dependencyBasis(related) {
			return nativeConflict(tracker.Revision(reason.Revision))
		}
		if s.database.plans() != nil {
			if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", now); err != nil {
				return err
			}
		}
		if err := writer.CompareAndSwapDependencyReason(ctx, corestore.DependencyReason{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: request.WorkItemID, Kind: request.Kind, Reason: result}, request.ExpectedRevision); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, prepared)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nil, nativeConflict(tracker.Revision(request.ExpectedRevision))
	}
	if err != nil {
		return nil, projectRecordError(err)
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}

func (s *Service) authorizeDependencyReasonLease(ctx context.Context, w tenantstore.Writer, scope nativeScope, anchor corestore.WorkItem, command tracker.Mutation, pinned policystore.Revision, requirements policy.Requirements, now time.Time) error {
	credential, err := w.AccessCredential(ctx, scope.credential.ID)
	if err != nil {
		return err
	}
	identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
	if err != nil {
		return err
	}
	if scope.credential.EntryKey != nil || identity.OrganizationID != scope.organization || identity.RunnerID != scope.credential.Runner.RunnerID || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) || identity.State == "disabled" || credential.Scope != "worker" || credential.Kind != "service" || !credential.Valid(scope.credential.Hash, now) {
		return runnerUnauthorized()
	}
	allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return nativeNotFound()
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
	locker, ok := w.(leasestore.FencedLeaseLocker)
	if !ok {
		return workItemStorageUnavailable()
	}
	lease, err := locker.LockLease(ctx, string(command.LeaseID))
	if err != nil {
		return err
	}
	if lease.OrganizationID != scope.organization || lease.IssueID != anchor.ID || lease.FencingToken != int64(command.FencingToken) || lease.ReleasedAt != nil || lease.RenewedAt.After(now) || !lease.ExpiresAt.After(now) {
		return tracker.ErrStaleFencingToken
	}
	assigned, err := w.RunnerForLease(ctx, lease.LeaseID)
	if err != nil {
		return err
	}
	if !runnerIdentityMatchesHeldLease(assigned, scope.organization, identity.RunnerID, credential.ID, identity.MachineID, now) || !slices.Contains(identity.Operations, runnerauth.Claim) {
		return runnerUnauthorized()
	}
	currentPolicy, err := w.LeasePolicy(ctx, command.LeaseID)
	if err != nil {
		return err
	}
	revision, err := w.ProjectPolicyRevision(ctx, scope.project, currentPolicy.PolicyID)
	if err != nil {
		return err
	}
	if currentPolicy.OrganizationID != scope.organization || currentPolicy.ProjectID != scope.project || currentPolicy.LeaseID != command.LeaseID || revision.OrganizationID != pinned.OrganizationID || revision.ProjectID != pinned.ProjectID || revision.PolicyID != pinned.PolicyID || revision.PayloadSHA256 != pinned.PayloadSHA256 || revision.PayloadObjectKey != pinned.PayloadObjectKey || revision.PayloadBytes != pinned.PayloadBytes || revision.PayloadContentBytes != pinned.PayloadContentBytes {
		return policyMismatch("Lease policy changed during the request")
	}
	runner := runnerauth.Runner{Binding: runnerauth.Binding{RunnerID: identity.RunnerID, MachineID: identity.MachineID}, Operations: identity.Operations, Routing: runnerauth.Routing{Scope: identity.Scope, ProjectIDs: []tracker.ProjectID{scope.project}, Tags: identity.Tags, State: identity.State}}
	return runnerExcluded(runner.Exclusions(scope.project, requirements, true))
}

func (s *Service) dependencyReasonPolicy(ctx context.Context, scope nativeScope, lease tracker.LeaseID) (policystore.Revision, policy.Requirements, error) {
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		pinned, err := r.LeasePolicy(ctx, lease)
		if err != nil {
			return err
		}
		if pinned.OrganizationID != scope.organization || pinned.ProjectID != scope.project || pinned.LeaseID != lease || pinned.PolicyID == "" {
			return policyMismatch("Lease policy is outside the authorized project")
		}
		revision, err = r.ProjectPolicyRevision(ctx, scope.project, pinned.PolicyID)
		if err != nil {
			return err
		}
		if revision.OrganizationID != scope.organization || revision.ProjectID != scope.project || revision.PolicyID != pinned.PolicyID {
			return policyMismatch("Lease policy is outside the authorized project")
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
	for start := 0; start < len(ids); start += corestore.MaxPageSize {
		records, err := reader.DependencyReasonRecords(ctx, project, ids[start:min(start+corestore.MaxPageSize, len(ids))])
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
