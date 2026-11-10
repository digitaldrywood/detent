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
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
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
}

func duplicateContentHash(request tracker.CreateIssue) (string, error) {
	request.ChallengeToken = ""
	request.NotDuplicateOf = nil
	return nativeCommandHash(request)
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

func validateWorkItemReceiptIssue(scope nativeScope, item string, raw json.RawMessage) error {
	var issue tracker.NativeIssue
	if err := decodeWorkItemReceipt(raw, &issue); err != nil {
		return err
	}
	if issue.OrganizationID != scope.organization || issue.ProjectID != scope.project || !validNativeID(string(issue.WorkItemID), "wi") || item != "" && string(issue.WorkItemID) != item ||
		issue.Number <= 0 || issue.Revision <= 0 || issue.Profile != "native" || issue.Title == "" || issue.State == "" {
		return receiptstore.ErrExpired
	}
	if !scope.reporterOwns(issue.Actor) {
		return nativeNotFound()
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

func (s *Service) readWorkItemRecordReceipt(ctx context.Context, scope nativeScope, receipt eventstore.Receipt, input any, item string) (json.RawMessage, error) {
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
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		challenges, ok := reader.(corestore.DuplicateChallengeReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		stored, err = challenges.DuplicateChallenge(ctx, scope.project, challenge.ChallengeToken)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) || err == nil && (stored.PrincipalID != scope.actor().PrincipalID || stored.RequestID != receipt.CommandKey || stored.ContentHash != receipt.RequestHash || !s.config.now().Before(stored.ExpiresAt)) {
		return nil, receiptstore.ErrExpired
	}
	if err != nil {
		return nil, projectRecordError(err)
	}
	if len(challenge.PossibleDuplicates) == 0 || len(challenge.PossibleDuplicates) > 5 || len(challenge.PossibleDuplicates) != len(stored.Candidates) {
		return nil, workItemStorageUnavailable()
	}
	for i, candidate := range challenge.PossibleDuplicates {
		if candidate.WorkItemID != stored.Candidates[i].WorkItemID || int64(candidate.Number) != stored.Candidates[i].Number {
			return nil, workItemStorageUnavailable()
		}
	}
	if err := s.validateWorkItemChallengeReplay(ctx, scope, stored); err != nil {
		return nil, err
	}
	if scope.credential.HostedRole != "reporter" {
		return raw, nil
	}
	visible := make([]possibleDuplicate, 0, len(challenge.PossibleDuplicates))
	for _, candidate := range challenge.PossibleDuplicates {
		_, _, err := s.readWorkItemRecord(ctx, scope, string(candidate.WorkItemID))
		if isNativeNotFound(err) || errors.Is(err, corestore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		visible = append(visible, candidate)
	}
	if len(visible) == 0 {
		return nil, nativePublicInvalid("No challenged issues are still visible; use a new request ID")
	}
	challenge.PossibleDuplicates = visible
	return json.Marshal(challenge)
}

func (s *Service) validateWorkItemChallengeReplay(ctx context.Context, scope nativeScope, challenge corestore.DuplicateChallenge) error {
	var project projectstore.Record
	var revision policystore.Revision
	var found bool
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		project, err = r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		revision, err = r.ProjectPolicy(ctx, scope.project)
		found = err == nil
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return projectRecordError(err)
	}
	policyHash, err := workItemDuplicatePolicyHash(revision, found)
	if err != nil {
		return err
	}
	if policyHash != challenge.PolicyHash {
		return policyMismatch("The approved policy changed after the duplicate challenge")
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
		return policyMismatch("The configured workflow requires its approved policy")
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		if err := lockWorkItemDuplicateSource(ctx, w, scope.project, challenge.SourceSequence); err != nil {
			if errors.Is(err, searchstore.ErrIndexPending) {
				return nativeConflict(0)
			}
			return err
		}
		if !s.config.now().Before(challenge.ExpiresAt) {
			return receiptstore.ErrExpired
		}
		challenges, ok := w.(corestore.DuplicateChallengeReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		stored, err := challenges.DuplicateChallenge(ctx, scope.project, challenge.Token)
		if errors.Is(err, corestore.ErrNotFound) {
			return receiptstore.ErrExpired
		}
		if err != nil {
			return err
		}
		if stored.PrincipalID != challenge.PrincipalID || stored.RequestID != challenge.RequestID || stored.ContentHash != challenge.ContentHash || stored.PolicyHash != challenge.PolicyHash || stored.SourceSequence != challenge.SourceSequence || !slices.Equal(stored.Candidates, challenge.Candidates) {
			return nativeConflict(0)
		}
		current, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if current.Core.DeletedAt != nil || !sameWorkItemProjectPlan(project, current) {
			return nativeConflict(0)
		}
		currentPolicy, err := w.ProjectPolicy(ctx, scope.project)
		if errors.Is(err, corestore.ErrNotFound) && !found {
			err = nil
		} else if err == nil && (!found || !sameNativeClaimProjectPolicy(revision, currentPolicy)) {
			return nativeConflict(0)
		}
		if err != nil {
			return err
		}
		for _, candidate := range challenge.Candidates {
			item, err := w.LockWorkItem(ctx, scope.project, candidate.WorkItemID)
			if err != nil {
				return err
			}
			if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
				return nativeNotFound()
			}
			if item.Archived || item.Number != candidate.Number || item.Revision != candidate.Revision {
				return nativeConflict(tracker.Revision(item.Revision))
			}
		}
		return nil
	})
	return projectRecordError(err)
}

func (s *Service) prepareWorkItemDuplicateLinks(ctx context.Context, scope nativeScope, project projectstore.Record, issue tracker.NativeIssue, confirmation *workItemDuplicateConfirmation) ([]workItemDuplicateLink, error) {
	if confirmation == nil {
		return nil, nil
	}
	content := eventstore.NewContent(s.runDetailStore())
	links := make([]workItemDuplicateLink, 0, len(confirmation.selected))
	for _, candidate := range confirmation.selected {
		issue.Revision++
		related := confirmation.issues[candidate.WorkItemID]
		issue.Related = append(slices.Clone(issue.Related), tracker.NativeDependency{ID: related.WorkItemID, ProjectID: related.ProjectID, Number: related.Number, Title: related.Title, State: related.State, Terminal: related.Terminal, Dispatchable: dispatchableProjectState(project, related.State), Identifier: string(related.ProjectID) + "#" + strconv.Itoa(related.Number), WebURL: related.WebURL})
		raw, err := json.Marshal(issue)
		if err != nil {
			return nil, err
		}
		version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
		if err != nil {
			return nil, err
		}
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: issue.WorkItemID, ID: newNativeID("evt"), Type: "issue.edited", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: issue.UpdatedAt, Data: tracker.CollaborationData{Revision: issue.Revision, Fields: []string{"related"}, RelatedWorkItemID: candidate.WorkItemID, Operation: "add"}})
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.organization, eventRaw)
		if err != nil {
			return nil, err
		}
		links = append(links, workItemDuplicateLink{candidate: candidate, issue: issue, version: version, event: event})
	}
	return links, nil
}

