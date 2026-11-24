package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) getNativeRecoveryTenant(c echo.Context, params url.Values) error {
	if hubapi.ValidateNativeQuery(params, "view", "projection", "detail") != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("view supports recovery"))
	}
	if params.Get("detail") != "" && params.Get("detail") != "full" && params.Get("detail") != "summary" {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("detail supports full or summary"))
	}
	ctx := c.Request().Context()
	ctx = s.runDetailContext(ctx)
	ctx = s.contentContext(ctx)
	if params.Get("detail") == "summary" || params.Get("detail") == "" && c.Request().Header.Get("X-Detent-Recovery-Detail") == "summary" {
		ctx = context.WithValue(ctx, attemptSummaryContextKey{}, true)
	}
	if params.Has("projection") || c.Request().Header.Get("X-Detent-Recovery-Projection") != "" {
		if params.Get("projection") != "bounded" && c.Request().Header.Get("X-Detent-Recovery-Projection") != "bounded" || params.Has("limit") || params.Has("cursor") {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("Recovery projection supports bounded"))
		}
		result, err := s.readNativeRecoveryTenant(ctx, hubscope.NativeRequestScope(c), c.Param("item"))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		result.Issue = s.nativeIssueResponse(result.Issue)
		if err := s.fitNativeRecovery(ctx, hubscope.NativeRequestScope(c), c.Request().URL.EscapedPath(), &result, c.Request().Header.Get("X-Detent-Recovery-Body") == "fragments"); err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	if params.Has("limit") || params.Has("cursor") {
		result, err := s.readNativeRecoveryPageTenant(ctx, hubscope.NativeRequestScope(c), c.Param("item"), c.Request().URL.EscapedPath(), params)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	result, err := s.readNativeRecoveryFactsTenant(ctx, hubscope.NativeRequestScope(c), c.Param("item"), false)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if attemptSummaryRead(ctx) {
		if err := projectNativeRecovery(&result); err != nil {
			return s.nativeAPIError(c, fmt.Errorf("project recovery response: %w", err))
		}
	} else {
		slices.Sort(result.Read.Omitted)
		result.Read.Omitted = slices.Compact(result.Read.Omitted)
	}
	result.Issue = s.nativeIssueResponse(result.Issue)
	return c.JSON(http.StatusOK, result)
}

func (s *Service) readNativeRecoveryTenant(ctx context.Context, scope hubscope.NativeScope, item string) (tracker.NativeRecovery, error) {
	return s.readNativeRecoveryCommentsTenant(ctx, scope, item, false)
}

func (s *Service) readNativeRecoveryCommentsTenant(ctx context.Context, scope hubscope.NativeScope, item string, excludeProgress bool) (tracker.NativeRecovery, error) {
	result, err := s.readNativeRecoveryFactsTenant(ctx, scope, item, excludeProgress)
	if err != nil {
		return result, err
	}
	if err := projectNativeRecovery(&result); err != nil {
		return result, fmt.Errorf("project recovery response: %w", err)
	}
	return result, nil
}

