package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CutoverRequest struct {
	ClosedState string `json:"closed_state"`
	tracker.Mutation
	DryRun         bool                  `json:"dry_run"`
	Checkpoint     string                `json:"checkpoint"`
	AcceptPartial  bool                  `json:"accept_partial"`
	CloseSource    bool                  `json:"close_source"`
	DestinationURL string                `json:"destination_url"`
	States         []tracker.NativeState `json:"states"`
	InitialState   string                `json:"initial_state"`
}

type CutoverReceipt struct {
	Checkpoint             string             `json:"checkpoint"`
	Applied                bool               `json:"applied"`
	Issues                 int                `json:"issues"`
	IncompleteImports      int                `json:"incomplete_imports"`
	UnresolvedDependencies int                `json:"unresolved_dependencies"`
	Blockers               []string           `json:"blockers"`
	HistoryLimitations     []string           `json:"history_limitations"`
	CloseSource            bool               `json:"close_source"`
	Integration            ProjectIntegration `json:"integration"`
}

type cutoverSourceRecord struct {
	Item        corestore.WorkItem
	Version     eventstore.CollaborationVersion
	Import      projectstore.GitHubImport
	ImportFound bool
	Records     []projectstore.GitHubImportRecord
}

type cutoverSourceSnapshot struct {
	Project projectstore.Record
	Policy  policystore.Revision
	Sources []cutoverSourceRecord
	Busy    bool
}

type preparedCutoverItem struct {
	current      corestore.WorkItem
	issue        tracker.NativeIssue
	version      eventstore.CollaborationVersion
	event        eventstore.Event
	dependencies []tracker.NativeWorkItemID
	outbound     *projectstore.GitHubOutbound
}

func (s *Service) cutoverProject(c echo.Context) error {
	var request CutoverRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	scope.RequireHostedAdmin = true
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, nil)
}

func readCutoverSourceSnapshot(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, now time.Time) (cutoverSourceSnapshot, error) {
	var snapshot cutoverSourceSnapshot
	var err error
	snapshot.Project, err = r.ProjectRecord(ctx, scope.Project)
	if err != nil {
		return snapshot, err
	}
	snapshot.Policy, err = r.ProjectPolicy(ctx, scope.Project)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return snapshot, err
	}
	imports, ok := r.(projectstore.GitHubImportReader)
	if !ok {
		return snapshot, hubapi.NativeWorkItemStorageUnavailable()
	}
	records, ok := r.(projectstore.GitHubImportRecordReader)
	if !ok {
		return snapshot, hubapi.NativeWorkItemStorageUnavailable()
	}
	after := int64(0)
	for {
		items, err := r.WorkItems(ctx, scope.Project, after, corestore.MaxPageSize)
		if err != nil {
			return snapshot, err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			source := cutoverSourceRecord{Item: item, Records: []projectstore.GitHubImportRecord{}}
			source.Version, err = r.CollaborationVersion(ctx, string(item.NativeID), item.Revision)
			if err != nil {
				return snapshot, err
			}
			source.Import, err = imports.GitHubImportForWorkItem(ctx, scope.Project, item.NativeID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return snapshot, err
			}
			source.ImportFound = err == nil
			if source.ImportFound {
				sequence := int64(0)
				for {
					page, err := records.GitHubImportRecords(ctx, scope.Project, source.Import.ID, sequence, corestore.MaxPageSize)
					if err != nil {
						return snapshot, err
					}
					count := min(len(page), corestore.MaxPageSize)
					source.Records = append(source.Records, page[:count]...)
					if len(page) <= corestore.MaxPageSize {
						break
					}
					if count == 0 || page[count-1].Sequence <= sequence {
						return snapshot, eventstore.ErrInvalidRecord
					}
					sequence = page[count-1].Sequence
				}
			}
			busy, err := r.WorkItemHasActiveLease(ctx, scope.Project, item.NativeID, now)
			if err != nil {
				return snapshot, err
			}
			snapshot.Busy = snapshot.Busy || busy
			snapshot.Sources = append(snapshot.Sources, source)
			if item.ID <= after {
				return snapshot, eventstore.ErrInvalidRecord
			}
			after = item.ID
		}
	}
	return snapshot, nil
}