func (s *Service) prepareWorkItemDuplicateConfirmation(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (*workItemDuplicateConfirmation, error) {
	if len(request.ChallengeToken) > 128 || len(request.NotDuplicateOf) < 1 || len(request.NotDuplicateOf) > 5 || request.GitHubIssueURL != "" || request.Provenance != nil {
		return nil, nativePublicInvalid("Supply a duplicate challenge token and one to five issue numbers together")
	}
	hash, err := duplicateContentHash(request)
	if err != nil {
		return nil, err
	}
	confirmation := &workItemDuplicateConfirmation{issues: make(map[tracker.NativeWorkItemID]tracker.NativeIssue, len(request.NotDuplicateOf))}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		challenges, ok := reader.(corestore.DuplicateChallengeReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		stored, err := challenges.DuplicateChallenge(ctx, scope.project, request.ChallengeToken)
		if errors.Is(err, corestore.ErrNotFound) {
			return receiptstore.ErrExpired
		}
		if err != nil {
			return err
		}
		if stored.PrincipalID != scope.actor().PrincipalID || stored.RequestID != request.IdempotencyKey || stored.ContentHash != hash || !s.config.now().Before(stored.ExpiresAt) {
			return receiptstore.ErrExpired
		}
		confirmation.challenge = stored
		project, err := reader.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil {
			return nativeNotFound()
		}
		organization, err := reader.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		for _, number := range request.NotDuplicateOf {
			if slices.ContainsFunc(confirmation.selected, func(candidate corestore.DuplicateCandidate) bool { return candidate.Number == int64(number) }) {
				return nativePublicInvalid("Not duplicate of must contain unique issue numbers")
			}
			index := slices.IndexFunc(stored.Candidates, func(candidate corestore.DuplicateCandidate) bool { return candidate.Number == int64(number) })
			if index < 0 {
				return nativePublicInvalid("Not duplicate of must name a challenged issue number")
			}
			candidate := stored.Candidates[index]
			item, err := reader.WorkItem(ctx, scope.project, candidate.WorkItemID)
			if err != nil || !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				return nativeNotFound()
			}
			if item.Archived || item.Number != candidate.Number || item.Revision != candidate.Revision {
				return nativeConflict(tracker.Revision(item.Revision))
			}
			confirmation.selected = append(confirmation.selected, candidate)
			state := stringValue(item.WorkflowStateName)
			confirmation.issues[candidate.WorkItemID] = s.nativeIssueResponse(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item.NativeID, Number: int(item.Number), Revision: tracker.Revision(item.Revision)}, Title: item.Title, State: state, Terminal: tenantProjectStateTerminal(project, state), WebURL: issueBrowserPath(organization.Slug, project.Core.Slug, int(item.Number))})
		}
		return nil
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	return confirmation, nil
}