func (s *Service) readNativeRecoveryFactsTenant(ctx context.Context, scope hubscope.NativeScope, item string, excludeProgress bool) (tracker.NativeRecovery, error) {
	var result tracker.NativeRecovery
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	var change eventstore.Change
	var changeVersion *eventstore.Version
	var policyRows hubrecords.CurrentChangePolicyRows
	var comments []corestore.Comment
	var commentVersions []eventstore.CollaborationVersion
	var commentsTruncated bool
	var snapshots []hubrecords.TenantAttemptSnapshot
	var attemptPage leasestore.AttemptPage
	var progressAttempts []leasestore.Attempt
	var checkpointCandidates []checkpointAttemptCandidate
	var checkpointPageTruncated bool
	var landingReceipt eventstore.LandingReceipt
	var latestAttemptID string
	var events []eventstore.Event
	var mergeHistoryTruncated bool
	var projectSequence int64
	var continuationEligible *bool
	var ownsLiveLease bool
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return tracker.NativeRecovery{}, hubapi.NativeRunEventStorageUnavailable()
	}
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		row, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		eligible, known, err := readRunnerRecoveryEligibility(ctx, r, scope, row.NativeID)
		if err != nil {
			return err
		}
		if known {
			continuationEligible = &eligible
		}
		if eligible && strings.EqualFold(hubapi.ValueOrEmpty(row.WorkflowStateName), "Blocked") && scope.Credential.Runner.RunnerID != "" {
			lease, err := r.ActiveLeaseForIssue(ctx, row.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				owner, err := r.RunnerForLease(ctx, lease.LeaseID)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				ownsLiveLease = err == nil && owner.RunnerID == scope.Credential.Runner.RunnerID
			}
		}
		version, err = r.CollaborationVersion(ctx, item, row.Revision)
		if err != nil {
			return err
		}
		change, err = r.LatestChange(ctx, scope.Project, row.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project {
				return corestore.ErrTenantMismatch
			}
			if change.CurrentVersionID != nil {
				value, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
				if err != nil {
					return err
				}
				changeVersion = &value
			}
			policyRows, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
			if err != nil {
				return err
			}
		}
		result.HumanDiscussionSequence, err = r.HumanDiscussionSequence(ctx, scope.Project, row.NativeID)
		if err != nil {
			return err
		}
		comments = make([]corestore.Comment, 0, 33)
		latestMachine := false
		beforeSequence := int64(1<<63 - 1)
		commentScanComplete := false
		for range claimPreparationLimit {
			page, err := r.SelectComments(ctx, scope.Project, row.NativeID, beforeSequence, true, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, comment := range page {
				if excludeProgress && comment.ActorKind == "machine" && hubapi.StringValue(comment.ProvenanceProvider) == "progress-check" {
					continue
				}
				keep := comment.ActorKind == "human" || hubapi.StringValue(comment.EditedByKind) == "human" || !latestMachine
				if comment.ActorKind != "human" {
					latestMachine = true
				}
				if keep {
					comments = append(comments, comment)
				}
				if len(comments) == 33 {
					commentsTruncated = true
					break
				}
			}
			if commentsTruncated || len(page) < corestore.MaxPageSize {
				commentScanComplete = true
				break
			}
			beforeSequence = page[len(page)-1].Sequence
		}
		if !commentScanComplete {
			commentsTruncated = true
		}
		if len(comments) > 32 {
			comments = comments[:32]
		}
		commentVersions = make([]eventstore.CollaborationVersion, len(comments))
		for i, comment := range comments {
			commentVersions[i], err = r.CollaborationVersion(ctx, comment.ID, comment.Revision)
			if err != nil {
				return err
			}
		}
		attemptPage, err = r.AttemptPage(ctx, row.ID, leasestore.AttemptCursor{}, claimPreparationLimit)
		if err != nil {
			return err
		}
		if len(attemptPage.Items) > 0 {
			latestAttemptID = attemptPage.Items[0].ID
		}
		wantedAttempts := make(map[string]struct{}, claimPreparationLimit)
		if excludeProgress {
			for _, attempt := range attemptPage.Items {
				if attempt.Status == "running" {
					continue
				}
				wantedAttempts[attempt.ID] = struct{}{}
				progressAttempts = append(progressAttempts, attempt)
				if len(progressAttempts) == progresscheck.MaxAttempts {
					break
				}
			}
		}
		if latestAttemptID != "" {
			wantedAttempts[latestAttemptID] = struct{}{}
		}
		checkpointReader, ok := r.(leasestore.CheckpointAttemptReader)
		if !ok {
			return corestore.ErrInvalidPage
		}
		checkpointCursor := leasestore.AttemptCursor{}
		for len(checkpointCandidates) < claimPreparationLimit {
			remaining := claimPreparationLimit - len(checkpointCandidates)
			limit := min(corestore.MaxPageSize, remaining)
			page, err := checkpointReader.CheckpointAttemptPage(ctx, row.ID, checkpointCursor, limit)
			if err != nil {
				return err
			}
			attemptIDs := make([]string, 0, len(page.Items))
			for _, attempt := range page.Items {
				if attempt.IssueID != row.ID || attempt.CheckpointObjectKey == nil {
					return corestore.ErrNotFound
				}
				attemptIDs = append(attemptIDs, attempt.ID)
			}
			if len(attemptIDs) == 0 {
				if page.Next == (leasestore.AttemptCursor{}) {
					break
				}
				checkpointCursor = page.Next
				continue
			}
			events, err := r.LatestCheckpointEvents(ctx, scope.Project, row.NativeID, attemptIDs)
			if err != nil {
				return err
			}
			eventsByAttempt := make(map[string]eventstore.Event, len(events))
			for _, event := range events {
				eventsByAttempt[event.AttemptID] = event
			}
			for _, attempt := range page.Items {
				event, ok := eventsByAttempt[attempt.ID]
				if !ok {
					return corestore.ErrNotFound
				}
				if event.OrganizationID != scope.Organization || event.ProjectID != scope.Project || event.WorkItemID != row.NativeID || event.AttemptID != attempt.ID || event.Type != "run.checkpointed" {
					return corestore.ErrNotFound
				}
				checkpointCandidates = append(checkpointCandidates, checkpointAttemptCandidate{attempt: attempt, event: event})
			}
			if page.Next == (leasestore.AttemptCursor{}) {
				break
			}
			checkpointCursor = page.Next
			if len(checkpointCandidates) == claimPreparationLimit {
				checkpointPageTruncated = true
			}
		}
		ensureAttempt := func(id string) error {
			if id == "" {
				return nil
			}
			for _, attempt := range attemptPage.Items {
				if attempt.ID == id {
					wantedAttempts[id] = struct{}{}
					return nil
				}
			}
			attempt, err := r.Attempt(ctx, id)
			if err != nil {
				return err
			}
			if attempt.IssueID != row.ID {
				return corestore.ErrNotFound
			}
			wantedAttempts[id] = struct{}{}
			attemptPage.Items = append(attemptPage.Items, attempt)
			return nil
		}
		if changeVersion != nil && changeVersion.AttemptID != "" {
			if err := ensureAttempt(changeVersion.AttemptID); err != nil {
				return err
			}
		}
		landingReceipt, err = r.LatestLandingReceipt(ctx, scope.Project, row.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			if landingReceipt.OrganizationID != scope.Organization || landingReceipt.AttemptID == "" || landingReceipt.VersionID == "" {
				return eventstore.ErrInvalidRecord
			}
			if err := ensureAttempt(landingReceipt.AttemptID); err != nil {
				return err
			}
		}
		selectedAttempts := make([]leasestore.Attempt, 0, len(wantedAttempts))
		for _, attempt := range attemptPage.Items {
			if _, wanted := wantedAttempts[attempt.ID]; wanted {
				selectedAttempts = append(selectedAttempts, attempt)
			}
		}
		r, err = readAttemptMetadataSnapshot(ctx, r, scope, item, selectedAttempts, false)
		if err != nil {
			return err
		}
		snapshots = make([]hubrecords.TenantAttemptSnapshot, 0, len(selectedAttempts))
		for _, attempt := range selectedAttempts {
			if attempt.IssueID != row.ID {
				return corestore.ErrNotFound
			}
			lease, err := r.LeaseSnapshot(ctx, attempt.LeaseID)
			if err != nil {
				return err
			}
			if lease.IssueID != row.ID || lease.FencingToken != attempt.FencingToken {
				return corestore.ErrNotFound
			}
			event, err := r.LatestRunEvent(ctx, scope.Project, attempt.ID)
			if err != nil {
				return err
			}
			if event.WorkItemID != row.NativeID {
				return corestore.ErrNotFound
			}
			snapshot := hubrecords.TenantAttemptSnapshot{Attempt: attempt, Lease: lease.Lease, LeaseObservation: lease.LeaseObservation, Event: event}
			if err := hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot); err != nil {
				return err
			}
			snapshots = append(snapshots, snapshot)
		}
		projectSequence, err = r.ProjectEventSequence(ctx, scope.Project)
		if err != nil {
			return err
		}
		if projectSequence == 0 {
			return nil
		}
		before := projectSequence + 1
		for range claimPreparationLimit {
			page, err := r.EventsBefore(ctx, scope.Project, row.NativeID, before, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for index, event := range page {
				if event.Type == "workflow.transitioned" && (event.FromState == "" || strings.EqualFold(event.FromState, "merging")) {
					events = append(events, event)
					if len(events) == claimPreparationLimit {
						mergeHistoryTruncated = recoveryMergeHistoryPageHasMore(page, index, corestore.MaxPageSize)
						return nil
					}
				}
			}
			if len(page) < corestore.MaxPageSize {
				return nil
			}
			before = page[len(page)-1].Sequence
		}
		return corestore.ErrInvalidPage
	})
	if err != nil {
		return result, fmt.Errorf("read PostgreSQL recovery facts: %w", hubapi.ProjectRecordError(err))
	}
	result.Issue, err = s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return result, fmt.Errorf("hydrate recovery work item: %w", err)
	}
	if err = s.hydrateWorkItemRelationshipFacts(ctx, scope, &result.Issue); err != nil {
		return result, fmt.Errorf("hydrate recovery work item relationships: %w", err)
	}
	if continuationEligible != nil && *continuationEligible && !ownsLiveLease && strings.EqualFold(result.Issue.State, "Blocked") {
		recovery, err := s.readRecordedRecoveryIntent(ctx, scope, row, result.Issue, false)
		if err != nil {
			return result, fmt.Errorf("read recorded recovery intent: %w", err)
		}
		continuationEligible = new(recovery != nil)
	}
	result.ContinuationEligible = continuationEligible
	result.Issue.OmittedFields = append(result.Issue.OmittedFields, "change_sequence")
	if change.ID != "" {
		var detail tracker.ChangeDetail
		detail, err = s.hydrateCurrentChangeDetailSelection(ctx, scope, row.NativeID, change, changeVersion, policyRows, s.config.Clock(), nil, nil, corestore.Serving, true)
		if err != nil {
			return result, fmt.Errorf("hydrate recovery current change: %w", err)
		}
		result.ChangeDetail = &detail
		if changeVersion != nil {
			result.Change = &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: changeVersion.ID, HeadSHA: changeVersion.HeadSHA}
			if strings.EqualFold(result.Issue.State, "Merging") && nativeChangeLandingClaimable(result.Issue.State, &detail) {
				result.Issue.ListChange = &tracker.NativeListChange{LandingPending: true, HeadSHA: changeVersion.HeadSHA}
			}
			for _, snapshot := range snapshots {
				if snapshot.Attempt.ID != latestAttemptID {
					continue
				}
				plan := hubrecords.NativeClaimPlan{Candidate: claimstore.Candidate{State: result.Issue.State}, Item: row, ChangeRecord: change, ChangeVersion: *changeVersion, LatestAttempt: snapshot.Attempt, LatestAttemptFound: true, LatestRunEvent: snapshot.Event, LatestRunEventFound: snapshot.Event.ID != ""}
				if nativeClaimPendingLanding(plan) {
					result.Issue.ListChange = &tracker.NativeListChange{LandingPending: true, HeadSHA: changeVersion.HeadSHA}
				}
				break
			}
			if row.RecoveryRunnerID != "" && row.RecoveryVersionID == changeVersion.ID {
				result.SourceVersionID = changeVersion.ID
			}
		}
	}
	for i, comment := range comments {
		value, err := s.hydrateCommentRecord(ctx, comment, commentVersions[i])
		if err != nil {
			return result, fmt.Errorf("hydrate recovery comment: %w", err)
		}
		result.Discussion = append(result.Discussion, value)
	}
	slices.Reverse(result.Discussion)
	if commentsTruncated {
		result.Read = &tracker.NativeRecoveryRead{Omitted: []string{"discussion"}}
	}
	if result.Read == nil {
		result.Read = &tracker.NativeRecoveryRead{}
	}
	if scope.Credential.Runner.RunnerID != "" {
		result.Read.Omitted = append(result.Read.Omitted, "instance_successes")
	}
	result.Read.DetailPath = "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/work-items/" + url.PathEscape(item) + "?view=recovery&limit=1"
	var sourceAttemptID, changeAttemptID, landingAttemptID, checkpointAttemptID string
	var hydrated = make(map[string]tracker.NativeAttempt, len(snapshots))
	checkpointsByAttempt := make(map[string]tracker.NativeCheckpoint, len(checkpointCandidates))
	var landingValue *tracker.NativeLandingReceipt
	if landingReceipt.AttemptID != "" {
		landingAttemptID = landingReceipt.AttemptID
		value, err := eventstore.NewContent(s.runDetailStore()).LandingReceipt(ctx, landingReceipt)
		if err != nil {
			return result, fmt.Errorf("read recovery landing receipt: %w", err)
		}
		landingValue = &value
	}
	if len(checkpointCandidates) > 0 {
		checkpointAttemptID = checkpointCandidates[0].attempt.ID
	}
	for _, candidate := range checkpointCandidates {
		value, err := eventstore.NewContent(s.runDetailStore()).Event(ctx, candidate.event)
		if err != nil {
			return result, fmt.Errorf("read recovery checkpoint event: %w", err)
		}
		if value.Data.Run == nil {
			return result, artifact.ErrIntegrity
		}
		if value.Data.Run.AttemptID != candidate.attempt.ID || value.Data.Run.RunID != candidate.attempt.RunID || value.Data.Run.Sequence < 1 || value.Data.Run.Sequence > candidate.attempt.Sequence || value.Data.Run.FencingToken != tracker.FencingToken(candidate.attempt.FencingToken) {
			return result, artifact.ErrIntegrity
		}
		if value.Data.Run.Handoff == nil {
			return result, artifact.ErrIntegrity
		}
		checkpointsByAttempt[candidate.attempt.ID] = *value.Data.Run.Handoff
		if sourceAttemptID == "" && value.Data.Run.Handoff.PreservesSource() {
			sourceAttemptID = candidate.attempt.ID
			break
		}
	}
	if checkpointPageTruncated && sourceAttemptID == "" {
		return result, fmt.Errorf("read recovery checkpoints: source checkpoint was not found within the bounded history: %w", corestore.ErrInvalidPage)
	}
	if result.ChangeDetail != nil && len(result.ChangeDetail.Versions) == 1 {
		changeAttemptID = result.ChangeDetail.Versions[0].AttemptID
		if sourceAttemptID == "" {
			sourceAttemptID = changeAttemptID
		}
	}
	result.SourceAttemptID = sourceAttemptID
	selected := make(map[string]struct{}, 5)
	for _, id := range []string{sourceAttemptID, changeAttemptID, landingAttemptID, checkpointAttemptID} {
		if id != "" {
			selected[id] = struct{}{}
		}
	}
	if len(snapshots) > 0 {
		selected[snapshots[0].Attempt.ID] = struct{}{}
	}
	if excludeProgress {
		selected = make(map[string]struct{}, len(progressAttempts))
		for _, attempt := range progressAttempts {
			selected[attempt.ID] = struct{}{}
		}
	}
	selectedIDs := make([]string, 0, len(selected))
	for _, id := range []string{sourceAttemptID, changeAttemptID, landingAttemptID, checkpointAttemptID, latestAttemptID} {
		if id == "" {
			continue
		}
		if _, seen := selected[id]; !seen {
			continue
		}
		selectedIDs = append(selectedIDs, id)
	}
	for _, id := range selectedIDs {
		if findRecoveryAttemptSnapshot(snapshots, id) {
			continue
		}
		snapshot, err := s.readNativeRecoveryAttemptSnapshot(ctx, scope, item, row.ID, id)
		if err != nil {
			return result, fmt.Errorf("read selected recovery attempt metadata: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	for _, snapshot := range snapshots {
		if _, ok := selected[snapshot.Attempt.ID]; !ok {
			continue
		}
		attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, s.config.Clock())
		if err != nil {
			return result, fmt.Errorf("hydrate selected recovery attempt: %w", err)
		}
		attempt.Runtime = attempt.Runtime.WithoutActivitySpans()
		if checkpoint, ok := checkpointsByAttempt[snapshot.Attempt.ID]; ok {
			attempt.Checkpoint = &checkpoint
		}
		if snapshot.Attempt.ID == landingAttemptID && landingValue != nil {
			if attempt.Runtime == nil {
				attempt.Runtime = &tracker.NativeRuntimeObservation{}
			}
			attempt.Runtime.Landing = landingValue
		}
		hydrated[snapshot.Attempt.ID] = attempt
	}
	ordered := make([]leasestore.Attempt, 0, len(selected))
	for _, snapshot := range snapshots {
		if _, ok := selected[snapshot.Attempt.ID]; ok {
			ordered = append(ordered, snapshot.Attempt)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].FencingToken < ordered[j].FencingToken })
	for _, attempt := range ordered {
		value, ok := hydrated[attempt.ID]
		if !ok {
			return result, corestore.ErrNotFound
		}
		result.Attempts = append(result.Attempts, value)
	}
	if attemptPage.Next != (leasestore.AttemptCursor{}) || len(result.Attempts) < len(attemptPage.Items) {
		result.Read.Omitted = append(result.Read.Omitted, "attempts")
	}
	for _, event := range events {
		if event.FromState != "" && !strings.EqualFold(event.FromState, "merging") {
			continue
		}
		value, _, err := s.readTenantCollaborationEvent(ctx, event)
		if err != nil {
			return result, fmt.Errorf("read recovery merge event: %w", err)
		}
		if strings.EqualFold(value.Data.FromState, "merging") {
			result.History = []tracker.CollaborationEvent{value}
			break
		}
	}
	if mergeHistoryTruncated && len(result.History) == 0 {
		result.Read.Omitted = append(result.Read.Omitted, "history")
	}
	if detail := result.ChangeDetail; detail != nil && attemptSummaryRead(ctx) {
		detail.Reviews, detail.Checks, detail.Discussion = nil, nil, nil
		result.Read.Omitted = append(result.Read.Omitted, "change_detail.reviews", "change_detail.checks", "change_detail.discussion")
	}
	return result, nil
}