func cutoverSourceFingerprint(snapshot cutoverSourceSnapshot, request CutoverRequest) (string, error) {
	snapshot.Project.Page = nil
	request.Mutation = tracker.Mutation{}
	request.DryRun = false
	request.Checkpoint = ""
	raw, err := json.Marshal(struct {
		Source  cutoverSourceSnapshot
		Request CutoverRequest
	}{snapshot, request})
	if err != nil {
		return "", err
	}
	return artifact.Digest(raw), nil
}

func cutoverSourceReference(issue tracker.NativeIssue) (string, int, error) {
	for _, reference := range issue.ExternalReferences {
		if reference.Provider != "github" || reference.Kind != "issue" {
			continue
		}
		_, repository, number, err := tracker.ParseGitHubIssueURL(reference.URL)
		if err != nil || reference.Repository != repository || reference.Number != number {
			return "", 0, eventstore.ErrInvalidRecord
		}
		return repository, number, nil
	}
	return "", 0, hubapi.NativePublicInvalid("Issue has no GitHub source")
}

func (s *Service) cutoverRecordDependency(ctx context.Context, row projectstore.GitHubImportRecord) (string, error) {
	if row.DependencyID != "" {
		return row.DependencyID, nil
	}
	if row.ProvenanceExternalID != "" {
		return row.ProvenanceExternalID, nil
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, row.Payload)
	if err != nil {
		return "", err
	}
	var historical GitHubImportRecord
	if json.Unmarshal(raw, &historical) != nil || historical.SourceKey != row.SourceKey || historical.Kind != "dependency" {
		return "", artifact.ErrIntegrity
	}
	var identity struct {
		NodeID string `json:"node_id"`
	}
	if json.Unmarshal(historical.Data, &identity) != nil || identity.NodeID == "" {
		return "", eventstore.ErrInvalidRecord
	}
	return identity.NodeID, nil
}