func consumeWorkItemDuplicateConfirmation(ctx context.Context, writer tenantstore.Writer, scope nativeScope, confirmation *workItemDuplicateConfirmation, now time.Time) error {
	challenges, ok := writer.(corestore.DuplicateChallengeWriter)
	if !ok {
		return workItemStorageUnavailable()
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
		item, err := writer.LockWorkItem(ctx, scope.project, candidate.WorkItemID)
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		if item.Archived || item.Number != candidate.Number || item.Revision != candidate.Revision {
			return nativeConflict(tracker.Revision(item.Revision))
		}
	}
	return nil
}

type workItemDuplicateSearch struct {
	available bool
	sequence  int64
	matches   []possibleDuplicate
}

func workItemDuplicatePolicyHash(revision policystore.Revision, found bool) (string, error) {
	return nativeCommandHash(struct {
		Revision policystore.Revision
		Found    bool
	}{revision, found})
}

func (s *Service) prepareWorkItemDuplicates(ctx context.Context, scope nativeScope, request tracker.CreateIssue, machine bool) (*workItemDuplicateSearch, error) {
	text := request.Title + "\n" + request.Body
	if origin, stamped := issueorigin.Parse(request.Body); stamped {
		text = origin.Fingerprint + "\n" + issueorigin.DefectFingerprint(request.Body) + "\n" + text
	}
	query := searchstore.DuplicateSourceQuery{ProjectID: scope.project, Machine: machine, Request: &searchstore.Text{Title: request.Title, Body: request.Body}, Text: nativeSearchBytePrefix(text, searchstore.MaxQueryTextBytes), Now: s.config.now()}
	if scope.credential.HostedRole == "reporter" {
		query.Reporter = new(scope.actor().PrincipalID)
	}
	var page searchstore.DuplicateSourcePage
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
			return err
		}
		reader, ok := r.(searchstore.DuplicateSourceReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		page, err = reader.DuplicateSources(ctx, query)
		return err
	})
	if errors.Is(err, searchstore.ErrIndexPending) || err == nil && !page.State.Ready {
		s.config.Logger.InfoContext(ctx, "duplicate search unavailable; allowing canonical filing", "availability", "unavailable")
		return &workItemDuplicateSearch{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(page.Sources) > 5 {
		return nil, workItemStorageUnavailable()
	}
	content := searchstore.NewContent(s.runDetailStore())
	var matches []searchstore.DuplicateMatch
	for _, source := range page.Sources {
		text, err := content.Read(ctx, source.Document)
		if err != nil {
			return nil, err
		}
		source.Statistics = searchstore.Describe(source.Document, text)
		parent := text
		if source.Document.Kind == "comment" || source.Document.Payload != source.Parent.Payload {
			parent, err = content.Read(ctx, source.Parent)
			if err != nil {
				return nil, err
			}
		}
		match, matched := searchstore.MatchDuplicateSource(source, text, parent, searchstore.Text{Title: request.Title, Body: request.Body})
		if matched && (!machine || match.Fingerprint) {
			matches = searchstore.MergeDuplicateMatches(matches, []searchstore.DuplicateMatch{match})
		}
	}
	search := &workItemDuplicateSearch{available: true, sequence: page.State.Sequence}
	for _, match := range matches {
		source := match.Source
		item, err := s.readWorkItem(ctx, scope, string(source.Document.WorkItemID))
		if err != nil {
			return nil, err
		}
		if item.Archived || item.Revision != source.Revision {
			return nil, nativeConflict(item.Revision)
		}
		why := "Matching terms: " + strings.Join(match.Terms[:min(6, len(match.Terms))], ", ")
		if match.Fingerprint {
			why = "Matching machine fingerprint"
		}
		search.matches = append(search.matches, possibleDuplicate{WorkItemID: item.WorkItemID, Number: item.Number, Title: item.Title, State: item.State, revision: item.Revision, terminal: source.Terminal, Why: why, Identifier: string(scope.project) + "#" + strconv.Itoa(item.Number)})
	}
	return search, nil
}

func lockWorkItemDuplicateSource(ctx context.Context, w tenantstore.Writer, project tracker.ProjectID, sequence int64) error {
	writer, ok := w.(searchstore.DuplicateSourceWriter)
	if !ok {
		return workItemStorageUnavailable()
	}
	err := writer.LockDuplicateSource(ctx, project, sequence)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nativeConflict(0)
	}
	return err
}

