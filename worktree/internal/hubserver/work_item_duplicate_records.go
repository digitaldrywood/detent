package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type workItemDuplicateConfirmation struct {
	challenge corestore.DuplicateChallenge
	selected  []corestore.DuplicateCandidate
	issues    map[tracker.NativeWorkItemID]tracker.NativeIssue
}

type workItemDuplicateLink struct {
	candidate corestore.DuplicateCandidate
	issue     tracker.NativeIssue
	version   eventstore.CollaborationVersion
	event     eventstore.Event
	bytes     int64
}

func duplicateContentHash(request tracker.CreateIssue) (string, error) {
	request.ChallengeToken = ""
	request.NotDuplicateOf = nil
	return hubapi.CommandHash(request)
}

func decodeWorkItemReceipt(raw json.RawMessage, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var uniqueFields func() error
	uniqueFields = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			fields := map[string]bool{}
			for decoder.More() {
				field, err := decoder.Token()
				name, ok := field.(string)
				name = strings.ToUpper(strings.ToLower(name))
				if err != nil || !ok || fields[name] {
					return receiptstore.ErrExpired
				}
				fields[name] = true
				if err := uniqueFields(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return receiptstore.ErrExpired
			}
		case json.Delim('['):
			for decoder.More() {
				if err := uniqueFields(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return receiptstore.ErrExpired
			}
		}
		return nil
	}
	if err := uniqueFields(); err != nil {
		return receiptstore.ErrExpired
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return receiptstore.ErrExpired
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return receiptstore.ErrExpired
	}
	return nil
}

func validateWorkItemReceiptIssue(scope hubscope.NativeScope, item string, raw json.RawMessage) error {
	var issue tracker.NativeIssue
	if err := decodeWorkItemReceipt(raw, &issue); err != nil {
		return err
	}
	if issue.OrganizationID != scope.Organization || issue.ProjectID != scope.Project || item == "" && !hubapi.ValidID(string(issue.WorkItemID), "wi") || item != "" && string(issue.WorkItemID) != item ||
		issue.Number <= 0 || issue.Revision <= 0 || issue.Profile != "native" && issue.Profile != "github_compatible" || issue.Title == "" || issue.State == "" {
		return receiptstore.ErrExpired
	}
	if !scope.ReporterOwns(issue.Actor) {
		return hubapi.NativeNotFound()
	}
	if len(issue.Dependencies) != 0 || len(issue.Blockers) != 0 || len(issue.Dependents) != 0 || len(issue.Related) != 0 ||
		issue.DependencyReady || issue.BlockedReason != (tracker.DependencyReason{}) || issue.UnblocksReason != (tracker.DependencyReason{}) {
		return receiptstore.ErrExpired
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return receiptstore.ErrExpired
	}
	for name, value := range fields {
		for _, field := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason"} {
			if strings.EqualFold(name, field) && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return receiptstore.ErrExpired
			}
		}
	}
	return nil
}