func (s *Service) inspectCutoverRecords(ctx context.Context, scope hubscope.NativeScope, source cutoverSourceSnapshot, request CutoverRequest) (CutoverReceipt, []tracker.NativeIssue, [][]tracker.NativeWorkItemID, error) {
	receipt := CutoverReceipt{Blockers: []string{}, HistoryLimitations: []string{}, CloseSource: request.CloseSource, Integration: projectIntegrationRecord(source.Project), Issues: len(source.Sources)}
	workflow, err := s.projectRecordWorkflow(ctx, source.Project)
	if err != nil {
		return receipt, nil, nil, err
	}
	receipt.Integration.WorkflowMarkdown = workflow
	if source.Project.Core.Profile != "github_compatible" {
		receipt.Blockers = append(receipt.Blockers, "Project is already native; automatic rollback is not supported")
	}
	if source.Busy {
		receipt.Blockers = append(receipt.Blockers, "Finish active project work before cutover")
	}
	issues := make([]tracker.NativeIssue, len(source.Sources))
	identities := map[string]tracker.NativeWorkItemID{}
	states := map[string]tracker.NativeState{}
	for _, state := range request.States {
		states[state.Name] = state
	}
	for i, row := range source.Sources {
		issue, err := s.hydrateCanonicalWorkItemRecord(ctx, row.Item, row.Version)
		if err != nil {
			return receipt, nil, nil, err
		}
		issues[i] = issue
		if issue.Provenance != nil && issue.Provenance.Provider == "github" && issue.Provenance.ExternalID != "" {
			identity := issue.Provenance.ExternalID
			if previous, found := identities[identity]; found && previous != issue.WorkItemID {
				return receipt, nil, nil, eventstore.ErrInvalidRecord
			}
			identities[identity] = issue.WorkItemID
		}
		if !row.ImportFound {
			receipt.IncompleteImports++
			receipt.Blockers = append(receipt.Blockers, "Import every source issue before cutover")
		} else {
			updated := row.Item.NativeUpdatedAt
			if issue.Provenance != nil && !issue.Provenance.UpdatedAt.IsZero() {
				updated = issue.Provenance.UpdatedAt
			}
			if row.Import.Status != "retrieved" || row.Import.SourceUpdatedAt == nil || !updated.Equal(*row.Import.SourceUpdatedAt) {
				receipt.IncompleteImports++
			}
			receipt.HistoryLimitations = append(receipt.HistoryLimitations, row.Import.Gaps...)
		}
		if issue.State != "" {
			if _, found := states[issue.State]; !found {
				receipt.Blockers = append(receipt.Blockers, "Native states must preserve every existing workflow state")
			}
		}
		if issue.Terminal || issue.ClosedAt != nil {
			if state, found := states[request.ClosedState]; !found || !state.Terminal {
				receipt.Blockers = append(receipt.Blockers, "Specify a terminal closed_state to preserve closed GitHub issues")
			}
		}
	}
	dependencies := make([][]tracker.NativeWorkItemID, len(source.Sources))
	for i, row := range source.Sources {
		for _, record := range row.Records {
			if record.Kind != "dependency" || !record.CurrentDependency {
				continue
			}
			identity, err := s.cutoverRecordDependency(ctx, record)
			if err != nil {
				return receipt, nil, nil, err
			}
			target, found := identities[identity]
			if !found {
				receipt.UnresolvedDependencies++
				continue
			}
			if target == row.Item.NativeID {
				return receipt, nil, nil, corestore.ErrInvalidRelationship
			}
			dependencies[i] = append(dependencies[i], target)
		}
		slices.Sort(dependencies[i])
		dependencies[i] = slices.Compact(dependencies[i])
	}
	dependenciesIncomplete := slices.ContainsFunc(source.Sources, func(row cutoverSourceRecord) bool {
		return row.ImportFound && slices.Contains([]string{"issue", "comments", "timeline", "dependencies"}, row.Import.Stage)
	})
	if dependenciesIncomplete {
		receipt.Blockers = append(receipt.Blockers, "Finish dependency retrieval before cutover; partial history acceptance does not omit dependency pages")
	}
	if receipt.IncompleteImports != 0 && !request.AcceptPartial {
		receipt.Blockers = append(receipt.Blockers, "Finish all import pages and reimport changed source issues, or explicitly accept partial history")
	}
	if receipt.UnresolvedDependencies != 0 {
		receipt.Blockers = append(receipt.Blockers, "Import unresolved dependency issues into this project before cutover")
	}
	receipt.Checkpoint, err = cutoverSourceFingerprint(source, request)
	return receipt, issues, dependencies, err
}