func (s *Service) readNativeRecoveryPageTenant(ctx context.Context, scope hubscope.NativeScope, item, path string, params url.Values) (tracker.NativeRecovery, error) {
	ctx = s.contentContext(ctx)
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, path, params, 1, 1)
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	_ = limit
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	var change eventstore.Change
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil || project.DeletedAt != nil {
			if err == nil {
				err = corestore.ErrNotFound
			}
			return err
		}
		row, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		version, err = r.CollaborationVersion(ctx, item, row.Revision)
		if err != nil {
			return err
		}
		change, err = r.LatestChange(ctx, scope.Project, row.NativeID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return tracker.NativeRecovery{}, hubapi.ProjectRecordError(err)
	}
	if change.ID != "" {
		visibleChange, visible, err := s.filterChangeRecord(ctx, scope, tracker.ChangeRequest{OrganizationID: change.OrganizationID, ProjectID: change.ProjectID, WorkItemID: change.WorkItemID, ID: change.ID})
		if err != nil {
			return tracker.NativeRecovery{}, err
		}
		if !visible || !slices.Contains(visibleChange.LinkedIssues, row.NativeID) {
			return tracker.NativeRecovery{}, hubapi.NativeNotFound()
		}
	}
	var issue tracker.NativeIssue
	issue, err = s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	selection := recoveryEvidenceCursor{Change: change.ID, Version: hubapi.ValueOrEmpty(change.CurrentVersionID), IssueRevision: tracker.Revision(row.Revision)}
	if cursor.After != "" {
		if json.Unmarshal([]byte(cursor.After), &selection) != nil || selection.Section < 0 || selection.Section > 8 || selection.After < 0 || selection.Offset < 0 {
			return tracker.NativeRecovery{}, hubapi.NativePublicInvalid("Recovery cursor is invalid")
		}
		if selection.IssueRevision != tracker.Revision(row.Revision) || selection.Change != change.ID || selection.Version != hubapi.ValueOrEmpty(change.CurrentVersionID) {
			return tracker.NativeRecovery{}, hubapi.NativeConflict(tracker.Revision(row.Revision))
		}
	}
	page := &tracker.NativeRecoveryPage{NativeReference: issue.NativeReference, Section: "complete"}
	for selection.Section < 8 {
		sequence, section, id, versionID, raw, err := s.readRecoveryEvidenceRecordTenant(ctx, scope, row, change, issue.Body, &selection)
		if errors.Is(err, sql.ErrNoRows) {
			if selection.Offset != 0 {
				return tracker.NativeRecovery{}, hubapi.NativeConflict(0)
			}
			selection.Section++
			selection.After, selection.Offset, selection.Digest, selection.Attempt = 0, 0, "", nil
			continue
		}
		if err != nil {
			return tracker.NativeRecovery{}, err
		}
		page.Section, page.RecordID, page.VersionID = section, id, versionID
		if err := recoveryRecordFragment(page, raw, selection.Offset, selection.Digest); err != nil {
			return tracker.NativeRecovery{}, err
		}
		end := page.Offset + int64(len(page.Data))
		if end == int64(len(raw)) {
			selection.After, selection.Offset, selection.Digest, selection.Attempt = sequence, 0, "", nil
		} else {
			selection.Offset, selection.Digest = end, page.SHA256
		}
		position, err := json.Marshal(selection)
		if err != nil {
			return tracker.NativeRecovery{}, err
		}
		cursor.After = string(position)
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.NativeRecovery{}, err
		}
		break
	}
	return tracker.NativeRecovery{Continuation: page}, nil
}

