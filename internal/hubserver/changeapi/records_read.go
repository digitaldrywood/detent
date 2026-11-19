package changeapi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *RecordReader) ListChangeRecords(ctx context.Context, scope hubscope.NativeScope, item string) ([]tracker.ChangeRequest, error) {
	if _, _, err := s.deps.ReadWorkItemRecord(ctx, scope, item); err != nil {
		return nil, err
	}
	var records []eventstore.Change
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		records, err = r.Changes(ctx, scope.Project, tracker.NativeWorkItemID(item), "", corestore.MaxPageSize)
		if err != nil || len(records) < corestore.MaxPageSize {
			return err
		}
		more, err := r.Changes(ctx, scope.Project, tracker.NativeWorkItemID(item), records[len(records)-1].ID, 1)
		if err != nil {
			return err
		}
		if len(more) != 0 {
			return corestore.ErrInvalidPage
		}
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if len(records) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	content := eventstore.NewContent(s.deps.RunDetailStore())
	result := make([]tracker.ChangeRequest, 0, len(records))
	for _, record := range records {
		change, err := content.Change(ctx, record)
		if err != nil {
			return nil, err
		}
		change, visible, err := s.FilterChangeRecord(ctx, scope, change)
		if err != nil {
			return nil, err
		}
		if visible {
			result = append(result, change)
		}
	}
	return result, nil
}

func (s *RecordReader) FilterChangeRecord(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest) (tracker.ChangeRequest, bool, error) {
	if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project {
		return tracker.ChangeRequest{}, false, corestore.ErrNotFound
	}
	linked := []tracker.NativeWorkItemID{}
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		cursor := tracker.NativeWorkItemID("")
		for {
			page, err := r.Links(ctx, change.ID, cursor, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, link := range page {
				if link.ProjectID != scope.Project {
					return corestore.ErrTenantMismatch
				}
				linked = append(linked, link.WorkItemID)
				if len(linked) > 32 {
					return corestore.ErrInvalidPage
				}
			}
			if len(page) < corestore.MaxPageSize {
				return nil
			}
			cursor = page[len(page)-1].WorkItemID
		}
	})
	if err != nil {
		return tracker.ChangeRequest{}, false, hubapi.ProjectRecordError(err)
	}
	slices.Sort(linked)
	linked = slices.Compact(linked)
	if len(linked) > 32 {
		return tracker.ChangeRequest{}, false, corestore.ErrInvalidPage
	}
	if len(linked) == 0 || !slices.Contains(linked, change.WorkItemID) {
		return tracker.ChangeRequest{}, false, corestore.ErrNotFound
	}
	change.LinkedIssues = linked
	visible := make([]tracker.NativeWorkItemID, 0, len(linked))
	for _, id := range linked {
		row, _, err := s.deps.ReadWorkItemRecord(ctx, scope, string(id))
		if errors.Is(err, corestore.ErrNotFound) || hubapi.IsNativeNotFound(err) {
			continue
		}
		if err != nil {
			return tracker.ChangeRequest{}, false, err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			continue
		}
		visible = append(visible, id)
	}
	if !slices.Contains(visible, change.WorkItemID) {
		return tracker.ChangeRequest{}, false, nil
	}
	change.LinkedIssues = visible
	return change, true, nil
}

func (s *RecordReader) ReadChangeRecord(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.ChangeRequest, error) {
	if _, _, err := s.deps.ReadWorkItemRecord(ctx, scope, item); err != nil {
		return tracker.ChangeRequest{}, err
	}
	var record eventstore.Change
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.Change(ctx, id)
		return err
	})
	if err != nil {
		return tracker.ChangeRequest{}, hubapi.ProjectRecordError(err)
	}
	change, err := eventstore.NewContent(s.deps.RunDetailStore()).Change(ctx, record)
	if err != nil {
		return tracker.ChangeRequest{}, err
	}
	change, visible, err := s.FilterChangeRecord(ctx, scope, change)
	if err != nil {
		return tracker.ChangeRequest{}, err
	}
	if !visible || !slices.Contains(change.LinkedIssues, tracker.NativeWorkItemID(item)) {
		return tracker.ChangeRequest{}, hubapi.NativeNotFound()
	}
	return change, nil
}

func (s *RecordReader) ReadChangeVersionRecord(ctx context.Context, organization tracker.OrganizationID, changeID, versionID string) (eventstore.Version, error) {
	var version eventstore.Version
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		version, err = r.Version(ctx, changeID, versionID)
		return err
	})
	return version, err
}

