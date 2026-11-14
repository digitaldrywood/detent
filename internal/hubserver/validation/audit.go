package validation

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
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

func (s *Service) AuditRecord(ctx context.Context, scope hubapi.Scope, item tracker.NativeIssue, attempt *tracker.NativeAttempt) (*tracker.ValidationAudit, error) {
	if item.OrganizationID != scope.Organization || item.ProjectID != scope.Project || item.WorkItemID == "" {
		return nil, artifact.ErrIntegrity
	}
	attemptID := ""
	if attempt != nil {
		attemptID = attempt.AttemptID
		if attemptID == "" {
			return nil, artifact.ErrIntegrity
		}
	}
	scheduledItem := ""
	if strings.Contains(item.Body, "detent-checks") {
		scheduledItem = string(item.WorkItemID)
	}
	localItem := string(item.WorkItemID)
	if attemptID == "" {
		landed, err := s.auditItemHasChange(ctx, scope, item.WorkItemID)
		if err != nil {
			return nil, err
		}
		if !landed {
			localItem = ""
		}
	}
	return s.AuditTenant(ctx, scope, scheduledItem, localItem, "", attemptID)
}

func (s *Service) auditItemHasChange(ctx context.Context, scope hubapi.Scope, item tracker.NativeWorkItemID) (bool, error) {
	if s.deps.Repository == nil {
		return false, hubapi.WorkItemStorageUnavailable()
	}
	snapshotRepository, ok := s.deps.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return false, hubapi.WorkItemStorageUnavailable()
	}
	found := false
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.Authorize(ctx, r, false); err != nil {
			return err
		}
		_, err := r.LatestChange(ctx, scope.Project, item)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return false, s.deps.RecordError(err)
	}
	return found, nil
}