func (s *Service) readRecoveryEvidenceRecordTenant(ctx context.Context, scope hubscope.NativeScope, item corestore.WorkItem, change eventstore.Change, body string, cursor *recoveryEvidenceCursor) (int64, string, string, string, []byte, error) {
	var sequence int64
	var section, id, versionID string
	var hydrate func() ([]byte, error)
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		current, err := r.WorkItem(ctx, scope.Project, item.NativeID)
		if err != nil {
			return err
		}
		if current.ID != item.ID || current.Revision != item.Revision || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
			return corestore.ErrRevisionConflict
		}
		latest, err := r.LatestChange(ctx, scope.Project, item.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if latest.ID != change.ID || hubapi.ValueOrEmpty(latest.CurrentVersionID) != hubapi.ValueOrEmpty(change.CurrentVersionID) {
			return corestore.ErrRevisionConflict
		}
		switch cursor.Section {
		case 0:
			if change.ID == "" {
				return sql.ErrNoRows
			}
			rows, err := r.Evidence(ctx, change.ID, cursor.After, 1)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return sql.ErrNoRows
			}
			row := rows[0]
			if row.OrganizationID != scope.Organization || row.ChangeID != change.ID {
				return artifact.ErrIntegrity
			}
			section = map[string]string{"review": "reviews", "check": "checks", "discussion": "change_discussion"}[row.Kind]
			if section == "" {
				return eventstore.ErrInvalidRecord
			}
			sequence, id, versionID = row.Sequence, strconv.FormatInt(row.Sequence, 10), hubapi.ValueOrEmpty(row.VersionID)
			if versionID != "" {
				if _, err := r.Version(ctx, change.ID, versionID); err != nil {
					return err
				}
			}
			hydrate = func() ([]byte, error) {
				raw, err := content.Evidence(ctx, row)
				if err != nil {
					return nil, err
				}
				var detail tracker.ChangeDetail
				if err := appendBoundedChangeEvidence(&detail, row, raw); err != nil {
					return nil, err
				}
				return raw, nil
			}
		case 3:
			rows, err := r.Comments(ctx, scope.Project, item.NativeID, cursor.After, 1)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return sql.ErrNoRows
			}
			row := rows[0]
			version, err := r.CollaborationVersion(ctx, row.ID, row.Revision)
			if err != nil {
				return err
			}
			sequence, section, id = row.Sequence, "discussion", row.ID
			hydrate = func() ([]byte, error) {
				value, err := s.hydrateCommentRecord(ctx, row, version)
				if err != nil {
					return nil, err
				}
				return json.Marshal(value)
			}
		case 4:
			rows, err := r.AttemptPage(ctx, item.ID, leasestore.AttemptCursor{IssueID: item.ID, AfterFence: cursor.After, Oldest: true}, 1)
			if err != nil {
				return err
			}
			if len(rows.Items) == 0 {
				return sql.ErrNoRows
			}
			row := rows.Items[0]
			if row.OrganizationID != scope.Organization || row.IssueID != item.ID {
				return artifact.ErrIntegrity
			}
			sequence, section, id = row.FencingToken, "attempts", row.ID
			hydrate = func() ([]byte, error) {
				if cursor.Offset > 0 && cursor.Attempt == nil {
					return nil, hubapi.NativeConflict(tracker.Revision(item.Revision))
				}
				value, pin, err := s.readRetainedAttemptTenant(ctx, scope, string(item.NativeID), row.ID, cursor.Attempt)
				if err != nil {
					return nil, err
				}
				cursor.Attempt = pin
				return json.Marshal(value)
			}
		case 5:
			if change.ID == "" {
				return sql.ErrNoRows
			}
			rows, err := r.Versions(ctx, change.ID, cursor.After, 1)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return sql.ErrNoRows
			}
			row := rows[0]
			if row.OrganizationID != scope.Organization || row.ChangeID != change.ID {
				return artifact.ErrIntegrity
			}
			sequence, section, id, versionID = row.Number, "versions", row.ID, row.ID
			policyRow, err := r.ProjectPolicyRevision(ctx, scope.Project, row.PolicyRevisionID)
			if err != nil {
				return err
			}
			if policyRow.OrganizationID != scope.Organization || policyRow.ProjectID != scope.Project || policyRow.PolicyID != row.PolicyRevisionID {
				return artifact.ErrIntegrity
			}
			hydrate = func() ([]byte, error) {
				value, err := s.hydrateChangeVersion(ctx, scope, item.NativeID, row)
				if err != nil {
					return nil, err
				}
				value.Policy, err = content.ProjectPolicy(ctx, policyRow)
				if err != nil {
					return nil, err
				}
				return json.Marshal(value)
			}
		case 6:
			if change.ID == "" || cursor.After > 0 {
				return sql.ErrNoRows
			}
			sequence, section, id = 1, "change", change.ID
			hydrate = func() ([]byte, error) {
				value, err := content.Change(ctx, change)
				if err != nil {
					return nil, err
				}
				value, visible, err := s.filterChangeRecord(ctx, scope, value)
				if err == nil && (!visible || !slices.Contains(value.LinkedIssues, item.NativeID)) {
					err = hubapi.NativeNotFound()
				}
				if err != nil {
					return nil, err
				}
				return json.Marshal(value)
			}
		case 7:
			if cursor.After > 0 {
				return sql.ErrNoRows
			}
			sequence, section, id = 1, "issue_body", string(item.NativeID)
			hydrate = func() ([]byte, error) { return json.Marshal(body) }
		default:
			return sql.ErrNoRows
		}
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", "", nil, err
	}
	if err != nil {
		return 0, "", "", "", nil, hubapi.ProjectRecordError(err)
	}
	raw, err := hydrate()
	return sequence, section, id, versionID, raw, err
}