func (s *RecordReader) SelectedChangeDetail(ctx context.Context, scope hubscope.NativeScope, item, id string, now time.Time) (tracker.ChangeDetail, error) {
	change, err := s.ReadChangeRecord(ctx, scope, item, id)
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	var versions []eventstore.Version
	var evidence []eventstore.Evidence
	var policyRows hubrecords.CurrentChangePolicyRows
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		versions, err = r.Versions(ctx, change.ID, 0, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		if len(versions) == corestore.MaxPageSize {
			more, err := r.Versions(ctx, change.ID, versions[len(versions)-1].Number, 1)
			if err != nil {
				return err
			}
			if len(more) != 0 {
				return corestore.ErrInvalidPage
			}
		}
		evidence, err = r.Evidence(ctx, change.ID, 0, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		if len(evidence) == corestore.MaxPageSize {
			more, err := r.Evidence(ctx, change.ID, evidence[len(evidence)-1].Sequence, 1)
			if err != nil {
				return err
			}
			if len(more) != 0 {
				return corestore.ErrInvalidPage
			}
		}
		policyRows, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
	}
	if len(versions) > corestore.MaxPageSize || len(evidence) > corestore.MaxPageSize {
		return tracker.ChangeDetail{}, corestore.ErrInvalidPage
	}
	content := eventstore.NewContent(s.deps.RunDetailStore())
	result := tracker.ChangeDetail{Change: change}
	seenSources := make(map[string]bool)
	for _, linked := range change.LinkedIssues {
		row, snapshot, err := s.deps.ReadWorkItemRecord(ctx, scope, string(linked))
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		issue, err := s.deps.HydrateWorkItemRecord(ctx, row, snapshot)
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		for _, reference := range issue.ExternalReferences {
			if reference.Provider == "github" && reference.URL != "" && !seenSources[reference.URL] {
				result.SourceIssues = append(result.SourceIssues, reference)
				seenSources[reference.URL] = true
			}
		}
	}
	for _, row := range versions {
		version, err := s.deps.HydrateChangeVersion(ctx, scope, tracker.NativeWorkItemID(item), row)
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		if landed := change.CurrentLanding(); landed != nil && landed.Quality != nil && version.ChangeID == change.ID && landed.VersionID == version.ID && landed.HeadSHA == version.HeadSHA {
			version.LandingQuality = landed.Quality
		}
		result.Versions = append(result.Versions, version)
	}
	for _, row := range evidence {
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		switch row.Kind {
		case "review":
			var value tracker.ChangeReview
			if err := json.Unmarshal(raw, &value); err != nil {
				return tracker.ChangeDetail{}, err
			}
			result.Reviews = append(result.Reviews, value)
		case "check":
			var value tracker.ChangeCheck
			if err := json.Unmarshal(raw, &value); err != nil {
				return tracker.ChangeDetail{}, err
			}
			result.Checks = append(result.Checks, value)
		case "discussion":
			var value tracker.ChangeDiscussion
			if err := json.Unmarshal(raw, &value); err != nil {
				return tracker.ChangeDetail{}, err
			}
			result.Discussion = append(result.Discussion, value)
		}
	}
	var current *tracker.ChangeVersion
	for i := range result.Versions {
		if result.Versions[i].ID == result.Change.CurrentVersion {
			current = &result.Versions[i]
			break
		}
	}
	currentApproval, err := s.deps.HydrateChangeSummaryPolicy(ctx, content, scope, policyRows)
	if err != nil {
		return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
	}
	policyID, reviewPolicyID := changerequest.CurrentPolicyIdentities(current, currentApproval.Policy, currentApproval.ReviewPolicy)
	result.Summary, err = s.deps.SummarizeChangeWithAuditDispositions(ctx, scope, result, policyID, reviewPolicyID, now, false)
	if err == nil {
		err = s.LoadChangeExternalRecord(ctx, scope, tracker.NativeWorkItemID(item), &result)
	}
	return result, err
}

func (s *RecordReader) NativeClaimCurrentChangeDetail(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.NativeClaimPlan, now time.Time) (tracker.ChangeDetail, error) {
	if plan.ChangeFacts == nil || plan.ChangeFacts.VersionObservations == nil || plan.ChangeRecord.ID == "" || plan.ChangeRecord.CurrentVersionID == nil || *plan.ChangeRecord.CurrentVersionID != plan.ChangeVersion.ID || plan.ChangeVersion.ChangeID != plan.ChangeRecord.ID {
		return tracker.ChangeDetail{}, corestore.ErrRevisionConflict
	}
	return s.HydrateCurrentChangeDetailWithFacts(ctx, scope, plan.Candidate.WorkItemID, plan.ChangeRecord, &plan.ChangeVersion, plan.ChangePolicy, now, plan.ChangeFacts, plan.Approval, corestore.Serving)
}

func (s *RecordReader) SelectedCurrentChangeSummary(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, id string, now time.Time) (tracker.ChangeSummary, error) {
	if _, _, err := s.deps.ReadWorkItemRecord(ctx, scope, string(item)); err != nil {
		return tracker.ChangeSummary{}, err
	}
	var change eventstore.Change
	var version *eventstore.Version
	var policyRows hubrecords.CurrentChangePolicyRows
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		change, err = r.Change(ctx, id)
		if err != nil {
			return err
		}
		if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project {
			return corestore.ErrNotFound
		}
		if change.CurrentVersionID != nil {
			row, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return err
			}
			version = &row
		}
		policyRows, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return tracker.ChangeSummary{}, hubapi.ProjectRecordError(err)
	}
	detail, err := s.HydrateCurrentChangeDetail(ctx, scope, item, change, version, policyRows, now)
	if err == nil {
		err = s.LoadChangeExternalRecord(ctx, scope, item, &detail)
	}
	return detail.Summary, err
}

