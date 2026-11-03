package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boundedChangeRecordCursor struct {
	changeReadCursor
	Organization     tracker.OrganizationID   `json:"organization"`
	Project          tracker.ProjectID        `json:"project"`
	Item             tracker.NativeWorkItemID `json:"item"`
	EvidenceSequence int64                    `json:"evidence_sequence"`
	Revision         tracker.Revision         `json:"revision"`
}

func encodeBoundedChangeRecordCursor(cursor boundedChangeRecordCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw), err
}

func (s *Service) readBoundedChangeRecord(ctx context.Context, scope nativeScope, args operatortool.ChangeArguments, result operatortool.ChangeResult) (operatortool.ChangeResult, error) {
	change, err := s.readChangeRecord(ctx, scope, args.ItemID, args.ChangeID)
	if err != nil {
		return result, err
	}
	var row eventstore.Change
	var current, selected eventstore.Version
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.Change(ctx, change.ID)
		if err != nil {
			return err
		}
		if row.ProjectID != scope.project || row.Revision != int64(change.Revision) || valueOrEmpty(row.CurrentVersionID) != change.CurrentVersion {
			return corestore.ErrRevisionConflict
		}
		if change.CurrentVersion != "" {
			current, err = r.Version(ctx, change.ID, change.CurrentVersion)
			if err != nil {
				return err
			}
		}
		selected = current
		if args.VersionID != "" && args.VersionID != change.CurrentVersion {
			selected, err = r.Version(ctx, change.ID, args.VersionID)
		}
		return err
	})
	if err != nil {
		return result, projectRecordError(err)
	}
	selection := boundedChangeRecordCursor{changeReadCursor: changeReadCursor{Change: change.ID, Head: change.CurrentVersion, Version: selected.ID, Section: args.Section}, Organization: scope.organization, Project: scope.project, Item: tracker.NativeWorkItemID(args.ItemID), Revision: change.Revision, EvidenceSequence: row.EvidenceSequence}
	if args.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(args.Cursor)
		if err != nil || operatortool.DecodeArguments(raw, &selection) != nil || selection.Offset < 0 || selection.Offset > 0 && (selection.Section != "reviews" || len(selection.Digest) != 64) || selection.After < 0 || selection.Through < selection.After || selection.Organization != scope.organization || selection.Project != scope.project || selection.Item != tracker.NativeWorkItemID(args.ItemID) || selection.Change != change.ID || selection.Version == "" && change.CurrentVersion != "" || !validChangeReadSection(selection.Section) || selection.Section == "current" || args.Section != "" && args.Section != selection.Section || args.VersionID != "" && args.VersionID != selection.Version {
			return result, operatortool.ErrInvalidArguments
		}
		if selection.Head != change.CurrentVersion || selection.Revision != change.Revision || selection.EvidenceSequence < 0 || selection.EvidenceSequence > row.EvidenceSequence {
			return result, mutation.ErrConflict
		}
		if selection.Version != selected.ID {
			err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				selected, err = r.Version(ctx, change.ID, selection.Version)
				return err
			})
			if err != nil {
				return result, projectRecordError(err)
			}
		}
	}
	if selection.Section == "" {
		selection.Section = "current"
	}
	if !validChangeReadSection(selection.Section) {
		return result, operatortool.ErrInvalidArguments
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	detail := tracker.ChangeDetail{Change: change}
	seenSources := map[string]bool{}
	for _, item := range change.LinkedIssues {
		row, snapshot, err := s.readWorkItemRecord(ctx, scope, string(item))
		if err != nil {
			return result, err
		}
		issue, err := s.hydrateWorkItemRecord(ctx, row, snapshot)
		if err != nil {
			return result, err
		}
		for _, reference := range issue.ExternalReferences {
			if reference.Provider == "github" && reference.URL != "" && !seenSources[reference.URL] {
				detail.SourceIssues = append(detail.SourceIssues, reference)
				seenSources[reference.URL] = true
			}
		}
	}

	if selected.ID != "" && selection.Section != "versions" {
		version, err := content.Version(ctx, selected)
		if err != nil {
			return result, err
		}
		detail.Versions = []tracker.ChangeVersion{boundedChangeVersion(version)}
	}
	summary, err := s.readBoundedChangeSummary(ctx, scope, change, current, selection.EvidenceSequence)
	if err != nil {
		return result, err
	}
	detail.Summary = summary
	if selected.ID != "" && selection.Section != "versions" {
		pushes, err := s.readBoundedChangePushes(ctx, scope, args.ItemID, detail.Versions)
		if err != nil {
			return result, err
		}
		detail.PublicationPushes = pushes
	}
	result.Detail = &detail
	result.Read = &operatortool.ChangeRead{VersionID: selection.Version, PolicyDetailsOmitted: true, Complete: true, Sections: map[string]operatortool.ChangeReadPage{}}
	sections := []string{selection.Section}
	if selection.Section == "current" {
		sections = []string{"versions", "reviews", "checks", "discussion"}
	}
	cursors := map[string]boundedChangeRecordCursor{}
	for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
		cursor := selection
		cursor.Section = section
		if args.Cursor == "" || section != selection.Section {
			cursor.After, cursor.Offset, cursor.Digest = 0, 0, ""
			cursor.Through = selection.EvidenceSequence
			if section == "versions" {
				cursor.Through = current.Number
			}
		}
		maximum := selection.EvidenceSequence
		if section == "versions" {
			maximum = current.Number
		}
		if cursor.Through != maximum {
			return result, operatortool.ErrInvalidArguments
		}
		cursors[section] = cursor
		next, err := encodeBoundedChangeRecordCursor(cursor)
		if err != nil {
			return result, err
		}
		result.Read.Sections[section] = operatortool.ChangeReadPage{NextCursor: next}
	}
	base, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if len(base)+512 >= operatortool.MaxResultBytes {
		return result, operatortool.ChangeRecordTooLarge()
	}
	budget := (operatortool.MaxResultBytes - len(base) - 512) / len(sections)
	for _, section := range sections {
		page, err := s.readChangeRecordSection(ctx, scope, args, &result, cursors[section], budget, len(sections) == 1, selection.Section == "current" && section == "versions")
		if err != nil {
			return result, err
		}
		result.Read.Sections[section] = page
	}
	for _, page := range result.Read.Sections {
		result.Read.Complete = result.Read.Complete && page.Complete
	}
	latest, err := s.readChangeRecord(ctx, scope, args.ItemID, args.ChangeID)
	if err != nil {
		return result, err
	}
	if latest.Revision != change.Revision || latest.CurrentVersion != change.CurrentVersion {
		return result, mutation.ErrConflict
	}
	return result, nil
}