func (s *Service) cutoverProjectRecordCommand(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, request CutoverRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, options.OperationID, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	if err := hubrecords.ValidateNativeStates(request.States); err != nil {
		return nil, err
	}
	initial := false
	for _, state := range request.States {
		initial = initial || state.Name == request.InitialState
	}
	if !initial {
		return nil, hubapi.NativePublicInvalid("Initial state must be a configured native workflow state")
	}
	if request.CloseSource {
		destination, err := url.Parse(request.DestinationURL)
		if err != nil || destination.Host == "" || destination.User != nil || (destination.Scheme != "https" && destination.Scheme != "http") {
			return nil, hubapi.NativePublicInvalid("Link-and-close requires an absolute Detent destination URL")
		}
	}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	now := s.config.Clock().UTC()
	var snapshot cutoverSourceSnapshot
	err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, r, "github_integration", now)); err != nil {
			return err
		}
		var err error
		snapshot, err = readCutoverSourceSnapshot(ctx, r, scope, now)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	receipt, issues, dependencies, err := s.inspectCutoverRecords(ctx, scope, snapshot, request)
	if err != nil {
		return nil, err
	}
	var prepared []preparedCutoverItem
	var stored eventstore.Receipt
	if !request.DryRun {
		if len(receipt.Blockers) != 0 {
			return nil, hubapi.NativePublicInvalid(strings.Join(receipt.Blockers, "; "))
		}
		if request.Checkpoint == "" || request.Checkpoint != receipt.Checkpoint {
			return nil, hubapi.NativePublicInvalid("Cutover checkpoint changed; repeat the dry run and review its receipt")
		}
		if request.CloseSource && s.config.OutboxBackend == nil {
			return nil, hubapi.NativePublicInvalid("Link-and-close requires configured GitHub transport")
		}
		prepared, err = s.prepareCutoverItems(ctx, scope, snapshot, issues, dependencies, request, now)
		if err != nil {
			return nil, err
		}
		next := snapshot.Project
		next.Core.Profile = "native"
		next.Core.ContentSettingsRevision++
		next.Settings.Intake = "manual"
		next.Settings.Revision++
		next.States = hubrecords.NativeTriageStates(request.States)
		receipt.Applied = true
		receipt.Integration = projectIntegrationRecord(next)
		receipt.Integration.WorkflowMarkdown, err = s.projectRecordWorkflow(ctx, snapshot.Project)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			return nil, err
		}
		stored, err = eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, raw)
		if err != nil {
			return nil, err
		}
	}
	return s.tenantReceiptMutation(ctx, scope, options.OperationID, request.Mutation, request, receipt, func(w tenantstore.Writer) error {
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "github_integration", now)); err != nil {
			return err
		}
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		current, err := readCutoverSourceSnapshot(ctx, w, scope, now)
		if err != nil {
			return err
		}
		fingerprint, err := cutoverSourceFingerprint(current, request)
		if err != nil {
			return err
		}
		if fingerprint != receipt.Checkpoint {
			return hubapi.NativeConflict(tracker.Revision(current.Project.Settings.Revision))
		}
		if request.DryRun {
			return nil
		}
		owner, ok := w.(postgres.CutoverWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		queues := make([]postgres.CutoverQueueItem, len(prepared))
		for i, item := range prepared {
			queues[i] = postgres.CutoverQueueItem{WorkItemID: item.current.NativeID, ExpectedRevision: item.current.Revision, State: item.issue.State}
		}
		outbox, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err := outbox.SupersedeGitHubCompatibilityWrites(ctx, scope.Project, snapshot.Project.Settings.RepositoryEnabled, now); err != nil {
			return err
		}
		_, err = owner.CommitProjectCutover(ctx, postgres.CutoverCommit{ExpectedContentRevision: snapshot.Project.Core.ContentSettingsRevision, States: hubrecords.NativeTriageStates(request.States), Queues: queues, Receipt: pgquery.StoreCutoverReceiptParams{OrganizationID: scope.Organization, ProjectID: scope.Project, Checkpoint: receipt.Checkpoint, SourceProfile: "github_compatible", ExpectedRevision: snapshot.Project.Settings.Revision, ReceiptObjectKey: stored.PayloadObjectKey, ReceiptSha256: stored.PayloadSHA256, ReceiptBytes: stored.PayloadBytes, ReceiptContentBytes: stored.PayloadContentBytes, ActorID: scope.Credential.ID, CreatedAt: now}})
		if errors.Is(err, postgres.ErrCutoverQueuePosition) {
			return hubapi.NativePublicInvalid("Choose one queue position per work item before cutover; existing ranks have been retained")
		}
		if errors.Is(err, projectstore.ErrActiveWork) {
			return hubapi.NativePublicInvalid("Finish active work and reconcile pending source writes before cutover")
		}
		if err != nil {
			return err
		}
		for _, item := range prepared {
			current := item.current
			current.WorkflowStateName = new(item.issue.State)
			current.NativeUpdatedAt, current.UpdatedAt, current.LastActivityAt = now, now, now
			if _, err := w.UpdateWorkItem(ctx, current); err != nil {
				return err
			}
			author := item.issue.Actor.PrincipalID
			if item.issue.Provenance != nil {
				author = item.issue.Provenance.AuthorID
			}
			var waitSince *time.Time
			if slices.ContainsFunc(request.States, func(state tracker.NativeState) bool {
				return state.Name == item.issue.State && state.Dispatchable && !state.Terminal && !state.OperatorOnly
			}) {
				waitSince = &now
			}
			if err := w.PutWorkItemFacts(ctx, claimstore.WorkItemFacts{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.current.NativeID, Author: author, Repository: snapshot.Project.Settings.Repository, Assignees: item.issue.Assignees, DispatchWaitSince: waitSince}); err != nil {
				return err
			}
			for _, target := range item.dependencies {
				relationship := corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.current.NativeID, RelatedProjectID: scope.Project, RelatedWorkItemID: target, Type: "dependency", CompatibilityEdge: true}
				if err := w.SetRelationship(ctx, relationship, "add", now); err != nil {
					return err
				}
			}
			if _, err := w.CreateCollaborationVersion(ctx, item.version); err != nil {
				return err
			}
			if _, err := w.AppendEvent(ctx, item.event); err != nil {
				return err
			}
			if item.outbound != nil {
				if err := outbox.EnqueueGitHubOutbound(ctx, *item.outbound); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Service) prepareCutoverItems(ctx context.Context, scope hubscope.NativeScope, snapshot cutoverSourceSnapshot, issues []tracker.NativeIssue, dependencies [][]tracker.NativeWorkItemID, request CutoverRequest, now time.Time) ([]preparedCutoverItem, error) {
	result := make([]preparedCutoverItem, len(issues))
	states := map[string]tracker.NativeState{}
	for _, state := range request.States {
		states[state.Name] = state
	}
	byID := make(map[tracker.NativeWorkItemID]tracker.NativeIssue, len(issues))
	for _, issue := range issues {
		byID[issue.WorkItemID] = issue
	}
	for i, issue := range issues {
		next := issue
		next.Profile = "native"
		next.Revision++
		next.UpdatedAt = now
		if issue.Terminal || issue.ClosedAt != nil {
			next.State = request.ClosedState
		} else if issue.State == "" {
			next.State = request.InitialState
		}
		next.Terminal = states[next.State].Terminal
		next.Dependencies = slices.Clone(dependencies[i])
		next.Blockers = []tracker.NativeDependency{}
		next.DependencyReady = true
		for _, target := range dependencies[i] {
			if candidate, found := byID[target]; found {
				targetState := candidate.State
				if candidate.Terminal || candidate.ClosedAt != nil {
					targetState = request.ClosedState
				} else if targetState == "" {
					targetState = request.InitialState
				}
				terminal := states[targetState].Terminal
				next.Blockers = append(next.Blockers, tracker.NativeDependency{ID: target, ProjectID: scope.Project, Number: candidate.Number, Title: candidate.Title, State: targetState, Terminal: terminal, Dispatchable: states[targetState].Dispatchable, ClosedAt: candidate.ClosedAt})
				if !terminal {
					next.DependencyReady = false
				}
			}
		}
		if _, _, err := s.validateWorkItemRecordFilingTenant(ctx, scope, hubrecords.WorkItemRecordPlan{Project: snapshot.Project, Issue: next}); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return nil, err
		}
		version, err := eventstore.NewContent(s.runDetailStore()).PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: next.WorkItemID, RecordID: string(next.WorkItemID), Revision: int64(next.Revision)}, raw)
		if err != nil {
			return nil, err
		}
		eventID := "evt_cutover_" + artifact.Digest([]byte(request.Checkpoint+":"+string(next.WorkItemID)))
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: eventID, OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: next.WorkItemID, Type: "issue.cutover", SchemaVersion: 1, RecordedAt: now, Actor: scope.Actor(), Data: tracker.CollaborationData{Revision: next.Revision}})
		if err != nil {
			return nil, err
		}
		event, err := eventstore.NewContent(s.runDetailStore()).PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return nil, err
		}
		prepared := preparedCutoverItem{current: snapshot.Sources[i].Item, issue: next, version: version, event: event, dependencies: dependencies[i]}
		if request.CloseSource {
			repository, number, err := cutoverSourceReference(issue)
			if err != nil {
				return nil, err
			}
			body := fmt.Sprintf("This issue is now managed in Detent: %s\n\nNative work item: %s", request.DestinationURL, next.WorkItemID)
			outbound, err := s.prepareNativeSourceSummary(ctx, scope, next.WorkItemID, repository, number, request.Checkpoint+":"+string(next.WorkItemID), body, true, "")
			if err != nil {
				return nil, err
			}
			prepared.outbound = &outbound
		}
		result[i] = prepared
	}
	return result, nil
}