func (s *Service) readWorkItemRecordReceipt(ctx context.Context, scope hubscope.NativeScope, receipt eventstore.Receipt, input any, item string) (json.RawMessage, error) {
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	if err != nil {
		return raw, err
	}
	var challenge duplicateChallenge
	if err := json.Unmarshal(raw, &challenge); err != nil {
		switch input.(type) {
		case tracker.CreateComment, tracker.UpdateComment:
			return raw, err
		}
		return nil, receiptstore.ErrExpired
	}
	switch input.(type) {
	case tracker.CreateComment, tracker.UpdateComment:
		return raw, nil
	}
	if challenge.ChallengeToken == "" {
		if err := validateWorkItemReceiptIssue(scope, item, raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	if _, create := input.(tracker.CreateIssue); !create {
		return nil, receiptstore.ErrExpired
	}
	if err := decodeWorkItemReceipt(raw, &challenge); err != nil {
		return nil, err
	}
	var stored corestore.DuplicateChallenge
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		challenges, ok := reader.(corestore.DuplicateChallengeReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		stored, err = challenges.DuplicateChallenge(ctx, scope.Project, challenge.ChallengeToken)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) || err == nil && (stored.PrincipalID != scope.Actor().PrincipalID || stored.RequestID != receipt.CommandKey || stored.ContentHash != receipt.RequestHash || !s.config.Clock().Before(stored.ExpiresAt)) {
		return nil, receiptstore.ErrExpired
	}
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if len(challenge.PossibleDuplicates) == 0 || len(challenge.PossibleDuplicates) > 5 || len(challenge.PossibleDuplicates) != len(stored.Candidates) {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	for i, candidate := range challenge.PossibleDuplicates {
		if candidate.WorkItemID != stored.Candidates[i].WorkItemID || int64(candidate.Number) != stored.Candidates[i].Number {
			return nil, hubapi.NativeWorkItemStorageUnavailable()
		}
	}
	if err := s.validateWorkItemChallengeReplay(ctx, scope, stored); err != nil {
		return nil, err
	}
	if scope.Credential.HostedRole != "reporter" {
		return raw, nil
	}
	visible := make([]hubscope.PossibleDuplicate, 0, len(challenge.PossibleDuplicates))
	for _, candidate := range challenge.PossibleDuplicates {
		_, _, err := s.readWorkItemRecord(ctx, scope, string(candidate.WorkItemID))
		if hubapi.IsNativeNotFound(err) || errors.Is(err, corestore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		visible = append(visible, candidate)
	}
	if len(visible) == 0 {
		return nil, hubapi.NativePublicInvalid("No challenged issues are still visible; use a new request ID")
	}
	challenge.PossibleDuplicates = visible
	return json.Marshal(challenge)
}

func (s *Service) validateWorkItemChallengeReplay(ctx context.Context, scope hubscope.NativeScope, challenge corestore.DuplicateChallenge) error {
	var project projectstore.Record
	var revision policystore.Revision
	var found bool
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		project, err = r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		revision, err = r.ProjectPolicy(ctx, scope.Project)
		found = err == nil
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return hubapi.ProjectRecordError(err)
	}
	policyHash, err := workItemDuplicatePolicyHash(revision, found)
	if err != nil {
		return err
	}
	if policyHash != challenge.PolicyHash {
		return hubapi.PolicyMismatch("The approved policy changed after the duplicate challenge")
	}
	if found {
		descriptor, err := eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, revision)
		if err != nil {
			return err
		}
		if err := validateConfiguredWorkflowPolicy(project, descriptor, true); err != nil {
			return err
		}
	} else if project.Core.WorkflowSource != "" {
		return hubapi.PolicyMismatch("The configured workflow requires its approved policy")
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if err := lockWorkItemDuplicateSource(ctx, w, scope.Project, challenge.SourceSequence); err != nil {
			if errors.Is(err, searchstore.ErrIndexPending) || errors.Is(err, corestore.ErrRevisionConflict) {
				return hubapi.NativeConflict(0)
			}
			return err
		}
		if !s.config.Clock().Before(challenge.ExpiresAt) {
			return receiptstore.ErrExpired
		}
		challenges, ok := w.(corestore.DuplicateChallengeReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		stored, err := challenges.DuplicateChallenge(ctx, scope.Project, challenge.Token)
		if errors.Is(err, corestore.ErrNotFound) {
			return receiptstore.ErrExpired
		}
		if err != nil {
			return err
		}
		if stored.PrincipalID != challenge.PrincipalID || stored.RequestID != challenge.RequestID || stored.ContentHash != challenge.ContentHash || stored.PolicyHash != challenge.PolicyHash || stored.SourceSequence != challenge.SourceSequence || !slices.Equal(stored.Candidates, challenge.Candidates) {
			return hubapi.NativeConflict(0)
		}
		current, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if current.Core.DeletedAt != nil || !hubrecords.SameWorkItemProjectPlan(project, current) {
			return hubapi.NativeConflict(0)
		}
		currentPolicy, err := w.ProjectPolicy(ctx, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) && !found {
			err = nil
		} else if err == nil && (!found || !hubrecords.SameNativeClaimProjectPolicy(revision, currentPolicy)) {
			return hubapi.NativeConflict(0)
		}
		if err != nil {
			return err
		}
		for _, candidate := range challenge.Candidates {
			item, err := w.LockWorkItem(ctx, scope.Project, candidate.WorkItemID)
			if err != nil {
				return err
			}
			if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
				return hubapi.NativeNotFound()
			}
			if item.Archived || item.Number != candidate.Number || item.Revision != candidate.Revision {
				return hubapi.NativeConflict(tracker.Revision(item.Revision))
			}
		}
		return nil
	})
	return hubapi.ProjectRecordError(err)
}

func (s *Service) prepareWorkItemDuplicateLinks(ctx context.Context, scope hubscope.NativeScope, project projectstore.Record, issue tracker.NativeIssue, confirmation *workItemDuplicateConfirmation) ([]workItemDuplicateLink, error) {
	if confirmation == nil {
		return nil, nil
	}
	content := eventstore.NewContent(s.runDetailStore())
	links := make([]workItemDuplicateLink, 0, len(confirmation.selected))
	for _, candidate := range confirmation.selected {
		issue.Revision++
		related := confirmation.issues[candidate.WorkItemID]
		issue.Related = append(slices.Clone(issue.Related), tracker.NativeDependency{ID: related.WorkItemID, ProjectID: related.ProjectID, Number: related.Number, Title: related.Title, State: related.State, Terminal: related.Terminal, Dispatchable: hubrecords.DispatchableProjectState(project, related.State), Identifier: string(related.ProjectID) + "#" + strconv.Itoa(related.Number), WebURL: related.WebURL})
		raw, err := json.Marshal(issue)
		if err != nil {
			return nil, err
		}
		version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
		if err != nil {
			return nil, err
		}
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: issue.WorkItemID, ID: hubapi.NewID("evt"), Type: "issue.edited", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: issue.UpdatedAt, Data: tracker.CollaborationData{Revision: issue.Revision, Fields: []string{"related"}, RelatedWorkItemID: candidate.WorkItemID, Operation: "add"}})
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return nil, err
		}
		eventBytes, err := collaborationEventChargeBytes(eventRaw)
		if err != nil {
			return nil, err
		}
		links = append(links, workItemDuplicateLink{candidate: candidate, issue: issue, version: version, event: event, bytes: int64(len(raw)) + eventBytes})
	}
	return links, nil
}