func (s *RecordReader) HydrateCurrentChangeDetail(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time) (tracker.ChangeDetail, error) {
	return s.HydrateCurrentChangeDetailWithFacts(ctx, scope, item, changeRecord, versionRecord, policyRows, now, nil, nil, corestore.Serving)
}

func (s *RecordReader) HydrateCurrentChangeDetailWithFacts(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time, facts *tenantstore.BoardChangeRows, approval *hubrecords.ChangeVersionApproval, workload corestore.Workload) (tracker.ChangeDetail, error) {
	return s.HydrateCurrentChangeDetailSelection(ctx, scope, item, changeRecord, versionRecord, policyRows, now, facts, approval, workload, false)
}

func (s *RecordReader) HydrateCurrentChangeDetailSelection(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time, facts *tenantstore.BoardChangeRows, approval *hubrecords.ChangeVersionApproval, workload corestore.Workload, recovery bool) (tracker.ChangeDetail, error) {
	if changeRecord.CurrentVersionID == nil && versionRecord != nil || changeRecord.CurrentVersionID != nil && (versionRecord == nil || *changeRecord.CurrentVersionID != versionRecord.ID || versionRecord.ChangeID != changeRecord.ID) {
		return tracker.ChangeDetail{}, corestore.ErrRevisionConflict
	}
	content := eventstore.NewContent(s.deps.RunDetailStoreFor(ctx))
	change, err := content.Change(ctx, changeRecord)
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	var visible bool
	if facts == nil {
		change, visible, err = s.FilterChangeRecord(ctx, scope, change)
	} else {
		change, visible, err = s.deps.FilterBoardChangeRecord(scope, change, *facts)
	}
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	if !visible || !slices.Contains(change.LinkedIssues, item) || change.CurrentVersion != hubapi.ValueOrEmpty(changeRecord.CurrentVersionID) {
		return tracker.ChangeDetail{}, corestore.ErrNotFound
	}
	if versionRecord == nil {
		currentApproval, err := s.deps.BoardCurrentApproval(ctx, content, scope, policyRows, approval)
		if err != nil {
			return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
		}
		detail := tracker.ChangeDetail{Change: change}
		policyID, reviewID := changerequest.CurrentPolicyIdentities(nil, currentApproval.Policy, currentApproval.ReviewPolicy)
		detail.Summary = changerequest.SummarizeCurrentVersion(detail, policyID, reviewID, now, false)
		return detail, nil
	}
	var observations *tenantstore.ChangeVersionRows
	if facts != nil {
		observations = facts.VersionObservations
	}
	version, err := s.deps.HydrateChangeVersionWithRows(ctx, scope, item, *versionRecord, observations)
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	if landed := change.CurrentLanding(); landed != nil && landed.Quality != nil && version.ChangeID == change.ID && landed.VersionID == version.ID && landed.HeadSHA == version.HeadSHA {
		version.LandingQuality = landed.Quality
	}
	var evidence []eventstore.Evidence
	var priorApproval bool
	if facts != nil {
		evidence = facts.Evidence
	} else if recovery {
		repository, ok := s.deps.Repository.(tenantstore.SnapshotRepository)
		if !ok {
			return tracker.ChangeDetail{}, corestore.ErrInvalidPage
		}
		checks := make([]string, 0, len(version.Checks))
		for _, check := range version.Checks {
			checks = append(checks, check.CheckRunID)
		}
		err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(eventstore.ChangeRecoveryReader)
			if !ok {
				return corestore.ErrInvalidPage
			}
			var err error
			evidence, priorApproval, err = reader.RecoveryChangeEvidence(ctx, change.ID, version.ID, changeRecord.EvidenceSequence, checks)
			return err
		})
	} else if hubcontent.ExplanationRead(ctx) {
		err = s.deps.Repository.ReadTenant(ctx, scope.Organization, workload, func(r tenantstore.Reader) error {
			reader, ok := r.(eventstore.ChangeGateEvidenceReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			var err error
			evidence, err = reader.CurrentVersionGateEvidence(ctx, change.ID, version.ID, changeRecord.EvidenceSequence)
			return err
		})
	} else {
		evidence, priorApproval, err = s.readCurrentChangeEvidence(ctx, scope, content, change.ID, version, changeRecord.EvidenceSequence)
	}
	if err != nil {
		return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
	}

	if !recovery && len(evidence) > corestore.MaxPageSize {
		return tracker.ChangeDetail{}, corestore.ErrInvalidPage
	}
	detail := tracker.ChangeDetail{Change: change, Versions: []tracker.ChangeVersion{ChangeVersionResponse(version)}}
	for _, row := range evidence {
		if row.OrganizationID != scope.Organization || row.ChangeID != changeRecord.ID {
			return tracker.ChangeDetail{}, corestore.ErrTenantMismatch
		}
		currentReview := row.Kind == "review" && row.VersionID != nil && *row.VersionID == version.ID
		priorReview := row.Kind == "review" && row.Decision == "approved" && (row.VersionID == nil || *row.VersionID != version.ID) && !priorApproval
		currentCheck := row.Kind == "check" && row.VersionID != nil && *row.VersionID == version.ID
		currentDiscussion := row.Kind == "discussion" && (row.VersionID == nil || *row.VersionID == "" || *row.VersionID == version.ID)
		if !currentReview && !priorReview && !currentCheck && !currentDiscussion {
			continue
		}
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		switch row.Kind {
		case "review":
			var review tracker.ChangeReview
			if err := json.Unmarshal(raw, &review); err != nil {
				return tracker.ChangeDetail{}, err
			}
			if review.VersionID != hubapi.ValueOrEmpty(row.VersionID) || review.Decision != row.Decision || review.Actor.Kind != row.ActorKind || review.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(review.CreatedAt, row.CreatedAt) {
				return tracker.ChangeDetail{}, eventstore.ErrInvalidRecord
			}
			if currentReview {
				detail.Reviews = append(detail.Reviews, review)
			} else if priorReview && review.Decision == "approved" && review.SecurityAudit == nil && review.ImplementationReview == nil && review.Validator == nil {
				priorApproval = true
			}
		case "check":
			var check tracker.ChangeCheck
			if err := json.Unmarshal(raw, &check); err != nil {
				return tracker.ChangeDetail{}, err
			}
			if check.VersionID != hubapi.ValueOrEmpty(row.VersionID) || check.CheckRunID != hubapi.ValueOrEmpty(row.SourceKey) || check.Actor.Kind != row.ActorKind || check.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(check.ReceivedAt, row.CreatedAt) {
				return tracker.ChangeDetail{}, eventstore.ErrInvalidRecord
			}
			detail.Checks = append(detail.Checks, check)
		case "discussion":
			var discussion tracker.ChangeDiscussion
			if err := json.Unmarshal(raw, &discussion); err != nil {
				return tracker.ChangeDetail{}, err
			}
			if discussion.VersionID != hubapi.ValueOrEmpty(row.VersionID) || discussion.Actor.Kind != row.ActorKind || discussion.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(discussion.CreatedAt, row.CreatedAt) {
				return tracker.ChangeDetail{}, eventstore.ErrInvalidRecord
			}
			detail.Discussion = append(detail.Discussion, discussion)
		}
	}
	currentApproval, err := s.deps.BoardCurrentApproval(ctx, content, scope, policyRows, approval)
	if err != nil {
		return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
	}
	policyID, reviewPolicyID := changerequest.CurrentPolicyIdentities(&detail.Versions[0], currentApproval.Policy, currentApproval.ReviewPolicy)
	detail.Summary, err = s.deps.SummarizeChangeWithAuditDispositionsFor(ctx, scope, detail, policyID, reviewPolicyID, now, priorApproval, workload, facts)
	if err == nil && hubcontent.ExplanationRead(ctx) && facts == nil && !recovery && (len(detail.Checks) > 64 || len(detail.Reviews) > 64) {
		detail.Summary.Status = "evidence_incomplete"
		if len(detail.Checks) > 64 {
			detail.Summary.Checks = "unknown"
		}
		if len(detail.Reviews) > 64 {
			detail.Summary.NativeReview, detail.Summary.ExternalReview = "unknown", "unknown"
		}
		detail.Summary.Messages = []string{"Current-version check or review history is truncated"}
	}
	if err == nil && recovery {
		detail.PublicationPushes, err = s.deps.ReadBoundedChangePushes(ctx, scope, string(item), detail.Versions)
	}
	return detail, err
}