type projectNativeSummaryRequest struct {
	tracker.Mutation
	Body string `json:"body"`
}

func (s *Service) projectNativeSummary(c echo.Context) error {
	var request projectNativeSummaryRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, nil)
}

func (s *Service) prepareNativeSourceSummary(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, repository string, number int, key, body string, closeSource bool, stateReason string) (projectstore.GitHubOutbound, error) {
	if strings.TrimSpace(body) == "" || len(body) > 60<<10 {
		return projectstore.GitHubOutbound{}, hubapi.NativePublicInvalid("Summary must contain 1 byte to 60 KiB")
	}
	if _, _, valid := splitRepositoryFullName(repository); !valid || number < 1 {
		return projectstore.GitHubOutbound{}, hubapi.NativePublicInvalid("Issue has no GitHub source")
	}
	marker := "<!-- detent-summary:" + string(item) + " -->"
	if key == "intake" || strings.HasPrefix(key, "cancelled:") {
		marker = "<!-- detent-summary:" + string(item) + ":" + key + " -->"
	}
	desired, err := json.Marshal(WorkpadDesired{Phase: "summary", Body: body, Marker: marker, Summary: true, CloseSource: closeSource, StateReason: stateReason, IssueNumber: number})
	if err != nil {
		return projectstore.GitHubOutbound{}, err
	}
	payload, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, desired)
	if err != nil {
		return projectstore.GitHubOutbound{}, err
	}
	bucket := "summary:" + string(item)
	if closeSource {
		bucket = "cutover:" + string(item)
	}
	return projectstore.GitHubOutbound{ProjectID: scope.Project, WorkItemID: item, Key: "native:" + string(item) + ":" + key, Repository: repository, IssueNumber: int64(number), Kind: string(MutationWorkpad), CreatedAt: s.config.Clock().UTC(), Payload: payload, CoalesceKey: bucket}, nil
}