func (s *Service) prepareWorkItemDuplicateConfirmation(ctx context.Context, scope hubscope.NativeScope, request tracker.CreateIssue) (*workItemDuplicateConfirmation, error) {
	if len(request.ChallengeToken) > 128 || len(request.NotDuplicateOf) < 1 || len(request.NotDuplicateOf) > 5 || request.GitHubIssueURL != "" || request.Provenance != nil {
		return nil, hubapi.NativePublicInvalid("Supply a duplicate challenge token and one to five issue numbers together")
	}
	hash, err := duplicateContentHash(request)
	if err != nil {
		return nil, err
	}
	confirmation := &workItemDuplicateConfirmation{issues: make(map[tracker.NativeWorkItemID]tracker.NativeIssue, len(request.NotDuplicateOf))}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		challenges, ok := reader.(corestore.DuplicateChallengeReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		stored, err := challenges.DuplicateChallenge(ctx, scope.Project, request.ChallengeToken)
		if errors.Is(err, corestore.ErrNotFound) {
			return receiptstore.ErrExpired
		}
		if err != nil {
			return err
		}
		if stored.PrincipalID != scope.Actor().PrincipalID || stored.RequestID != request.IdempotencyKey || stored.ContentHash != hash || !s.config.Clock().Before(stored.ExpiresAt) {
			return receiptstore.ErrExpired
		}
		confirmation.challenge = stored
		project, err := reader.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil {
			return hubapi.NativeNotFound()
		}
		organization, err := reader.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		for _, number := range request.NotDuplicateOf {
			if slices.ContainsFunc(confirmation.selected, func(candidate corestore.DuplicateCandidate) bool { return candidate.Number == int64(number) }) {
				return hubapi.NativePublicInvalid("Not duplicate of must contain unique issue numbers")
			}
			index := slices.IndexFunc(stored.Candidates, func(candidate corestore.DuplicateCandidate) bool { return candidate.Number == int64(number) })
			if index < 0 {
				return hubapi.NativePublicInvalid("Not duplicate of must name a challenged issue number")
			}
			candidate := stored.Candidates[index]
			item, err := reader.WorkItem(ctx, scope.Project, candidate.WorkItemID)
			if err != nil || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				return hubapi.NativeNotFound()
			}
			if item.Archived || item.Number != candidate.Number || item.Revision != candidate.Revision {
				return hubapi.NativeConflict(tracker.Revision(item.Revision))
			}
			confirmation.selected = append(confirmation.selected, candidate)
			state := hubapi.StringValue(item.WorkflowStateName)
			confirmation.issues[candidate.WorkItemID] = s.nativeIssueResponse(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.NativeID, Number: int(item.Number), Revision: tracker.Revision(item.Revision)}, Title: item.Title, State: state, Terminal: hubrecords.TenantProjectStateTerminal(project, state), WebURL: hubrecords.IssueBrowserPath(organization.Slug, project.Core.Slug, int(item.Number))})
		}
		return nil
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	return confirmation, nil
}

