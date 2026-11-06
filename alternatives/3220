package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sync"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type validationAuditComment struct {
	row     corestore.Comment
	version eventstore.CollaborationVersion
}

type validationAuditCandidate struct {
	change  eventstore.Change
	version *eventstore.Version
	receipt *eventstore.LandingReceipt
	binding *leasestore.Attempt
	event   *eventstore.Event
}

func (s *Service) readValidationAuditRecord(ctx context.Context, scope nativeScope, item tracker.NativeIssue, attempt *tracker.NativeAttempt) (*tracker.ValidationAudit, error) {
	audit := &tracker.ValidationAudit{Source: "verified_tenant_collaboration_and_change_records", Coverage: "selected_issue_revision_and_newest_20_current_comment_sequences; current_change_or_selected_attempt_landing; other_project_scheduled_sources_and_change_versions_unavailable; candidate_only; source_inclusion_and_causality_unverified", Partial: true, Unavailable: []string{"project_wide_scheduled_sources", "other_change_versions"}, Occurrences: []tracker.ValidationOccurrence{}}
	if s.config.TenantRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	if item.OrganizationID != scope.organization || item.ProjectID != scope.project || item.WorkItemID == "" {
		return nil, artifact.ErrIntegrity
	}
	comments := []validationAuditComment{}
	candidate := validationAuditCandidate{}
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
		current, err := r.WorkItem(ctx, scope.project, item.WorkItemID)
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(current.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		rows, err := r.SelectComments(ctx, scope.project, item.WorkItemID, math.MaxInt64, true, 21)
		if err != nil {
			return err
		}
		if len(rows) > 20 {
			rows = rows[:20]
			audit.Unavailable = append(audit.Unavailable, "older_comment_sequences")
		}
		keys := make([]eventstore.CollaborationVersionKey, len(rows))
		for i, row := range rows {
			keys[i] = eventstore.CollaborationVersionKey{RecordID: row.ID, Revision: row.Revision}
		}
		reader, ok := r.(eventstore.CollaborationVersionsReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		versions, err := reader.CollaborationVersions(ctx, keys)
		if err != nil {
			return err
		}
		byKey := map[eventstore.CollaborationVersionKey]eventstore.CollaborationVersion{}
		for _, version := range versions {
			byKey[eventstore.CollaborationVersionKey{RecordID: version.RecordID, Revision: version.Revision}] = version
		}
		budget := int64(32 * 1024 * 1024)
		for _, row := range rows {
			version, ok := byKey[eventstore.CollaborationVersionKey{RecordID: row.ID, Revision: row.Revision}]
			if !ok {
				return artifact.ErrIntegrity
			}
			if version.PayloadContentBytes > budget {
				audit.Unavailable = append(audit.Unavailable, "comment_payload_budget")
				break
			}
			budget -= version.PayloadContentBytes
			comments = append(comments, validationAuditComment{row, version})
		}
		var receipt eventstore.LandingReceipt
		if attempt != nil {
			receipt, err = r.LandingReceipt(ctx, attempt.AttemptID)
		} else {
			receipt, err = r.LatestLandingReceipt(ctx, scope.project, item.WorkItemID)
		}
		if err == nil {
			if receipt.OrganizationID != scope.organization || attempt != nil && receipt.AttemptID != attempt.AttemptID {
				return artifact.ErrIntegrity
			}
			binding, err := r.Attempt(ctx, receipt.AttemptID)
			if err != nil {
				return err
			}
			if binding.OrganizationID != scope.organization || binding.IssueID != current.ID {
				return artifact.ErrIntegrity
			}
			candidate.binding = &binding
			if attempt == nil {
				event, err := r.LatestRunEvent(ctx, scope.project, binding.ID)
				if err == nil {
					if event.OrganizationID != scope.organization || event.ProjectID != scope.project || event.WorkItemID != item.WorkItemID || event.AttemptID != binding.ID {
						return artifact.ErrIntegrity
					}
					candidate.event = &event
				} else if !errors.Is(err, corestore.ErrNotFound) {
					return err
				} else {
					audit.Unavailable = append(audit.Unavailable, "local_attempt_reference")
				}
			}
			candidate.receipt = &receipt
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		} else if attempt != nil {
			audit.Unavailable = append(audit.Unavailable, "selected_attempt_landing_receipt")
		}
		change, err := r.LatestChange(ctx, scope.project, item.WorkItemID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		candidate.change = change
		if change.CurrentVersionID != nil {
			version, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return err
			}
			candidate.version = &version
		}
		return nil
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	sources := make([]tracker.ValidationSource, len(comments)+1)
	bodies := make([]string, len(sources))
	sources[0] = tracker.ValidationSource{Actor: item.Actor, Kind: "issue", WorkItemID: item.WorkItemID, RecordID: string(item.WorkItemID), Revision: item.Revision, ObservedAt: item.UpdatedAt}
	bodies[0] = item.Body
	failures := make([]error, len(comments))
	var workers sync.WaitGroup
	for worker := 0; worker < min(4, len(comments)); worker++ {
		workers.Go(func() {
			for i := worker; i < len(comments); i += 4 {
				comment, err := s.hydrateValidationAuditComment(ctx, comments[i])
				if err != nil {
					failures[i] = err
					continue
				}
				sources[i+1] = tracker.ValidationSource{Actor: comment.Actor, Kind: "comment", WorkItemID: comment.WorkItemID, RecordID: comment.ID, Revision: comment.Revision, ObservedAt: comments[i].row.UpdatedAt}
				bodies[i+1] = comment.Body
			}
		})
	}
	workers.Wait()
	if err := errors.Join(failures...); err != nil {
		return nil, err
	}
	comparison, repository, found, err := s.hydrateValidationAuditCandidate(ctx, scope, item.WorkItemID, candidate, attempt)
	if err != nil {
		return nil, err
	}
	if !found {
		audit.Unavailable = append(audit.Unavailable, "current_change_validation")
	}
	seen := map[string]bool{}
	for i, body := range bodies {
		origin, ok := issueorigin.Parse(body)
		if !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" {
			continue
		}
		for _, scheduled := range gate.ParseScheduledEvidence(body) {
			if !scheduledRepairOriginMatches(origin.Source, scheduled) || seen[scheduled.OccurrenceKey] {
				continue
			}
			if len(audit.Occurrences) == 40 {
				break
			}
			seen[scheduled.OccurrenceKey] = true
			occurrence := tracker.ValidationOccurrence{Source: sources[i], Scheduled: scheduled, Comparisons: []tracker.ValidationComparison{}}
			if found && repository == scheduled.Repository {
				failed := []int{}
				for j, check := range scheduled.Checks {
					if check.ExitCode != 0 {
						failed = append(failed, j)
					}
				}
				if len(failed) == 0 {
					value := comparison
					value.CheckComparison = gate.CompareCheck(value.Local, value.ObservedAt, scheduled, nil)
					occurrence.Comparisons = append(occurrence.Comparisons, value)
				}
				for _, j := range failed {
					value := comparison
					value.CheckIndex = new(j)
					value.CheckComparison = gate.CompareCheck(value.Local, value.ObservedAt, scheduled, &scheduled.Checks[j])
					occurrence.Comparisons = append(occurrence.Comparisons, value)
				}
				for j := range occurrence.Comparisons {
					occurrence.Comparisons[j].IntegratedSource = "unknown"
					if comparison.IntegratedHeadSHA != "" {
						occurrence.Comparisons[j].IntegratedSource = "different_commit"
						if comparison.IntegratedHeadSHA == scheduled.HeadSHA {
							occurrence.Comparisons[j].IntegratedSource = "same_commit"
						}
					}
				}
			}
			audit.Occurrences = append(audit.Occurrences, occurrence)
		}
	}
	if len(audit.Occurrences) == 0 {
		audit.Unavailable = append(audit.Unavailable, "selected_issue_scheduled_evidence")
	}
	for {
		raw, err := json.Marshal(audit)
		if err != nil {
			return nil, err
		}
		if len(raw) <= 64*1024 {
			return audit, nil
		}
		if !slices.Contains(audit.Unavailable, "audit_payload_budget") {
			audit.Unavailable = append(audit.Unavailable, "audit_payload_budget")
		}
		last := len(audit.Occurrences) - 1
		if len(audit.Occurrences[last].Comparisons) > 1 {
			audit.Occurrences[last].Comparisons = audit.Occurrences[last].Comparisons[:len(audit.Occurrences[last].Comparisons)-1]
		} else {
			audit.Occurrences = audit.Occurrences[:last]
		}
	}
}

func (s *Service) hydrateValidationAuditCandidate(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, rows validationAuditCandidate, attempt *tracker.NativeAttempt) (tracker.ValidationComparison, string, bool, error) {
	result := tracker.ValidationComparison{WorkItemID: item, IntegratedSource: "unknown", Attribution: "candidate_only; source_inclusion_and_causality_unverified"}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	var landing *tracker.NativeLandingReceipt
	if rows.receipt != nil {
		value, err := content.LandingReceipt(ctx, *rows.receipt)
		if err != nil {
			return result, "", false, err
		}
		if value.ChangeID == "" {
			return result, "", false, artifact.ErrIntegrity
		}
		landing = &value
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			change, err := r.Change(ctx, value.ChangeID)
			if err != nil {
				return err
			}
			if change.ProjectID != scope.project {
				return artifact.ErrIntegrity
			}
			version, err := r.Version(ctx, change.ID, value.VersionID)
			if err != nil {
				return err
			}
			rows.change, rows.version = change, &version
			return nil
		})
		if err != nil {
			return result, "", false, projectRecordError(err)
		}
	}
	if rows.version == nil {
		return result, "", false, nil
	}
	change, err := content.Change(ctx, rows.change)
	if err != nil {
		return result, "", false, err
	}
	change, visible, err := s.filterChangeRecord(ctx, scope, change)
	if err != nil {
		return result, "", false, err
	}
	if !visible || !slices.Contains(change.LinkedIssues, item) {
		return result, "", false, nativeNotFound()
	}
	version, err := content.Version(ctx, *rows.version)
	if err != nil {
		return result, "", false, err
	}
	if version.Repository != rows.version.Repository || version.AttemptID != rows.version.AttemptID || version.ChangeID != change.ID {
		return result, "", false, artifact.ErrIntegrity
	}
	result.ChangeID, result.VersionID, result.VersionAttemptID, result.VersionRunID, result.ReviewedHeadSHA = version.ChangeID, version.ID, version.AttemptID, version.RunID, version.HeadSHA
	result.Local, result.ObservedAt = version.Validation, version.CreatedAt
	if landing != nil {
		if landing.Gate != nil {
			result.Local, result.ObservedAt = landing.Gate, landing.ObservedAt
		}
		result.IntegratedHeadSHA = landing.MergeSHA
		result.NativeAttemptID = rows.receipt.AttemptID
		if rows.binding != nil {
			result.NativeRunID = rows.binding.RunID
		}
		if rows.event != nil {
			value, err := content.Event(ctx, *rows.event)
			if err != nil {
				return result, "", false, err
			}
			run := value.Data.Run
			if run == nil || rows.binding == nil || run.AttemptID != rows.binding.ID || run.RunID != rows.binding.RunID || string(run.LeaseID) != rows.binding.LeaseID || int64(run.FencingToken) != rows.binding.FencingToken || run.Sequence != rows.binding.Sequence {
				return result, "", false, artifact.ErrIntegrity
			}
			if run.Runtime != nil {
				result.LocalAttemptID = run.Runtime.LocalAttemptID
			}
		}
		if attempt != nil {
			if attempt.AttemptID != rows.receipt.AttemptID {
				return result, "", false, artifact.ErrIntegrity
			}
			result.NativeRunID = attempt.RunID
			if attempt.Runtime != nil {
				result.LocalAttemptID = attempt.Runtime.LocalAttemptID
			}
		}
	} else if change.Landed != nil && change.Landed.VersionID == version.ID {
		result.IntegratedHeadSHA = change.Landed.MergeSHA
	}
	if result.Local != nil {
		value := *result.Local
		value.Output = ""
		if !value.Evidence.Valid() {
			value.Evidence = nil
		}
		value.Command = tracker.PublicValidationCommand(value.Command)
		result.Local = &value
	}
	return result, validationRepository(version.Repository), true, nil
}

func (s *Service) hydrateValidationAuditComment(ctx context.Context, input validationAuditComment) (tracker.NativeComment, error) {
	row, version := input.row, input.version
	var comment tracker.NativeComment
	if version.OrganizationID != row.OrganizationID || version.ProjectID != row.ProjectID || version.WorkItemID != row.WorkItemID || version.RecordID != row.ID || version.Revision != row.Revision {
		return comment, artifact.ErrIntegrity
	}
	raw, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).CollaborationVersion(ctx, version)
	if err != nil {
		return comment, err
	}
	if json.Unmarshal(raw, &comment) != nil || comment.OrganizationID != row.OrganizationID || comment.ProjectID != row.ProjectID || comment.WorkItemID != row.WorkItemID || comment.ID != row.ID || int64(comment.Revision) != row.Revision || comment.Sequence != row.Sequence || comment.Deleted != row.Deleted || comment.Actor.Kind != row.ActorKind || comment.Actor.PrincipalID != row.ActorPrincipalID {
		return comment, artifact.ErrIntegrity
	}
	return comment, nil
}