func sameTypedEvidenceTime(payload, typed time.Time) bool {
	return payload.UTC().Truncate(time.Microsecond).Equal(typed.UTC().Truncate(time.Microsecond))
}

func (s *RecordReader) SelectedLatestChangeSummary(ctx context.Context, scope hubscope.NativeScope, item string, now time.Time) (tracker.NativeChangeReference, tracker.ChangeSummary, bool, error) {
	var latest eventstore.Change
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		latest, err = r.LatestChange(ctx, scope.Project, tracker.NativeWorkItemID(item))
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return tracker.NativeChangeReference{}, tracker.ChangeSummary{}, false, nil
	}
	if err != nil {
		return tracker.NativeChangeReference{}, tracker.ChangeSummary{}, false, hubapi.ProjectRecordError(err)
	}
	detail, err := s.SelectedChangeDetail(ctx, scope, item, latest.ID, now)
	if err != nil {
		return tracker.NativeChangeReference{}, tracker.ChangeSummary{}, false, err
	}
	reference := tracker.NativeChangeReference{ChangeID: latest.ID}
	for _, version := range detail.Versions {
		if version.ID == detail.Change.CurrentVersion {
			reference.VersionID = version.ID
			reference.HeadSHA = version.HeadSHA
			break
		}
	}
	return reference, detail.Summary, true, nil
}

