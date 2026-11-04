package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenantAttemptSnapshot struct {
	attempt      leasestore.Attempt
	lease        leasestore.Lease
	event        eventstore.Event
	evidence     bool
	evidenceRows []attachmentstore.Record
}

func (s *Service) readNativeAttemptTenant(ctx context.Context, scope nativeScope, item, id string) (tracker.NativeAttempt, error) {
	var snapshot tenantAttemptSnapshot
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
		workItem, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(workItem.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		snapshot.attempt, err = r.Attempt(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.attempt.IssueID != workItem.ID {
			return nativeNotFound()
		}
		snapshot.lease, err = r.Lease(ctx, snapshot.attempt.LeaseID)
		if err != nil {
			return err
		}
		if snapshot.lease.IssueID != workItem.ID || snapshot.lease.FencingToken != snapshot.attempt.FencingToken {
			return corestore.ErrNotFound
		}
		snapshot.event, err = r.LatestRunEvent(ctx, scope.project, snapshot.attempt.ID)
		if err == nil && snapshot.event.WorkItemID != tracker.NativeWorkItemID(item) {
			return corestore.ErrNotFound
		}
		if err != nil {
			return err
		}
		return readTenantAttemptEvidence(ctx, r, &snapshot)
	})
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	return s.hydrateNativeAttemptTenant(ctx, snapshot, s.config.now())
}

func (s *Service) readNativeAttemptPageTenant(ctx context.Context, scope nativeScope, item string, params url.Values, marshal func(any) ([]byte, error)) (nativeAttemptPage, error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/attempts"
	if err := validateNativeQuery(params, "view", "direction"); err != nil {
		return nativeAttemptPage{}, err
	}
	view := params.Get("view")
	if view != "" && view != "recovery" && view != "detail" && view != "blockers" && view != "recorded_blockers" {
		return nativeAttemptPage{}, nativePublicInvalid("view supports recovery, detail, blockers or recorded_blockers")
	}
	if view == "recovery" {
		ctx = context.WithValue(ctx, attemptSummaryContextKey{}, true)
	}
	budget := 1 << 20
	if view != "" && view != "detail" {
		budget = operatortool.WorkHistoryPageBytes
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return nativeAttemptPage{}, err
	}
	_, after, err := nativePageOrder(params, "a.fencing_token", cursor.After)
	if err != nil {
		return nativeAttemptPage{}, err
	}
	typedCursor := leasestore.AttemptCursor{Oldest: params.Get("direction") != "newest"}
	if after != "" {
		fence, parseErr := strconv.ParseInt(after, 10, 64)
		if parseErr != nil || fence < 0 {
			return nativeAttemptPage{}, nativePublicInvalid("Cursor is invalid")
		}
		if typedCursor.Oldest {
			typedCursor.AfterFence = fence
		} else {
			typedCursor.BeforeFence = fence
		}
	}

	var snapshots []tenantAttemptSnapshot
	var page leasestore.AttemptPage
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		workItem, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(workItem.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		typedCursor.IssueID = workItem.ID
		page, err = r.AttemptPage(ctx, workItem.ID, typedCursor, limit)
		if err != nil {
			return err
		}
		snapshots = make([]tenantAttemptSnapshot, 0, len(page.Items))
		for _, attempt := range page.Items {
			if attempt.IssueID != workItem.ID {
				return corestore.ErrNotFound
			}
			lease, err := r.Lease(ctx, attempt.LeaseID)
			if err != nil {
				return err
			}
			if lease.IssueID != workItem.ID || lease.FencingToken != attempt.FencingToken {
				return corestore.ErrNotFound
			}
			event, err := r.LatestRunEvent(ctx, scope.project, attempt.ID)
			if err != nil {
				return err
			}
			if event.WorkItemID != tracker.NativeWorkItemID(item) {
				return corestore.ErrNotFound
			}
			snapshot := tenantAttemptSnapshot{attempt: attempt, lease: lease, event: event}
			if view != "recorded_blockers" && !attemptSummaryRead(ctx) {
				if err := readTenantAttemptEvidence(ctx, r, &snapshot); err != nil {
					return err
				}
			}
			snapshots = append(snapshots, snapshot)
		}
		return nil
	})
	if err != nil {
		return nativeAttemptPage{}, err
	}
	result := nativeAttemptPage{Page: tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{}}, encoded: []byte(`{"items":[`)}
	hasMore := page.Next != (leasestore.AttemptCursor{})
	for _, snapshot := range snapshots {
		attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, s.config.now())
		if err != nil {
			return nativeAttemptPage{}, err
		}
		if view != "" && view != "detail" {
			attempt.Runtime = nil
			attempt.RuntimeFreshness = "unavailable"
		}
		attempt.Identity = nativeAttemptDisplayIdentity(attempt)
		attempt.Runtime = attempt.Runtime.WithoutActivitySpans()
		next := cursor
		next.After = strconv.FormatInt(snapshot.attempt.FencingToken, 10)
		if typedCursor.Oldest {
			next.After = strconv.FormatInt(snapshot.attempt.FencingToken, 10)
		}
		nextCursor, err := encodeNativeCursor(next, key)
		if err != nil {
			return nativeAttemptPage{}, err
		}
		encoded, err := marshal(attempt)
		if err != nil {
			return nativeAttemptPage{}, err
		}
		if result.candidateBytes(encoded, nextCursor) > budget && len(result.Items) == 0 && view != "" && view != "detail" {
			attempt = nativeAttemptAuthorityProjection(attempt)
			encoded, err = marshal(attempt)
			if err != nil {
				return nativeAttemptPage{}, err
			}
		}
		if result.candidateBytes(encoded, nextCursor) > budget {
			if len(result.Items) == 0 {
				return nativeAttemptPage{}, &nativeError{Code: "attempt_record_too_large", Message: fmt.Sprintf("Work item %s attempt %s exceeds the %d-byte page limit", item, attempt.AttemptID, budget), status: http.StatusUnprocessableEntity, publicMessage: true}
			}
			hasMore = true
			break
		}
		result.appendAttempt(attempt, encoded)
		cursor = next
	}
	if hasMore && len(result.Items) > 0 {
		result.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return nativeAttemptPage{}, err
		}
	}
	result.finish()
	return result, nil
}