type workItemMachinePriority struct {
	plan     workItemRecordPlan
	fields   workItemRecordFields
	version  eventstore.CollaborationVersion
	event    eventstore.Event
	document searchstore.Document
}

func (s *Service) prepareWorkItemMachinePriority(ctx context.Context, scope nativeScope, occurrence workItemRecordPlan, priority *int) (*workItemMachinePriority, error) {
	if err := requireNativeEdit(occurrence.issue, occurrence.issue.Revision); err != nil {
		return nil, err
	}
	plan := workItemRecordPlan{project: occurrence.project, current: occurrence.current, issue: occurrence.issue, fields: []string{"priority"}, eventType: "issue.edited"}
	plan.issue.Priority = priority
	plan.issue.Revision++
	plan.issue.UpdatedAt = s.config.now().UTC().Truncate(time.Microsecond)
	plan.issue.LastActivityAt = plan.issue.UpdatedAt
	fields, err := s.prepareWorkItemFields(ctx, scope.organization, plan)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(plan.issue)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RecordID: string(plan.issue.WorkItemID), Revision: int64(plan.issue.Revision)}, raw)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: plan.issue.WorkItemID, ID: newNativeID("evt"), Type: plan.eventType, SchemaVersion: 1, Actor: scope.actor(), RecordedAt: plan.issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.issue.Revision, Fields: plan.fields}})
	if err != nil {
		return nil, err
	}
	event, err := content.PrepareEvent(ctx, scope.organization, raw)
	if err != nil {
		return nil, err
	}
	document := searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, ID: "issue:" + string(plan.issue.WorkItemID), SourceID: string(plan.issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(plan.issue.Revision), UpdatedAt: plan.issue.UpdatedAt}
	document, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, document, searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels})
	if err != nil {
		return nil, err
	}
	return &workItemMachinePriority{plan: plan, fields: fields, version: version, event: event, document: document}, nil
}

func (p *workItemMachinePriority) write(ctx context.Context, w tenantstore.Writer) error {
	if err := writeWorkItemRecord(ctx, w, p.plan, p.fields); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, p.version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, p.event); err != nil {
		return err
	}
	return w.PutSearch(ctx, p.document, p.plan.current.Revision)
}