func (s *Service) readChangeRecordSection(ctx context.Context, scope nativeScope, args operatortool.ChangeArguments, result *operatortool.ChangeResult, cursor boundedChangeRecordCursor, budget int, single, omitHistory bool) (operatortool.ChangeReadPage, error) {
	var versions []eventstore.Version
	var evidence []eventstore.Evidence
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		if cursor.Section == "versions" {
			versions, err = r.Versions(ctx, cursor.Change, cursor.After, changeLimit(args)+1)
		} else {
			evidence, err = r.Evidence(ctx, cursor.Change, cursor.After, changeLimit(args)+1)
		}
		return err
	})
	if err != nil {
		return operatortool.ChangeReadPage{}, projectRecordError(err)
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	count := len(evidence)
	if cursor.Section == "versions" {
		count = len(versions)
	}
	used := 0
	pastBoundary := false
	for i := 0; i < count && i < changeLimit(args); i++ {
		var sequence int64
		var raw []byte
		before := *result.Detail
		previous, err := json.Marshal(result)
		if err != nil {
			return operatortool.ChangeReadPage{}, err
		}
		if cursor.Section == "versions" {
			row := versions[i]
			sequence = row.Number
			if sequence > cursor.Through {
				pastBoundary = true
				break
			}
			if omitHistory {
				break
			}
			if row.OrganizationID != scope.organization || row.ChangeID != cursor.Change {
				return operatortool.ChangeReadPage{}, eventstore.ErrInvalidRecord
			}
			if row.ID == cursor.Version {
				cursor.After = sequence
				continue
			}
			version, err := content.Version(ctx, row)
			if err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			pushes, err := s.readBoundedChangePushes(ctx, scope, args.ItemID, []tracker.ChangeVersion{version})
			if err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			result.Detail.PublicationPushes = append(result.Detail.PublicationPushes, pushes...)
			result.Detail.Versions = append(result.Detail.Versions, boundedChangeVersion(version))
		} else {
			row := evidence[i]
			sequence = row.Sequence
			if sequence > cursor.Through {
				pastBoundary = true
				break
			}
			if row.OrganizationID != scope.organization || row.ChangeID != cursor.Change {
				return operatortool.ChangeReadPage{}, eventstore.ErrInvalidRecord
			}
			matches := row.Kind == strings.TrimSuffix(cursor.Section, "s") && (valueOrEmpty(row.VersionID) == cursor.Version || cursor.Section == "discussion" && valueOrEmpty(row.VersionID) == "")
			if !matches {
				if cursor.Offset > 0 {
					return operatortool.ChangeReadPage{}, mutation.ErrConflict
				}
				cursor.After = sequence
				continue
			}
			raw, err = content.Evidence(ctx, row)
			if err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			if err := appendBoundedChangeEvidence(result.Detail, row, raw); err != nil {
				return operatortool.ChangeReadPage{}, err
			}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return operatortool.ChangeReadPage{}, err
		}
		size := len(encoded) - len(previous)
		if used+size > budget || cursor.Offset > 0 {
			*result.Detail = before
			if single && used == 0 && cursor.Section == "reviews" {
				fragment := &tracker.NativeRecoveryPage{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(args.ItemID)}, Section: cursor.Section, RecordID: strconv.FormatInt(sequence, 10), VersionID: cursor.Version}
				if err := recoveryRecordFragment(fragment, raw, cursor.Offset, cursor.Digest); err != nil {
					return operatortool.ChangeReadPage{}, err
				}
				result.Fragment = fragment
				encoded, err := json.Marshal(result)
				if err != nil {
					return operatortool.ChangeReadPage{}, err
				}
				if excess := len(encoded) + 512 - operatortool.MaxResultBytes; excess > 0 {
					keep := len(fragment.Data) - (excess+3)*3/4
					if keep <= 0 {
						return operatortool.ChangeReadPage{}, operatortool.ChangeRecordTooLarge()
					}
					fragment.Data = fragment.Data[:keep]
				}
				cursor.Offset, cursor.Digest = fragment.Offset+int64(len(fragment.Data)), fragment.SHA256
				if cursor.Offset == fragment.TotalBytes {
					cursor.After, cursor.Offset, cursor.Digest = sequence, 0, ""
				}
			} else if single && used == 0 {
				return operatortool.ChangeReadPage{}, operatortool.ChangeRecordTooLarge()
			}
			break
		}
		used += size
		cursor.After = sequence
	}
	if cursor.Offset > 0 && count == 0 {
		return operatortool.ChangeReadPage{}, mutation.ErrConflict
	}
	complete := cursor.After >= cursor.Through || count == 0 || pastBoundary
	if !complete && !omitHistory && count <= changeLimit(args) {
		last := int64(0)
		if cursor.Section == "versions" {
			last = versions[count-1].Number
		} else {
			last = evidence[count-1].Sequence
		}
		complete = cursor.After >= last
	}
	if complete {
		return operatortool.ChangeReadPage{Complete: true}, nil
	}
	next, err := encodeBoundedChangeRecordCursor(cursor)
	return operatortool.ChangeReadPage{NextCursor: next}, err
}