func (s *Service) AuditTenant(ctx context.Context, scope hubapi.Scope, scheduledItem, localItem, changeID, attemptID string) (*tracker.ValidationAudit, error) {
	audit := &tracker.ValidationAudit{Source: "verified_tenant_collaboration_and_change_records", Coverage: "up_to_40_classified_current_scheduled_records; unresolved_current_sources_unavailable; matching_historical_landing_receipts_and_newest_20_candidate_receipts; candidate_only; source_inclusion_and_causality_unverified", Unavailable: []string{"unlanded_historical_versions", "historical_repository_identity"}, Partial: true, Occurrences: []tracker.ValidationOccurrence{}}
	if s.deps.Repository == nil {
		return nil, hubapi.WorkItemStorageUnavailable()
	}
	snapshotRepository, ok := s.deps.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, hubapi.WorkItemStorageUnavailable()
	}
	var localRow corestore.WorkItem
	var sourceVersions []eventstore.CollaborationVersion
	sourceItems := map[tracker.NativeWorkItemID]corestore.WorkItem{}
	sourceComments := map[string]corestore.Comment{}
	fallback := validationAuditCandidate{}
	budget := int64(32 * 1024 * 1024)
	reserved := map[string]int64{}
	reserve := func(key string, size int64) (bool, error) {
		if key == "" || size <= 0 {
			return false, artifact.ErrIntegrity
		}
		if previous, ok := reserved[key]; ok {
			if previous != size {
				return false, artifact.ErrIntegrity
			}
			return true, nil
		}
		if size > budget {
			return false, nil
		}
		reserved[key] = size
		budget -= size
		return true, nil
	}
	unavailable := func(value string) {
		if !slices.Contains(audit.Unavailable, value) {
			audit.Unavailable = append(audit.Unavailable, value)
		}
		audit.Partial = true
	}
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.Authorize(ctx, r, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) && scheduledItem == "" && localItem == "" && changeID == "" && attemptID == "" {
			unavailable("project_not_found")
			return nil
		}
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		if localItem != "" {
			localRow, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(localItem))
			if err != nil {
				return err
			}
		} else if attemptID != "" {
			attempt, err := r.Attempt(ctx, attemptID)
			if err != nil {
				return err
			}
			localRow, err = r.WorkItemByID(ctx, scope.Project, attempt.IssueID)
			if err != nil {
				return err
			}
		} else if changeID != "" {
			change, err := r.Change(ctx, changeID)
			if err != nil {
				return err
			}
			if change.ProjectID != scope.Project {
				return corestore.ErrNotFound
			}
			localRow, err = r.WorkItem(ctx, scope.Project, change.WorkItemID)
			if err != nil {
				return err
			}
		}
		if localRow.NativeID != "" && !reporterOwns(scope, tracker.Actor{PrincipalID: stringValue(localRow.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		if attemptID != "" {
			attempt, err := r.Attempt(ctx, attemptID)
			if err != nil {
				return err
			}
			if attempt.OrganizationID != scope.Organization || attempt.IssueID != localRow.ID {
				return corestore.ErrNotFound
			}
		}
		var change eventstore.Change
		if changeID != "" {
			change, err = r.Change(ctx, changeID)
		} else if localRow.NativeID != "" {
			change, err = r.LatestChange(ctx, scope.Project, localRow.NativeID)
		} else {
			err = corestore.ErrNotFound
		}
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project || change.WorkItemID != localRow.NativeID {
				if changeID != "" {
					return corestore.ErrNotFound
				}
				return artifact.ErrIntegrity
			}
			fallback.change = change
			if change.CurrentVersionID != nil {
				version, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
				if err != nil {
					return err
				}
				fallback.version = &version
			}
		}
		reader, ok := r.(eventstore.ScheduledSourceReader)
		if !ok {
			return hubapi.WorkItemStorageUnavailable()
		}
		var partial bool
		sourceVersions, partial, err = reader.ScheduledSources(ctx, scope.Project, tracker.NativeWorkItemID(scheduledItem))
		if errors.Is(err, eventstore.ErrScheduledSourceIncomplete) {
			return hubapi.WorkItemStorageUnavailable()
		}
		if err != nil {
			return err
		}
		if partial {
			unavailable("incomplete_scheduled_sources")
		}
		selected := sourceVersions[:0]
		for _, version := range sourceVersions {
			if version.OrganizationID != scope.Organization || version.ProjectID != scope.Project || !version.SourceKnown || !version.SourceScheduled {
				return artifact.ErrIntegrity
			}
			row, ok := sourceItems[version.WorkItemID]
			if !ok {
				row, err = r.WorkItem(ctx, scope.Project, version.WorkItemID)
				if err != nil {
					return err
				}
				sourceItems[version.WorkItemID] = row
			}
			if !reporterOwns(scope, tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
				unavailable("restricted_scheduled_sources")
				continue
			}
			if version.RecordID == string(version.WorkItemID) {
				if row.Revision != version.Revision {
					return artifact.ErrIntegrity
				}
			} else {
				comment, err := r.Comment(ctx, scope.Project, version.WorkItemID, version.RecordID)
				if err != nil {
					return err
				}
				if comment.Revision != version.Revision {
					return artifact.ErrIntegrity
				}
				sourceComments[version.RecordID] = comment
			}
			fits, err := reserve(version.PayloadObjectKey, version.PayloadContentBytes)
			if err != nil {
				return err
			}
			if !fits {
				unavailable("scheduled_source_payload_budget")
				break
			}
			selected = append(selected, version)
		}
		sourceVersions = selected
		return nil
	})
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	sources := make([]tracker.ValidationSource, 0, len(sourceVersions))
	bodies := make([]string, 0, len(sourceVersions))
	for _, version := range sourceVersions {
		var source tracker.ValidationSource
		var body string
		if version.RecordID == string(version.WorkItemID) {
			item, err := s.deps.HydrateWorkItem(ctx, sourceItems[version.WorkItemID], version)
			if err != nil {
				return nil, err
			}
			source = tracker.ValidationSource{Actor: item.Actor, Kind: "issue", WorkItemID: item.WorkItemID, RecordID: string(item.WorkItemID), Revision: item.Revision, ObservedAt: item.UpdatedAt}
			body = item.Body
		} else {
			comment, err := s.hydrateValidationAuditComment(ctx, validationAuditComment{sourceComments[version.RecordID], version})
			if err != nil {
				return nil, err
			}
			source = tracker.ValidationSource{Actor: comment.Actor, Kind: "comment", WorkItemID: comment.WorkItemID, RecordID: comment.ID, Revision: comment.Revision, ObservedAt: comment.UpdatedAt}
			body = comment.Body
		}
		if !source.ObservedAt.UTC().Truncate(time.Microsecond).Equal(version.SourceObservedAt) {
			return nil, artifact.ErrIntegrity
		}
		sources = append(sources, source)
		bodies = append(bodies, body)
	}
	cache := map[string][]tracker.ValidationComparison{}
	seen := map[string]bool{}
	payloadBytes := 0
	metadataBytes := func() (int, error) {
		value := *audit
		value.Occurrences = []tracker.ValidationOccurrence{}
		raw, err := json.Marshal(value)
		return len(raw), err
	}