func (s *RecordReader) LoadChangeExternalRecord(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, detail *tracker.ChangeDetail) error {
	var selected *tracker.ChangeVersion
	for index := range detail.Versions {
		version := &detail.Versions[index]
		if version.ID == detail.Change.CurrentVersion && version.External != nil && version.External.Provider == "github" {
			selected = version
			break
		}
	}
	if selected == nil {
		return nil
	}
	number := s.deps.ExternalPullRequestNumber(*selected.External)
	owner, repository, valid := s.deps.SplitRepositoryFullName(strings.TrimPrefix(strings.TrimSuffix(selected.Repository, ".git"), "https://github.com/"))
	if !valid || number <= 0 || selected.External.URL != "https://github.com/"+owner+"/"+repository+"/pull/"+selected.External.ID {
		return nil
	}
	name := strings.ToLower(owner + "/" + repository)
	var observed *projectstore.GitHubPullRequestObservation
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		itemRecord, err := r.WorkItem(ctx, scope.Project, item)
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(itemRecord.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		project, err := r.ProjectMetadata(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" || !project.Settings.RepositoryEnabled || !strings.EqualFold(project.Settings.CheckoutRepository, name) {
			return nil
		}
		change, err := r.Change(ctx, detail.Change.ID)
		if err != nil {
			return err
		}
		if change.ProjectID != scope.Project || change.Revision != int64(detail.Change.Revision) || hubapi.ValueOrEmpty(change.CurrentVersionID) != selected.ID {
			return corestore.ErrRevisionConflict
		}
		version, err := r.Version(ctx, change.ID, selected.ID)
		if err != nil {
			return err
		}
		if version.HeadSHA != selected.HeadSHA {
			return corestore.ErrRevisionConflict
		}
		if version.ExternalProvider != selected.External.Provider || version.ExternalID != selected.External.ID || version.ExternalURL != selected.External.URL {
			publication, err := r.PRPublication(ctx, selected.ID)
			if err != nil {
				return err
			}
			binding := tracker.NativePRPublication{ChangeID: publication.ChangeID, VersionID: publication.VersionID, Repository: publication.Repository, BaseRef: publication.BaseRef, Branch: publication.Branch, HeadSHA: publication.HeadSHA, PolicyID: publication.PolicyID, SourceVersion: tracker.NativeChangeReference{ChangeID: publication.ChangeID, VersionID: publication.VersionID, HeadSHA: publication.HeadSHA}, SourceAttemptID: publication.SourceAttemptID, External: publication.External}
			if !binding.Matches(detail.Change.ID, *selected) || publication.External != *selected.External {
				return corestore.ErrRevisionConflict
			}
		}
		reader, ok := r.(projectstore.GitHubPullRequestRefreshReader)
		if !ok {
			return hubapi.ErrPostgresFeatureUnavailable
		}
		row, err := reader.GitHubPullRequestRefresh(ctx, projectstore.GitHubPullRequestRefreshKey{ProjectID: scope.Project, ChangeID: change.ID, Repository: name, Number: number})
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.RequestedVersion != selected.ID || row.RequestedHeadSHA != selected.HeadSHA || row.ObservedRequestCount != row.RequestCount || row.Observation == nil || row.Observation.Repository != name || row.Observation.Number != number || row.Observation.ObservedAt.Before(row.RequestedAt) {
			return nil
		}
		observed = row.Observation
		return nil
	})
	if err != nil || observed == nil {
		return err
	}
	detail.External = &tracker.PullRequestSummary{Number: number, URL: selected.External.URL, State: observed.State, Draft: observed.Draft, HeadSHA: observed.HeadSHA, Checks: observed.Checks, Reviews: observed.Reviews, Merge: tracker.MergeSummary{State: "unknown"}}
	if observed.HeadSHA != selected.HeadSHA {
		detail.Summary.ExternalReview = "stale_head"
	} else if observed.Reviews.Decision != "" {
		detail.Summary.ExternalReview = "snapshot: " + observed.Reviews.Decision
	}
	return nil
}