func appendBoundedChangeEvidence(detail *tracker.ChangeDetail, row eventstore.Evidence, raw []byte) error {
	switch row.Kind {
	case "review":
		var value tracker.ChangeReview
		if json.Unmarshal(raw, &value) != nil || value.ID == "" || value.VersionID != valueOrEmpty(row.VersionID) || value.Decision != row.Decision || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(value.CreatedAt, row.CreatedAt) {
			return eventstore.ErrInvalidRecord
		}
		if row.ReviewKind != "" {
			kind, landing := "human", false
			switch {
			case value.SecurityAudit != nil:
				kind, landing = "security_audit", value.SecurityAudit.Landing
			case value.ImplementationReview != nil:
				kind = "implementation"
			case value.Validator != nil:
				kind = "validator"
			}
			if row.ReviewKind != kind || row.ReviewLanding != landing {
				return eventstore.ErrInvalidRecord
			}
		}
		value.Validator = boundedChangeReviewResult(value.Validator)
		value.ImplementationReview = boundedChangeReviewResult(value.ImplementationReview)
		detail.Reviews = append(detail.Reviews, value)
	case "check":
		var value tracker.ChangeCheck
		if json.Unmarshal(raw, &value) != nil || value.VersionID != valueOrEmpty(row.VersionID) || value.CheckRunID != valueOrEmpty(row.SourceKey) || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(value.ReceivedAt, row.CreatedAt) {
			return eventstore.ErrInvalidRecord
		}
		detail.Checks = append(detail.Checks, value)
	case "discussion":
		var value tracker.ChangeDiscussion
		if json.Unmarshal(raw, &value) != nil || value.ID == "" || value.VersionID != valueOrEmpty(row.VersionID) || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(value.CreatedAt, row.CreatedAt) {
			return eventstore.ErrInvalidRecord
		}
		detail.Discussion = append(detail.Discussion, value)
	default:
		return eventstore.ErrInvalidRecord
	}
	return nil
}

