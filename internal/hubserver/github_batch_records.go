package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) loadGitHubBatchRecord(ctx context.Context, scope nativeScope) (*tracker.GitHubBatch, error) {
	if s.config.TenantRepository == nil {
		return nil, ErrPostgresFeatureUnavailable
	}
	var row projectstore.IntakeBatch
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		intakeBatchReader, ok := r.(projectstore.IntakeBatchReader)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		row, err = intakeBatchReader.ProjectIntakeBatch(ctx, scope.project)
		return err
	})
	if err != nil {
		return nil, err
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, row.Payload)
	if err != nil {
		return nil, err
	}
	var batch tracker.GitHubBatch
	if err := json.Unmarshal(raw, &batch); err != nil {
		return nil, err
	}
	if batch.ID != row.ID || batch.Revision != row.Revision || batch.Status != row.Status || batch.HubOwned != row.HubOwned {
		return nil, eventstore.ErrInvalidRecord
	}
	return &batch, nil
}

func (s *Service) prepareGitHubBatchRecord(ctx context.Context, scope nativeScope, batch *tracker.GitHubBatch) (projectstore.IntakeBatch, error) {
	raw, err := json.Marshal(batch)
	if err != nil {
		return projectstore.IntakeBatch{}, err
	}
	payload, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, raw)
	return projectstore.IntakeBatch{ProjectID: scope.project, ID: batch.ID, Revision: batch.Revision, Status: batch.Status, HubOwned: batch.HubOwned, Payload: payload}, err
}

func (s *Service) saveGitHubBatchRecord(ctx context.Context, scope nativeScope, batch *tracker.GitHubBatch) error {
	expected := batch.Revision
	batch.Revision++
	row, err := s.prepareGitHubBatchRecord(ctx, scope, batch)
	if err != nil {
		return err
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Background, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		intakeBatchWriter, ok := w.(projectstore.IntakeBatchWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return intakeBatchWriter.SaveProjectIntakeBatch(ctx, row, expected)
	})
}

func (s *Service) commandGitHubBatchRecord(ctx context.Context, scope nativeScope, operation string, request tracker.GitHubBatchCommand) (json.RawMessage, error) {
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	batch, err := s.loadGitHubBatchRecord(ctx, scope)
	if errors.Is(err, corestore.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	expected := int64(0)
	if batch != nil {
		expected = batch.Revision
	}
	if request.Revision != expected {
		return nil, nativeConflict(tracker.Revision(expected))
	}
	repository := project.Settings.CheckoutRepository
	if repository == "" {
		repository = project.Settings.Repository
	}
	if project.Core.Profile != "native" || repository == "" || project.Settings.Intake != "manual" {
		return nil, intakeInvalid("Enable manual import on a repository-backed native project")
	}
	switch request.Action {
	case "start", "discover":
		if batch != nil && (batchHasPending(batch) || slices.ContainsFunc(batch.Items, func(i tracker.GitHubBatchItem) bool { return i.Status == "failed" })) {
			return nil, intakeInvalid("Finish the current intake before starting another preview")
		}
		if s.config.ImportBackend == nil {
			return nil, nativePublicInvalid("GitHub import transport is unavailable")
		}
		if len(request.Labels) > 20 {
			return nil, intakeInvalid("At most 20 label filters are allowed")
		}
		for _, label := range request.Labels {
			if strings.TrimSpace(label) == "" || len(label) > 100 {
				return nil, intakeInvalid("Label filters must be nonempty and at most 100 bytes")
			}
		}
		if err := validateBatchProjectSource(project.States, request.ProjectV2); err != nil {
			return nil, err
		}
		actor := scope.actor()
		limit, err := s.githubBatchRecordLimit(ctx, scope)
		if err != nil {
			return nil, err
		}
		batch = &tracker.GitHubBatch{ID: newNativeID("intake"), Revision: expected, HubOwned: true, Automatic: request.Action == "start", RequestedBy: &actor, Limit: limit, Discovery: tracker.GitHubDiscovery{Repository: strings.ToLower(repository), Snapshots: true, Labels: request.Labels, IncludeClosed: request.IncludeClosed, ProjectV2: request.ProjectV2}, Status: "discovering", Page: tracker.GitHubDiscoveryPage{Issues: []tracker.GitHubIssuePreview{}}, Items: []tracker.GitHubBatchItem{}}
		if request.Action == "start" {
			if err := configureBatchRecordDestination(project.States, batch, request); err != nil {
				return nil, err
			}
		}
	case "more":
		if batch == nil || batch.Status != "preview" || batch.Page.NextCursor == "" || len(batch.Page.Issues) >= batchLimit(batch) {
			return nil, intakeInvalid("No further preview page is available")
		}
		batch.Discovery.Cursor, batch.Status = batch.Page.NextCursor, "discovering"
	case "apply":
		if batch == nil || batch.Status != "preview" || len(request.Numbers) == 0 || len(request.Numbers) > batchLimit(batch) {
			return nil, intakeInvalid("Select issues from the completed preview")
		}
		if err := configureBatchRecordDestination(project.States, batch, request); err != nil {
			return nil, err
		}
		for _, number := range request.Numbers {
			if !slices.ContainsFunc(batch.Page.Issues, func(p tracker.GitHubIssuePreview) bool { return p.Number == number }) {
				return nil, intakeInvalid("Selected issue is absent from the preview")
			}
			batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: number, Status: "pending"})
		}
		batch.Status = "importing"
	case "retry":
		if batch == nil {
			return nil, intakeInvalid("No intake exists")
		}
		if err := batchRetryAllowed(batch.RetryAt, s.config.now()); err != nil {
			return nil, err
		}
		batch.Error, batch.RetryAt = "", ""
		if batch.Status == "discovery_failed" {
			batch.Status = "discovering"
		} else {
			for i := range batch.Items {
				if batch.Items[i].Status == "failed" {
					batch.Items[i].Status = "pending"
				}
			}
			batch.Status = "importing"
		}
	case "cancel":
		if batch == nil {
			return nil, intakeInvalid("No intake exists")
		}
		batch.Status = "cancelled"
	default:
		return nil, intakeInvalid("Unknown intake action")
	}
	batch.Revision++
	row, err := s.prepareGitHubBatchRecord(ctx, scope, batch)
	if err != nil {
		return nil, err
	}
	return s.tenantReceiptMutation(ctx, scope, operation, request.Mutation, request, projectGitHubBatchView(project.States, batch), func(w tenantstore.Writer) error {
		if err := s.database.requireHostedFeatureRecord(ctx, w, "github_integration", s.config.now()); err != nil {
			return err
		}
		current, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if current.Settings.Revision != project.Settings.Revision {
			return corestore.ErrRevisionConflict
		}
		intakeBatchWriter, ok := w.(projectstore.IntakeBatchWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return intakeBatchWriter.SaveProjectIntakeBatch(ctx, row, expected)
	})
}