func (s *RecordReader) readCurrentChangeEvidence(ctx context.Context, scope hubscope.NativeScope, content *eventstore.Content, change string, version tracker.ChangeVersion, through int64) ([]eventstore.Evidence, bool, error) {
	checks := make([]string, 0, len(version.Checks))
	for _, check := range version.Checks {
		checks = append(checks, check.CheckRunID)
	}
	var evidence []eventstore.Evidence
	var candidate eventstore.Evidence
	found := false
	var after eventstore.PriorApprovalCursor
	for range corestore.MaxPageSize + 1 {
		err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(eventstore.CurrentChangeEvidenceReader)
			if !ok {
				return corestore.ErrInvalidPage
			}
			if evidence == nil {
				var err error
				evidence, err = reader.CurrentChangeEvidence(ctx, change, version.ID, through, checks)
				if err != nil {
					return err
				}
				if evidence == nil {
					evidence = []eventstore.Evidence{}
				}
			}
			var err error
			candidate, err = reader.PriorApprovalCandidate(ctx, change, version.ID, through, after)
			found = err == nil
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			return err
		})
		if err != nil || !found {
			return evidence, false, err
		}
		if candidate.OrganizationID != scope.Organization || candidate.ChangeID != change || candidate.Kind != "review" || candidate.Decision != "approved" || candidate.Sequence > through || candidate.VersionID != nil && *candidate.VersionID == version.ID {
			return nil, false, corestore.ErrTenantMismatch
		}
		if candidate.ReviewKind == "human" {
			return evidence, true, nil
		}
		raw, err := content.Evidence(ctx, candidate)
		if err != nil {
			return nil, false, err
		}
		var review tracker.ChangeReview
		if err := json.Unmarshal(raw, &review); err != nil {
			return nil, false, err
		}
		if review.VersionID != hubapi.ValueOrEmpty(candidate.VersionID) || review.Decision != candidate.Decision || review.Actor.Kind != candidate.ActorKind || review.Actor.PrincipalID != candidate.ActorPrincipalID || !sameTypedEvidenceTime(review.CreatedAt, candidate.CreatedAt) {
			return nil, false, eventstore.ErrInvalidRecord
		}
		if review.SecurityAudit == nil && review.ImplementationReview == nil && review.Validator == nil {
			return evidence, true, nil
		}
		after = eventstore.PriorApprovalCursorAfter(candidate)
	}
	return nil, false, corestore.ErrInvalidPage
}
