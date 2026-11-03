package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func (s *Service) getNativeRecoveryTenant(c echo.Context, params url.Values) error {
	if validateNativeQuery(params, "view", "projection", "detail") != nil {
		return s.nativeAPIError(c, nativePublicInvalid("view supports recovery"))
	}
	if params.Get("detail") != "" && params.Get("detail") != "full" && params.Get("detail") != "summary" {
		return s.nativeAPIError(c, nativePublicInvalid("detail supports full or summary"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	ctx = s.runDetailContext(ctx)
	ctx = s.contentContext(ctx)
	if params.Get("detail") == "summary" || params.Get("detail") == "" && c.Request().Header.Get("X-Detent-Recovery-Detail") == "summary" {
		ctx = context.WithValue(ctx, attemptSummaryContextKey{}, true)
	}
	if params.Has("projection") || c.Request().Header.Get("X-Detent-Recovery-Projection") != "" {
		if params.Get("projection") != "bounded" && c.Request().Header.Get("X-Detent-Recovery-Projection") != "bounded" || params.Has("limit") || params.Has("cursor") {
			return s.nativeAPIError(c, nativePublicInvalid("Recovery projection supports bounded"))
		}
		result, err := s.readNativeRecoveryTenant(ctx, nativeRequestScope(c), c.Param("item"))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		result.Issue = s.nativeIssueResponse(result.Issue)
		if err := s.fitNativeRecovery(ctx, nativeRequestScope(c), c.Request().URL.EscapedPath(), &result, c.Request().Header.Get("X-Detent-Recovery-Body") == "fragments"); err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	if params.Has("limit") || params.Has("cursor") {
		result, err := s.readNativeRecoveryPageTenant(ctx, nativeRequestScope(c), c.Param("item"), c.Request().URL.EscapedPath(), params)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	result, err := s.readNativeRecoveryTenant(ctx, nativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result.Issue = s.nativeIssueResponse(result.Issue)
	return c.JSON(http.StatusOK, result)
}

func (s *Service) readNativeRecoveryTenant(ctx context.Context, scope nativeScope, item string) (tracker.NativeRecovery, error) {
	var result tracker.NativeRecovery
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	var change eventstore.Change
	var changeVersion *eventstore.Version
	var policyRows currentChangePolicyRows
	var comments []corestore.Comment
	var commentVersions []eventstore.CollaborationVersion
	var commentsTruncated bool
	var snapshots []tenantAttemptSnapshot
	var attemptPage leasestore.AttemptPage
	var checkpointCandidates []checkpointAttemptCandidate
	var checkpointPageTruncated bool
	var landingReceipt eventstore.LandingReceipt
	var latestAttemptID string
	var events []eventstore.Event
	var mergeHistoryTruncated bool
	var projectSequence int64
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		row, err = r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		version, err = r.CollaborationVersion(ctx, item, row.Revision)
		if err != nil {
			return err
		}
		change, err = r.LatestChange(ctx, scope.project, row.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			if change.OrganizationID != scope.organization || change.ProjectID != scope.project {
				return corestore.ErrTenantMismatch
			}
			if change.CurrentVersionID != nil {
				value, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
				if err != nil {
					return err
				}
				changeVersion = &value
			}
			policyRows, err = readCurrentChangePolicyRows(ctx, r, scope)
			if err != nil {
				return err
			}
		}
		comments = make([]corestore.Comment, 0, 33)
		latestMachine := false
		beforeSequence := int64(1<<63 - 1)
		commentScanComplete := false
		for range claimPreparationLimit {
			page, err := r.SelectComments(ctx, scope.project, row.NativeID, beforeSequence, true, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, comment := range page {
				keep := comment.ActorKind == "human" || stringValue(comment.EditedByKind) == "human" || !latestMachine
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
			events, err := r.LatestCheckpointEvents(ctx, scope.project, row.NativeID, attemptIDs)
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
				if event.OrganizationID != scope.organization || event.ProjectID != scope.project || event.WorkItemID != row.NativeID || event.AttemptID != attempt.ID || event.Type != "run.checkpointed" {
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
		landingReceipt, err = r.LatestLandingReceipt(ctx, scope.project, row.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			if landingReceipt.OrganizationID != scope.organization || landingReceipt.AttemptID == "" || landingReceipt.VersionID == "" {
				return eventstore.ErrInvalidRecord
			}
			if err := ensureAttempt(landingReceipt.AttemptID); err != nil {
				return err
			}
		}
		snapshots = make([]tenantAttemptSnapshot, 0, len(wantedAttempts))
		for _, attempt := range attemptPage.Items {
			if _, wanted := wantedAttempts[attempt.ID]; !wanted {
				continue
			}
			if attempt.IssueID != row.ID {
				return corestore.ErrNotFound
			}
			lease, err := r.Lease(ctx, attempt.LeaseID)
			if err != nil {
				return err
			}
			if lease.IssueID != row.ID || lease.FencingToken != attempt.FencingToken {
				return corestore.ErrNotFound
			}
			event, err := r.LatestRunEvent(ctx, scope.project, attempt.ID)
			if err != nil {
				return err
			}
			if event.WorkItemID != row.NativeID {
				return corestore.ErrNotFound
			}
			snapshots = append(snapshots, tenantAttemptSnapshot{attempt: attempt, lease: lease, event: event})
		}
		projectSequence, err = r.ProjectEventSequence(ctx, scope.project)
		if err != nil {
			return err
		}
		if projectSequence == 0 {
			return nil
		}
		before := projectSequence + 1
		for range claimPreparationLimit {
			page, err := r.EventsBefore(ctx, scope.project, row.NativeID, before, corestore.MaxPageSize)
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
		return result, fmt.Errorf("read PostgreSQL recovery facts: %w", projectRecordError(err))
	}
	result.Issue, err = s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return result, fmt.Errorf("hydrate recovery work item: %w", err)
	}
	if err = s.hydrateWorkItemRelationshipFacts(ctx, scope, &result.Issue); err != nil {
		return result, fmt.Errorf("hydrate recovery work item relationships: %w", err)
	}
	result.Issue.OmittedFields = append(result.Issue.OmittedFields, "change_sequence")
	if change.ID != "" {
		var detail tracker.ChangeDetail
		detail, err = s.hydrateCurrentChangeDetail(ctx, scope, row.NativeID, change, changeVersion, policyRows, s.config.now())
		if err != nil {
			return result, fmt.Errorf("hydrate recovery current change: %w", err)
		}
		result.ChangeDetail = &detail
		if changeVersion != nil {
			result.Change = &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: changeVersion.ID, HeadSHA: changeVersion.HeadSHA}
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
	if scope.credential.Runner.RunnerID != "" {
		result.Read.Omitted = append(result.Read.Omitted, "source_version_id", "instance_successes")
	}
	result.Read.DetailPath = "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "?view=recovery&limit=1"
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
		selected[snapshots[0].attempt.ID] = struct{}{}
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
		if _, ok := selected[snapshot.attempt.ID]; !ok {
			continue
		}
		attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, s.config.now())
		if err != nil {
			return result, fmt.Errorf("hydrate selected recovery attempt: %w", err)
		}
		attempt.Runtime = attempt.Runtime.WithoutActivitySpans()
		if checkpoint, ok := checkpointsByAttempt[snapshot.attempt.ID]; ok {
			attempt.Checkpoint = &checkpoint
		}
		if snapshot.attempt.ID == landingAttemptID && landingValue != nil {
			if attempt.Runtime == nil {
				attempt.Runtime = &tracker.NativeRuntimeObservation{}
			}
			attempt.Runtime.Landing = landingValue
		}
		hydrated[snapshot.attempt.ID] = attempt
	}
	ordered := make([]leasestore.Attempt, 0, len(selected))
	for _, snapshot := range snapshots {
		if _, ok := selected[snapshot.attempt.ID]; ok {
			ordered = append(ordered, snapshot.attempt)
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
	if err := projectNativeRecovery(&result); err != nil {
		return result, fmt.Errorf("project recovery response: %w", err)
	}
	return result, nil
}

func (s *Service) readNativeRecoveryPageTenant(ctx context.Context, scope nativeScope, item, path string, params url.Values) (tracker.NativeRecovery, error) {
	ctx = s.contentContext(ctx)
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, path, params, 1, 1)
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	_ = limit
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	var change eventstore.Change
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil || project.DeletedAt != nil {
			if err == nil {
				err = corestore.ErrNotFound
			}
			return err
		}
		row, err = r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		version, err = r.CollaborationVersion(ctx, item, row.Revision)
		if err != nil {
			return err
		}
		change, err = r.LatestChange(ctx, scope.project, row.NativeID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return tracker.NativeRecovery{}, projectRecordError(err)
	}
	var issue tracker.NativeIssue
	issue, err = s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	selection := recoveryEvidenceCursor{}
	if cursor.After != "" {
		if err := json.Unmarshal([]byte(cursor.After), &selection); err != nil || selection.Section != 7 || selection.After < 0 || selection.Offset < 0 || selection.IssueRevision != tracker.Revision(row.Revision) {
			return tracker.NativeRecovery{}, nativePublicInvalid("Recovery cursor is invalid")
		}
		if selection.Change != "" && (change.ID != selection.Change || selection.Version != valueOrEmpty(change.CurrentVersionID)) {
			return tracker.NativeRecovery{}, nativeConflict(0)
		}
	}
	if selection.Change == "" {
		selection.Change, selection.Version = change.ID, valueOrEmpty(change.CurrentVersionID)
	}
	encodedBody, err := json.Marshal(issue.Body)
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	page := &tracker.NativeRecoveryPage{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: row.NativeID, Revision: tracker.Revision(row.Revision)}, Section: "issue_body", RecordID: item, VersionID: selection.Version}
	if err := recoveryRecordFragment(page, encodedBody, selection.Offset, selection.Digest); err != nil {
		return tracker.NativeRecovery{}, err
	}
	end := page.Offset + int64(len(page.Data))
	if end < int64(len(encodedBody)) {
		selection.Section = 7
		selection.Offset, selection.Digest = end, page.SHA256
	} else {
		selection.Section = 8
		selection.Offset, selection.After, selection.Digest = 0, 1, ""
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
	return tracker.NativeRecovery{Continuation: page}, nil
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

func findRecoveryAttemptSnapshot(snapshots []tenantAttemptSnapshot, attemptID string) bool {
	for _, snapshot := range snapshots {
		if snapshot.attempt.ID == attemptID {
			return true
		}
	}
	return false
}

func (s *Service) readNativeRecoveryAttemptSnapshot(ctx context.Context, scope nativeScope, item string, issueID int64, attemptID string) (tenantAttemptSnapshot, error) {
	var snapshot tenantAttemptSnapshot
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		workItem, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if workItem.ID != issueID || !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(workItem.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		snapshot.attempt, err = r.Attempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if snapshot.attempt.IssueID != issueID {
			return corestore.ErrNotFound
		}
		snapshot.lease, err = r.Lease(ctx, snapshot.attempt.LeaseID)
		if err != nil {
			return err
		}
		if snapshot.lease.IssueID != issueID || snapshot.lease.FencingToken != snapshot.attempt.FencingToken {
			return corestore.ErrNotFound
		}
		snapshot.event, err = r.LatestRunEvent(ctx, scope.project, attemptID)
		if err == nil && (snapshot.event.WorkItemID != tracker.NativeWorkItemID(item) || snapshot.event.AttemptID != attemptID) {
			return corestore.ErrNotFound
		}
		return err
	})
	if err != nil {
		return tenantAttemptSnapshot{}, projectRecordError(err)
	}
	return snapshot, nil
}