func configureBatchRecordDestination(states []tracker.NativeState, batch *tracker.GitHubBatch, request tracker.GitHubBatchCommand) error {
	destination := request.Destination
	if destination == "" {
		for _, state := range states {
			if !state.Dispatchable && !state.Terminal && !state.OperatorOnly {
				destination = state.Name
				break
			}
		}
	}
	lane, ok := findBatchLane(states, destination)
	if !ok || lane.Dispatchable && !request.AllowDispatch {
		return intakeInvalid("Select a destination and explicitly confirm any dispatchable lane")
	}
	if len(request.LabelLanes) > 100 {
		return intakeInvalid("At most 100 label lane mappings are allowed")
	}
	for label, name := range request.LabelLanes {
		lane, ok := findBatchLane(states, name)
		if label == "" || len(label) > 100 || !ok || lane.Dispatchable && !request.AllowDispatch {
			return intakeInvalid("Select valid label lane mappings")
		}
	}
	if err := validateBatchProjectSource(states, batch.Discovery.ProjectV2); err != nil {
		return err
	}
	batch.Destination, batch.AllowDispatch, batch.LabelLanes, batch.IgnoreLanes = destination, request.AllowDispatch, request.LabelLanes, request.IgnoreLanes
	return nil
}

func (s *Service) importGitHubSnapshotRecord(ctx context.Context, scope nativeScope, key string, preview tracker.GitHubIssuePreview, snapshot tracker.GitHubIssueSnapshot, state string, labels []string) (tracker.NativeIssue, error) {
	canonical, repository, number, err := tracker.ParseGitHubIssueURL(preview.URL)
	if err != nil || !strings.EqualFold(repository, scope.integrationRepository) || number != preview.Number || snapshot.URL != canonical {
		return tracker.NativeIssue{}, nativePublicInvalid("GitHub source does not match its repository")
	}
	scope.scheduledRepair = s.observeImportedScheduledRepair(ctx, scope, snapshot)
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, GitHubIssueURL: canonical, State: state, Labels: labels}
	raw, err := s.createNativeIssueCommand(ctx, scope, request)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return issue, err
	}
	raw, err = s.applyLinkedIssueRecord(ctx, scope, issue.WorkItemID, "github.snapshot", tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: key + ":snapshot"}, Snapshot: snapshot})
	if err != nil {
		return issue, err
	}
	err = json.Unmarshal(raw, &issue)
	return issue, err
}