func consumeWorkItemDuplicateConfirmation(ctx context.Context, writer tenantstore.Writer, scope hubscope.NativeScope, confirmation *workItemDuplicateConfirmation, now time.Time) error {
	challenges, ok := writer.(corestore.DuplicateChallengeWriter)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	stored, err := challenges.ConsumeDuplicateChallenge(ctx, confirmation.challenge, now)
	if errors.Is(err, corestore.ErrNotFound) {
		return receiptstore.ErrExpired
	}
	if err != nil {
		return err
	}
	if !slices.Equal(stored.Candidates, confirmation.challenge.Candidates) {
		return corestore.ErrSerializationFailure
	}
	ordered := slices.Clone(confirmation.selected)
	slices.SortFunc(ordered, func(a, b corestore.DuplicateCandidate) int {
		return strings.Compare(string(a.WorkItemID), string(b.WorkItemID))
	})
	for _, candidate := range ordered {
		item, err := writer.LockWorkItem(ctx, scope.Project, candidate.WorkItemID)
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		if item.Archived || item.Number != candidate.Number || item.Revision != candidate.Revision {
			return hubapi.NativeConflict(tracker.Revision(item.Revision))
		}
	}
	return nil
}

func workItemDuplicatePolicyHash(revision policystore.Revision, found bool) (string, error) {
	return hubapi.CommandHash(struct {
		Revision policystore.Revision
		Found    bool
	}{revision, found})
}

func lockWorkItemDuplicateSource(ctx context.Context, w tenantstore.Writer, project tracker.ProjectID, sequence int64) error {
	writer, ok := w.(searchstore.DuplicateSourceWriter)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return writer.LockDuplicateSource(ctx, project, sequence)
}

type workItemMachinePriority struct {
	plan    hubrecords.WorkItemRecordPlan
	fields  hubrecords.WorkItemRecordFields
	version eventstore.CollaborationVersion
	event   eventstore.Event
}

func (s *Service) prepareWorkItemMachinePriority(ctx context.Context, scope hubscope.NativeScope, occurrence hubrecords.WorkItemRecordPlan, priority *int) (*workItemMachinePriority, error) {
	if err := hubrecords.RequireNativeEdit(occurrence.Issue, occurrence.Issue.Revision); err != nil {
		return nil, err
	}
	plan := hubrecords.WorkItemRecordPlan{Project: occurrence.Project, Current: occurrence.Current, Issue: occurrence.Issue, Fields: []string{"priority"}, EventType: "issue.edited"}
	plan.Issue.Priority = priority
	plan.Issue.Revision++
	plan.Issue.UpdatedAt = s.config.Clock().UTC().Truncate(time.Microsecond)
	plan.Issue.LastActivityAt = plan.Issue.UpdatedAt
	fields, err := s.prepareWorkItemFields(ctx, scope.Organization, plan)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(plan.Issue)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Issue.WorkItemID, RecordID: string(plan.Issue.WorkItemID), Revision: int64(plan.Issue.Revision)}, raw)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: plan.Issue.WorkItemID, ID: hubapi.NewID("evt"), Type: plan.EventType, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: plan.Issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.Issue.Revision, Fields: plan.Fields}})
	if err != nil {
		return nil, err
	}
	event, err := content.PrepareEvent(ctx, scope.Organization, raw)
	if err != nil {
		return nil, err
	}
	return &workItemMachinePriority{plan: plan, fields: fields, version: version, event: event}, nil
}

func (p *workItemMachinePriority) write(ctx context.Context, w tenantstore.Writer) error {
	if err := hubrecords.WriteWorkItemRecord(ctx, w, p.plan, p.fields); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, p.version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, p.event); err != nil {
		return err
	}
	return nil
}