sourcesLoop:
	for index, body := range bodies {
		origin, ok := issueorigin.Parse(body)
		if !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" {
			continue
		}
		for _, scheduled := range gate.ParseScheduledEvidence(body) {
			if !s.deps.OriginMatches(origin.Source, scheduled) || seen[scheduled.OccurrenceKey] {
				continue
			}
			if len(audit.Occurrences) == 40 {
				unavailable("scheduled_occurrence_budget")
				break
			}
			seen[scheduled.OccurrenceKey] = true
			repositoryIdentity := validationAuditRepository(scheduled.Repository)
			key := repositoryIdentity + "\x00" + scheduled.HeadSHA
			comparisons, cached := cache[key]
			if !cached {
				var page tenantstore.ValidationCandidatePage
				err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					if err := scope.Authorize(ctx, r, false); err != nil {
						return err
					}
					if localRow.NativeID != "" {
						current, err := r.WorkItem(ctx, scope.Project, localRow.NativeID)
						if err != nil {
							return err
						}
						if current.ID != localRow.ID || !reporterOwns(scope, tracker.Actor{PrincipalID: stringValue(current.ActorPrincipalID)}) {
							return corestore.ErrNotFound
						}
					} else if scope.HostedRole == "reporter" {
						return hubapi.WorkItemStorageUnavailable()
					}
					reader, ok := r.(tenantstore.ValidationCandidateReader)
					if !ok {
						return hubapi.WorkItemStorageUnavailable()
					}
					page, err = reader.ValidationCandidates(ctx, tenantstore.ValidationCandidateQuery{ProjectID: scope.Project, WorkItemID: localRow.NativeID, Repository: repositoryIdentity, HeadSHA: scheduled.HeadSHA, ChangeID: changeID, AttemptID: attemptID})
					return err
				})
				if err != nil {
					return nil, s.deps.RecordError(err)
				}
				audit.Partial = audit.Partial || page.Partial
				candidates := make([]validationAuditCandidate, 0, len(page.Rows)+1)
				for _, row := range page.Rows {
					version, receipt, binding := row.Version, row.Receipt, row.Attempt
					candidates = append(candidates, validationAuditCandidate{change: row.Change, version: &version, receipt: &receipt, binding: &binding, event: row.Event})
				}
				if len(candidates) == 0 && fallback.version != nil && validationAuditRepository(fallback.version.Repository) == repositoryIdentity {
					candidates = append(candidates, fallback)
				}
				comparisons = []tracker.ValidationComparison{}
				for _, candidate := range candidates {
					refs := []struct {
						key  string
						size int64
					}{{candidate.change.PayloadObjectKey, candidate.change.PayloadContentBytes}}
					if candidate.version != nil {
						refs = append(refs, struct {
							key  string
							size int64
						}{candidate.version.PayloadObjectKey, candidate.version.PayloadContentBytes})
					}
					if candidate.receipt != nil {
						refs = append(refs, struct {
							key  string
							size int64
						}{candidate.receipt.PayloadObjectKey, candidate.receipt.PayloadContentBytes})
					}
					if candidate.event != nil {
						refs = append(refs, struct {
							key  string
							size int64
						}{candidate.event.PayloadObjectKey, candidate.event.PayloadContentBytes})
					}
					fits := true
					for _, ref := range refs {
						allowed, err := reserve(ref.key, ref.size)
						if err != nil {
							return nil, err
						}
						if !allowed {
							fits = false
							break
						}
					}
					if !fits {
						unavailable("candidate_payload_budget")
						break
					}
					value, repository, found, err := s.hydrateValidationAuditCandidate(ctx, scope, candidate.change.WorkItemID, candidate, nil)
					if err != nil {
						return nil, err
					}
					if found && repository == repositoryIdentity {
						comparisons = append(comparisons, value)
					}
					if candidate.receipt != nil && candidate.event == nil && (candidate.binding == nil || !candidate.binding.LocalAttemptIDRecorded) {
						unavailable("local_attempt_reference")
					}
				}
				if len(comparisons) == 0 {
					unavailable("current_change_validation")
					comparisons = []tracker.ValidationComparison{{WorkItemID: localRow.NativeID, IntegratedSource: "unknown", Attribution: "candidate_only; source_inclusion_and_causality_unverified"}}
				}
				cache[key] = comparisons
			}
			occurrence := tracker.ValidationOccurrence{Source: sources[index], Scheduled: scheduled, Comparisons: []tracker.ValidationComparison{}}
			empty, err := json.Marshal(occurrence)
			if err != nil {
				return nil, err
			}
			metadata, err := metadataBytes()
			if err != nil {
				return nil, err
			}
			separator := 0
			if len(audit.Occurrences) > 0 {
				separator = 1
			}
			if metadata+payloadBytes+separator+len(empty) > 64*1024 {
				unavailable("audit_payload_budget")
				break sourcesLoop
			}
			occurrenceBytes := len(empty)
			appendComparison := func(value tracker.ValidationComparison) (bool, error) {
				raw, err := json.Marshal(value)
				if err != nil {
					return false, err
				}
				size := len(raw)
				if len(occurrence.Comparisons) > 0 {
					size++
				}
				if metadata+payloadBytes+separator+occurrenceBytes+size > 64*1024 {
					return false, nil
				}
				occurrence.Comparisons = append(occurrence.Comparisons, value)
				occurrenceBytes += size
				return true, nil
			}
			failed := []int{}
			for index, check := range scheduled.Checks {
				if check.ExitCode != 0 {
					failed = append(failed, index)
				}
			}
			full := false
		comparisonsLoop:
			for _, comparison := range comparisons {
				comparison.IntegratedSource = "unknown"
				if comparison.IntegratedHeadSHA != "" {
					comparison.IntegratedSource = "different_commit"
					if comparison.IntegratedHeadSHA == scheduled.HeadSHA {
						comparison.IntegratedSource = "same_commit"
					}
				}
				if len(failed) == 0 {
					value := comparison
					value.CheckComparison = gate.CompareCheck(value.Local, value.ObservedAt, scheduled, nil)
					fits, err := appendComparison(value)
					if err != nil {
						return nil, err
					}
					if !fits {
						full = true
						break comparisonsLoop
					}
				}
				for _, index := range failed {
					value := comparison
					value.CheckIndex = new(index)
					value.CheckComparison = gate.CompareCheck(value.Local, value.ObservedAt, scheduled, &scheduled.Checks[index])
					fits, err := appendComparison(value)
					if err != nil {
						return nil, err
					}
					if !fits {
						full = true
						break comparisonsLoop
					}
				}
			}
			if len(occurrence.Comparisons) > 0 || !full {
				audit.Occurrences = append(audit.Occurrences, occurrence)
				payloadBytes += separator + occurrenceBytes
			}
			if full {
				unavailable("audit_payload_budget")
				break sourcesLoop
			}
		}
	}
	if len(audit.Occurrences) == 0 && len(seen) == 0 {
		unavailable("selected_issue_scheduled_evidence")
	}
	for {
		raw, err := json.Marshal(audit)
		if err != nil {
			return nil, err
		}
		if len(raw) <= 64*1024 {
			return audit, nil
		}
		unavailable("audit_payload_budget")
		last := len(audit.Occurrences) - 1
		if len(audit.Occurrences[last].Comparisons) > 1 {
			audit.Occurrences[last].Comparisons = audit.Occurrences[last].Comparisons[:len(audit.Occurrences[last].Comparisons)-1]
		} else {
			audit.Occurrences = audit.Occurrences[:last]
		}
	}
}