func (s *Service) advanceGitHubBatchRecord(ctx context.Context, scope nativeScope) error {
	batch, err := s.loadGitHubBatchRecord(ctx, scope)
	if err != nil || batch == nil {
		return err
	}
	if batch.Status != "discovering" && batch.Status != "importing" {
		return nil
	}
	scope.githubIntakeRevision = &githubIntakeRevision{batchID: batch.ID, revision: batch.Revision}
	if batch.Status == "discovering" {
		if s.config.ImportBackend == nil {
			return nativePublicInvalid("GitHub import transport is unavailable")
		}
		page, fetchErr := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: batch.Discovery.Repository, Stage: "batch", Discovery: &batch.Discovery})
		if fetchErr != nil {
			return fetchErr
		}
		if page.BatchPage == nil || len(page.BatchPage.Issues) > githubBatchMaintenanceLimit || len(batch.Page.Issues)+len(page.BatchPage.Issues) > batchLimit(batch) {
			return nativePublicInvalid("GitHub discovery exceeded its page budget")
		}
		for _, preview := range page.BatchPage.Issues {
			if slices.ContainsFunc(batch.Page.Issues, func(p tracker.GitHubIssuePreview) bool { return p.Number == preview.Number }) {
				continue
			}
			batch.Page.Issues = append(batch.Page.Issues, preview)
			if batch.Automatic {
				batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: preview.Number, Status: "pending"})
			}
		}
		batch.Page.Total, batch.Page.NextCursor = page.BatchPage.Total, page.BatchPage.NextCursor
		batch.Status = "preview"
		if batch.Automatic {
			batch.Status = "importing"
		}
		if err := s.saveGitHubBatchRecord(ctx, scope, batch); err != nil {
			return err
		}
		scope.githubIntakeRevision.revision = batch.Revision
	}
	if !batch.Automatic && batch.Status == "preview" {
		return nil
	}
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return err
	}
	processed := 0
	for i := range batch.Items {
		item := &batch.Items[i]
		if item.Status != "pending" {
			continue
		}
		if processed >= githubBatchMaintenanceLimit {
			break
		}
		processed++
		index := slices.IndexFunc(batch.Page.Issues, func(p tracker.GitHubIssuePreview) bool { return p.Number == item.Number })
		if index < 0 {
			return eventstore.ErrInvalidRecord
		}
		preview := batch.Page.Issues[index]
		snapshot := preview.Snapshot
		if snapshot == nil {
			source, err := s.fetchLinkedSnapshot(ctx, scope, preview.URL)
			if err != nil {
				return err
			}
			snapshot = source
		}
		destination, labels, err := batchIssueDestination(project.States, batch, preview)
		if err != nil {
			return err
		}
		issue, err := s.importGitHubSnapshotRecord(ctx, scope, batch.ID+":"+preview.ID, preview, *snapshot, destination, labels)
		if err != nil {
			return err
		}
		item.WorkItemID, item.Status = issue.WorkItemID, "completed"
		item.UnmappedProjectStatus = batchUnmappedProjectStatus(batch, preview)
		batch.Page.Issues[index].Body = ""
		batch.Page.Issues[index].Snapshot = nil
	}
	if !slices.ContainsFunc(batch.Items, func(i tracker.GitHubBatchItem) bool { return i.Status == "pending" }) {
		if batch.Automatic && batch.Page.NextCursor != "" && len(batch.Page.Issues) < batchLimit(batch) {
			batch.Discovery.Cursor, batch.Status = batch.Page.NextCursor, "discovering"
		} else {
			batch.Status = "finished"
		}
	}
	return s.saveGitHubBatchRecord(ctx, scope, batch)
}

func (s *Service) githubBatchRecordLimit(ctx context.Context, scope nativeScope) (int, error) {
	if s.database.plans() == nil {
		return 1000, nil
	}
	var entitlement HostedEntitlement
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		entitlement, err = s.database.hostedEntitlementRecord(ctx, r, s.config.now())
		return err
	})
	if err != nil {
		return 0, err
	}
	limit := int(min(entitlement.Allowances["unarchived_issues"], 50000))
	if limit <= 0 {
		return 0, intakeInvalid("The plan does not allow issue import")
	}
	return limit, nil
}
