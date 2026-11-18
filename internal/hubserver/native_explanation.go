package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) attachExplanationCurrentChange(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, now time.Time, evidence *tracker.NativeRuntimeEvidence) error {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	var change eventstore.Change
	var version *eventstore.Version
	var policy hubrecords.CurrentChangePolicyRows
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeBulkExportItem(ctx, r, scope, item); err != nil {
			return err
		}
		row, err := r.LatestChange(ctx, scope.Project, item)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.OrganizationID != scope.Organization || row.ProjectID != scope.Project {
			return corestore.ErrNotFound
		}
		change = row
		if change.CurrentVersionID != nil {
			current, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return err
			}
			version = &current
		}
		policy, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return hubapi.ProjectRecordError(err)
	}
	if change.ID == "" {
		return nil
	}
	detail, err := s.hydrateCurrentChangeDetail(ctx, scope, item, change, version, policy, now)
	if err != nil {
		return err
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeBulkExportItem(ctx, r, scope, item)
	})
	if err != nil {
		return hubapi.ProjectRecordError(err)
	}
	attachRuntimeChangeDetail(evidence, detail)
	return nil
}

func attachRuntimeChangeDetail(result *tracker.NativeRuntimeEvidence, detail tracker.ChangeDetail) {
	if len(detail.Versions) != 0 {
		version := detail.Versions[0]
		result.CurrentVersion = &tracker.NativeChangeVersionEvidence{NativeChangeReference: tracker.NativeChangeReference{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}, PublishedAt: version.CreatedAt, Validation: hubrecords.BoundedChangeCommand(version.Validation), Landing: hubrecords.BoundedChangeLanding(version.Landing)}
	}
	if detail.Summary.Status == "evidence_incomplete" {
		result.Unavailable = append(result.Unavailable, "change_gate_history_truncated")
	}
	detail.Change.Body = ""
	detail.Change.LinkedIssues = nil
	detail.Versions, detail.Reviews, detail.Checks, detail.Discussion = nil, nil, nil, nil
	detail.Summary.Messages = nil
	result.Change = &detail
}

func boundedNativeExplanation(evidence tracker.NativeRuntimeEvidence, failure *explain.FailureSignature) (explain.IssueExplanation, error) {
	evidence.Issue.IssueContract = nil
	if version := evidence.CurrentVersion; version != nil {
		current := *version
		current.Landing = hubrecords.ExplanationLanding(version.Landing)
		current.Validation = explanationCommand(version.Validation)
		evidence.CurrentVersion = &current
		if version.Validation != nil && (version.Validation.Pipeline != nil || version.Validation.Evidence != nil) {
			evidence.Unavailable = append(evidence.Unavailable, "current_version_validation_evidence_omitted")
		}
		if version.Landing != nil {
			evidence.Unavailable = append(evidence.Unavailable, "landing_details_omitted")
		}
	}
	result := explain.FromNativeEvidence(evidence)
	result.FailureSignature = failure
	raw, err := json.Marshal(result)
	if err != nil {
		return explain.IssueExplanation{}, err
	}
	if len(raw) < 256*1024 {
		return result, nil
	}
	result.NativeRuntime = &tracker.NativeRuntimeEvidence{
		ChangeSequence: evidence.ChangeSequence,
		Issue:          tracker.NativeIssue{NativeReference: evidence.Issue.NativeReference, State: evidence.Issue.State},
		ObservedAt:     evidence.ObservedAt,
		Unavailable:    []string{"explanation_evidence_truncated"},
	}
	if slices.Contains(evidence.Unavailable, "change_gate_history_truncated") {
		result.NativeRuntime.Unavailable = append(result.NativeRuntime.Unavailable, "change_gate_history_truncated")
	}
	result.Sources = []explain.SourceStatus{{Name: "explanation_evidence_truncated", State: explain.SourceUnavailable}}
	result.Evidence, result.Reasons, result.LatestTransition, result.FailureSignature = nil, nil, nil, nil
	result.Eligibility.Refusals, result.Eligibility.Latest, result.Eligibility.Current = nil, nil, nil
	result.RequiredGate.Reason = hubapi.BoundedFailureText(result.RequiredGate.Reason, 4096)
	result.RequiredGate.Failures, result.RequiredGate.Running = nil, nil
	if result.RequiredGate.ExternalMerge != nil {
		result.RequiredGate.ExternalMerge.Detail = hubapi.BoundedFailureText(result.RequiredGate.ExternalMerge.Detail, 4096)
	}
	raw, err = json.Marshal(result)
	if err != nil {
		return explain.IssueExplanation{}, err
	}
	if len(raw) >= 256*1024 {
		return explain.IssueExplanation{}, errors.New("explanation identity and gate exceed response budget")
	}
	return result, nil
}

func explanationCommand(value *gate.CommandResult) *gate.CommandResult {
	if value == nil {
		return nil
	}
	result := *value
	result.Pipeline, result.Evidence = nil, nil
	return hubrecords.BoundedChangeCommand(&result)
}