type checkpointAttemptCandidate struct {
	attempt leasestore.Attempt
	event   eventstore.Event
}

func recoveryMergeHistoryPageHasMore(page []eventstore.Event, index, pageLimit int) bool {
	for _, event := range page[index+1:] {
		if event.Type == "workflow.transitioned" && (event.FromState == "" || strings.EqualFold(event.FromState, "merging")) {
			return true
		}
	}
	return len(page) == pageLimit
}

func findRecoveryAttemptSnapshot(snapshots []hubrecords.TenantAttemptSnapshot, attemptID string) bool {
	for _, snapshot := range snapshots {
		if snapshot.Attempt.ID == attemptID {
			return true
		}
	}
	return false
}

func (s *Service) readNativeRecoveryAttemptSnapshot(ctx context.Context, scope hubscope.NativeScope, item string, issueID int64, attemptID string) (hubrecords.TenantAttemptSnapshot, error) {
	var snapshot hubrecords.TenantAttemptSnapshot
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubrecords.TenantAttemptSnapshot{}, hubapi.NativeRunEventStorageUnavailable()
	}
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		workItem, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if workItem.ID != issueID || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(workItem.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		snapshot.Attempt, err = r.Attempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if snapshot.Attempt.IssueID != issueID {
			return corestore.ErrNotFound
		}
		r, err = readAttemptMetadataSnapshot(ctx, r, scope, item, []leasestore.Attempt{snapshot.Attempt}, false)
		if err != nil {
			return err
		}
		lease, err := r.LeaseSnapshot(ctx, snapshot.Attempt.LeaseID)
		if err != nil {
			return err
		}
		snapshot.Lease, snapshot.LeaseObservation = lease.Lease, lease.LeaseObservation
		if lease.IssueID != issueID || lease.FencingToken != snapshot.Attempt.FencingToken {
			return corestore.ErrNotFound
		}
		snapshot.Event, err = r.LatestRunEvent(ctx, scope.Project, attemptID)
		if err == nil && (snapshot.Event.WorkItemID != tracker.NativeWorkItemID(item) || snapshot.Event.AttemptID != attemptID) {
			return corestore.ErrNotFound
		}
		if err != nil {
			return err
		}
		return hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot)
	})
	if err != nil {
		return hubrecords.TenantAttemptSnapshot{}, hubapi.ProjectRecordError(err)
	}
	return snapshot, nil
}