func (s *Service) hydrateValidationAuditCandidate(ctx context.Context, scope hubapi.Scope, item tracker.NativeWorkItemID, rows validationAuditCandidate, attempt *tracker.NativeAttempt) (tracker.ValidationComparison, string, bool, error) {
	result := tracker.ValidationComparison{WorkItemID: item, IntegratedSource: "unknown", Attribution: "candidate_only; source_inclusion_and_causality_unverified"}
	content := eventstore.NewContent(s.deps.Details(ctx))
	if rows.change.OrganizationID != "" && (rows.change.OrganizationID != scope.Organization || rows.change.ProjectID != scope.Project || rows.change.WorkItemID != item) {
		return result, "", false, artifact.ErrIntegrity
	}
	var landing *tracker.NativeLandingReceipt
	if rows.receipt != nil {
		value, err := content.LandingReceipt(ctx, *rows.receipt)
		if err != nil {
			return result, "", false, err
		}
		if value.ChangeID == "" && rows.version != nil && rows.change.ID == rows.version.ChangeID && rows.receipt.VersionID == rows.version.ID {
			value.ChangeID = rows.change.ID
		}
		if value.ChangeID == "" || rows.version != nil && (rows.change.ID != value.ChangeID || rows.version.ID != value.VersionID) {
			return result, "", false, artifact.ErrIntegrity
		}
		landing = &value
		if rows.version == nil || rows.change.ID != value.ChangeID || rows.version.ID != value.VersionID {
			err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				if err := scope.Authorize(ctx, r, false); err != nil {
					return err
				}
				change, err := r.Change(ctx, value.ChangeID)
				if err != nil {
					return err
				}
				if change.ProjectID != scope.Project {
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
				return result, "", false, s.deps.RecordError(err)
			}
		}
		if rows.receipt.OrganizationID != scope.Organization || rows.receipt.VersionID != value.VersionID || rows.binding != nil && (rows.binding.OrganizationID != scope.Organization || rows.binding.ID != rows.receipt.AttemptID) {
			return result, "", false, artifact.ErrIntegrity
		}
	}
	if rows.version == nil {
		return result, "", false, nil
	}
	if rows.change.OrganizationID != scope.Organization || rows.change.ProjectID != scope.Project || rows.change.WorkItemID != item || rows.version.OrganizationID != scope.Organization || rows.version.ChangeID != rows.change.ID {
		return result, "", false, artifact.ErrIntegrity
	}
	change, err := content.Change(ctx, rows.change)
	if err != nil {
		return result, "", false, err
	}
	change, visible, err := s.deps.FilterChange(ctx, scope, change)
	if err != nil {
		return result, "", false, err
	}
	if !visible || !slices.Contains(change.LinkedIssues, item) {
		return result, "", false, hubapi.NotFound()
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
			if rows.binding.LocalAttemptIDRecorded {
				if rows.binding.LocalAttemptID < 0 {
					return result, "", false, artifact.ErrIntegrity
				}
				result.LocalAttemptID = rows.binding.LocalAttemptID
			}
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
			if run.Runtime != nil && run.Runtime.LocalAttemptID > 0 {
				if result.LocalAttemptID > 0 && result.LocalAttemptID != run.Runtime.LocalAttemptID {
					return result, "", false, artifact.ErrIntegrity
				}
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
	repository := validationAuditRepository(version.Repository)
	if rows.receipt != nil && rows.receipt.Repository != "" {
		repository = validationAuditRepository(rows.receipt.Repository)
	}
	return result, repository, true, nil
}

func (s *Service) hydrateValidationAuditComment(ctx context.Context, input validationAuditComment) (tracker.NativeComment, error) {
	row, version := input.row, input.version
	var comment tracker.NativeComment
	if version.OrganizationID != row.OrganizationID || version.ProjectID != row.ProjectID || version.WorkItemID != row.WorkItemID || version.RecordID != row.ID || version.Revision != row.Revision {
		return comment, artifact.ErrIntegrity
	}
	raw, err := eventstore.NewContent(s.deps.Details(ctx)).CollaborationVersion(ctx, version)
	if err != nil {
		return comment, err
	}
	comment, err = eventstore.DecodeCollaborationComment(version, raw)
	if err != nil || comment.OrganizationID != row.OrganizationID || comment.ProjectID != row.ProjectID || comment.WorkItemID != row.WorkItemID || comment.ID != row.ID || int64(comment.Revision) != row.Revision || comment.Sequence != row.Sequence || comment.Deleted != row.Deleted || comment.Actor.Kind != row.ActorKind || comment.Actor.PrincipalID != row.ActorPrincipalID {
		return comment, artifact.ErrIntegrity
	}
	return comment, nil
}

func validationAuditRepository(repository string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(repository), "https://github.com/"), "git@github.com:"), ".git")
}