func (s *Service) projectNativeSummaryRecordCommand(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, request projectNativeSummaryRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, options.OperationID, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	if project.Core.Profile != "native" || project.Settings.Projection != "summary" {
		return nil, hubapi.NativePublicInvalid("Native summary projection is not enabled")
	}
	item, version, err := s.readWorkItemRecord(ctx, scope, options.Item)
	if err != nil {
		return nil, err
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.LinkedSource(ctx, scope.Project, item.NativeID)
		if err == nil {
			return hubapi.NativePublicInvalid("Linked source comments are private to Cloud")
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	issue, err := s.hydrateCanonicalWorkItemRecord(ctx, item, version)
	if err != nil {
		return nil, err
	}
	repository, number, err := cutoverSourceReference(issue)
	if err != nil {
		return nil, err
	}
	outbound, err := s.prepareNativeSourceSummary(ctx, scope, item.NativeID, repository, number, request.IdempotencyKey, request.Body, false, "")
	if err != nil {
		return nil, err
	}
	outbound.Key = "native:" + string(item.NativeID) + ":" + hubapi.NewID("projection") + ":" + request.IdempotencyKey
	result := struct {
		Status string `json:"status"`
	}{"pending"}
	return s.tenantReceiptMutation(ctx, scope, options.OperationID, request.Mutation, request, result, func(w tenantstore.Writer) error {
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, options.Feature, s.config.Clock())); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		latest, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if latest.Core.Profile != "native" || latest.Settings.Projection != "summary" || latest.Settings.Revision != project.Settings.Revision {
			return hubapi.NativeConflict(tracker.Revision(latest.Settings.Revision))
		}
		current, err := w.LockWorkItem(ctx, scope.Project, item.NativeID)
		if err != nil {
			return err
		}
		if current.Revision != item.Revision || !reflect.DeepEqual(current.NativeSourceKey, item.NativeSourceKey) {
			return hubapi.NativeConflict(tracker.Revision(current.Revision))
		}
		if scope.Credential.Scope == hubapi.APIScopeWorker {
			if err := s.authorizeAttachmentCommentLease(ctx, w, scope, item, request.Mutation); err != nil {
				return err
			}
		}
		if _, err := w.LinkedSource(ctx, scope.Project, item.NativeID); err == nil {
			return hubapi.NativePublicInvalid("Linked source comments are private to Cloud")
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		owner, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err := owner.SupersedeGitHubSummaries(ctx, scope.Project, outbound.CoalesceKey, s.config.Clock()); err != nil {
			return err
		}
		return owner.EnqueueGitHubOutbound(ctx, outbound)
	})
}