func (s *Service) readBoundedChangeSummary(ctx context.Context, scope nativeScope, change tracker.ChangeRequest, current eventstore.Version, through int64) (tracker.ChangeSummary, error) {
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	detail := tracker.ChangeDetail{Change: change}
	if current.ID == "" {
		return changerequest.SummarizeCurrentVersion(detail, "", "", s.config.now(), false), nil
	}
	var version tracker.ChangeVersion
	var evidence []eventstore.Evidence
	var prior bool
	var rows currentChangePolicyRows
	var checks []string
	if current.ID != "" {
		var err error
		version, err = content.Version(ctx, current)
		if err != nil {
			return tracker.ChangeSummary{}, err
		}
		detail.Versions = []tracker.ChangeVersion{version}
		for _, check := range version.Checks {
			checks = append(checks, check.CheckRunID)
		}
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = readChangeSummaryPolicyRows(ctx, r, scope)
		if err != nil {
			return err
		}
		reader, ok := r.(eventstore.ChangeRecoveryReader)
		if !ok {
			return corestore.ErrInvalidPage
		}
		evidence, prior, err = reader.ChangeSummaryEvidence(ctx, change.ID, current.ID, through, checks)
		return err
	})
	if err != nil {
		return tracker.ChangeSummary{}, projectRecordError(err)
	}
	approved, err := hydrateChangeSummaryPolicy(ctx, content, scope, rows)
	if err != nil {
		return tracker.ChangeSummary{}, err
	}

	for _, row := range evidence {
		if row.ChangeID != change.ID || row.OrganizationID != scope.organization || valueOrEmpty(row.VersionID) != current.ID || row.Sequence > through {
			return tracker.ChangeSummary{}, eventstore.ErrInvalidRecord
		}
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return tracker.ChangeSummary{}, err
		}
		if err := appendBoundedChangeEvidence(&detail, row, raw); err != nil {
			return tracker.ChangeSummary{}, err
		}
	}
	policyID, reviewID := changerequest.CurrentPolicyIdentities(&version, approved.Policy, approved.ReviewPolicy)
	return s.summarizeChangeWithAuditDispositions(ctx, scope, detail, policyID, reviewID, s.config.now(), prior)
}

func (s *Service) readBoundedChangePushes(ctx context.Context, scope nativeScope, item string, versions []tracker.ChangeVersion) ([]tracker.ChangePublicationPush, error) {
	var result []tracker.ChangePublicationPush
	var events []eventstore.Event
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, version := range versions {
		if version.External != nil || version.AttemptID == "" || !version.Policy.Gates.GitHubPullRequest || version.Landing != nil && version.Landing.Landed {
			continue
		}
		reference := tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}
		{
			err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				reader, ok := r.(eventstore.ChangeRecoveryReader)
				if !ok {
					return corestore.ErrInvalidPage
				}
				var err error
				events, err = reader.ChangePublicationCheckpoints(ctx, scope.project, tracker.NativeWorkItemID(item), reference)
				return err
			})
			if err != nil {
				return nil, projectRecordError(err)
			}
		}
		for _, row := range events {
			event, err := content.Event(ctx, row)
			if err != nil {
				return nil, err
			}
			if row.OrganizationID != scope.organization || row.ProjectID != scope.project || row.WorkItemID != tracker.NativeWorkItemID(item) || row.Type != "run.checkpointed" {
				return nil, eventstore.ErrInvalidRecord
			}
			run := event.Data.Run
			if run == nil || run.Handoff == nil {
				continue
			}
			checkpoint := run.Handoff
			if checkpoint.ExternalEffect != "git_push" || checkpoint.EffectState != "confirmed" || checkpoint.Change == nil || *checkpoint.Change != reference {
				continue
			}
			if checkpoint.HeadSHA != version.HeadSHA || !validNativeID(checkpoint.EffectID, "effect") || run.AttemptID != row.AttemptID || run.RunID != row.RunID {
				break
			}
			valid := false
			err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				reader, ok := r.(eventstore.ChangeRecoveryReader)
				if !ok {
					return corestore.ErrInvalidPage
				}
				var err error
				valid, err = reader.ChangePublicationProvenance(ctx, scope.project, tracker.NativeWorkItemID(item), *run)
				return err
			})
			if err != nil {
				return nil, projectRecordError(err)
			}
			if valid {
				result = append(result, tracker.ChangePublicationPush{Change: reference, RecordingAttemptID: run.AttemptID, EffectID: checkpoint.EffectID})
			}
			break
		}
	}
	return result, nil
}