func (s *Service) hydrateNativeAttemptTenant(ctx context.Context, snapshot tenantAttemptSnapshot, now time.Time) (tracker.NativeAttempt, error) {
	if s.runDetailStore() == nil {
		return tracker.NativeAttempt{}, nativeRunEventStorageUnavailable()
	}
	value, err := eventstore.NewContent(s.runDetailStore()).Event(ctx, snapshot.event)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	if value.Data.Run == nil {
		return tracker.NativeAttempt{}, fmt.Errorf("hydrate attempt run: %w", artifact.ErrIntegrity)
	}
	run := *value.Data.Run
	attempt := snapshot.attempt
	lease := snapshot.lease
	if run.AttemptID != attempt.ID || run.RunID != attempt.RunID || run.LeaseID != tracker.LeaseID(lease.LeaseID) || run.FencingToken != tracker.FencingToken(lease.FencingToken) || run.Sequence != attempt.Sequence {
		return tracker.NativeAttempt{}, fmt.Errorf("hydrate attempt identity: %w", artifact.ErrIntegrity)
	}
	result := tracker.NativeAttempt{NativeRunData: run, Status: attempt.Status, StartedAt: attempt.StartedAt, UpdatedAt: attempt.UpdatedAt,
		WorkItemRevision: tracker.Revision(attempt.WorkItemRevision), DispatchGeneration: attempt.DispatchGeneration,
		LeaseRenewedAt: lease.RenewedAt, LeaseExpiresAt: lease.ExpiresAt, Current: lease.ReleasedAt == nil && !now.Before(lease.RenewedAt) && now.Before(lease.ExpiresAt),
		Checkpoint: run.Handoff, RuntimeFreshness: "unavailable",
		TerminalFailureAvailability: "unavailable", FinalizationAvailability: "unavailable", Unavailable: []string{"evidence"}}
	if snapshot.evidence {
		var evidence []tracker.NativeEvidence
		budget := 1 << 20
		for _, record := range snapshot.evidenceRows {
			if record.OrganizationID != attempt.OrganizationID || record.Metadata.ProjectID != string(snapshot.event.ProjectID) || record.Metadata.WorkItemID != string(snapshot.event.WorkItemID) || record.EvidenceAttemptID != attempt.ID || record.Metadata.Validate() != nil {
				return tracker.NativeAttempt{}, fmt.Errorf("hydrate attempt evidence: %w", artifact.ErrIntegrity)
			}
			file := tracker.NativeEvidence{AttachmentID: record.Metadata.ID, Caption: record.EvidenceCaption, Reference: record.Metadata.Markdown(string(attempt.OrganizationID))}
			raw, err := json.Marshal(file)
			if err != nil {
				return tracker.NativeAttempt{}, err
			}
			budget -= len(raw) + 1
			if budget < 0 {
				return tracker.NativeAttempt{}, nativePublicInvalid("Attempt evidence exceeds the 1048576-byte read limit")
			}
			evidence = append(evidence, file)
		}
		result.Evidence = append(result.Evidence, evidence...)
		result.Unavailable = slices.DeleteFunc(result.Unavailable, func(value string) bool { return value == "evidence" })
	}
	result.ArtifactIDs = run.ArtifactIDs
	if lease.ReleasedAt != nil {
		released := *lease.ReleasedAt
		result.ClaimReleasedAt = &released
	}
	if result.TerminalFailure != nil {
		failure := result.TerminalFailure.Public()
		result.TerminalFailure = &failure
		result.TerminalFailureAvailability = "available"
	}
	if result.Finalization != nil {
		finalization := result.Finalization.Public()
		result.Finalization = &finalization
		result.FinalizationAvailability = "available"
	}
	if result.Runtime != nil {
		result.RuntimeFreshness = "available"
		heartbeat := result.Runtime.HeartbeatAt
		if heartbeat.IsZero() || now.Before(heartbeat) || result.Status == "running" && (!result.Current || now.Sub(heartbeat) >= lease.ExpiresAt.Sub(lease.RenewedAt)) {
			result.RuntimeFreshness = "expired"
		}
	}
	if result.Status == "running" && !result.Current {
		result.Status = "interrupted"
	}
	return result, nil
}

func readTenantAttemptEvidence(ctx context.Context, r tenantstore.Reader, snapshot *tenantAttemptSnapshot) error {
	records, err := r.AttachmentEvidence(ctx, snapshot.event.ProjectID, snapshot.event.WorkItemID, snapshot.attempt.ID, 201)
	if err != nil {
		return err
	}
	if len(records) > 200 {
		return nativePublicInvalid("Attempt evidence exceeds the 200-reference read bound")
	}
	snapshot.evidence, snapshot.evidenceRows = true, records
	return nil
}
