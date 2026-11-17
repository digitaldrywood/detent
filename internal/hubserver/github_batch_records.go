package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type githubBatchRecord struct {
	tracker.GitHubBatch
	RequireAdminKey  bool                           `json:"require_admin_key,omitempty"`
	Issuer           *hubscope.OperatorChatIdentity `json:"issuer,omitempty"`
	FallbackRunnerID string                         `json:"fallback_runner_id,omitempty"`
}

func (s *Service) loadGitHubBatchRecord(ctx context.Context, scope hubscope.NativeScope) (*tracker.GitHubBatch, error) {
	record, err := s.loadGitHubBatchState(ctx, scope)
	if err != nil {
		return nil, err
	}
	return &record.GitHubBatch, nil
}

func (s *Service) loadGitHubBatchState(ctx context.Context, scope hubscope.NativeScope) (*githubBatchRecord, error) {
	if s.config.TenantRepository == nil {
		return nil, hubapi.ErrPostgresFeatureUnavailable
	}
	var row projectstore.IntakeBatch
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		intakeBatchReader, ok := r.(projectstore.IntakeBatchReader)
		if !ok {
			return hubapi.ErrPostgresFeatureUnavailable
		}
		row, err = intakeBatchReader.ProjectIntakeBatch(ctx, scope.Project)
		return err
	})
	if err != nil {
		return nil, err
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, row.Payload)
	if err != nil {
		return nil, err
	}
	var batch githubBatchRecord
	if err := json.Unmarshal(raw, &batch); err != nil {
		return nil, err
	}
	if row.ProjectID != scope.Project || row.Payload.OrganizationID != scope.Organization || batch.ID != row.ID || batch.Revision != row.Revision || batch.Status != row.Status || batch.HubOwned != row.HubOwned {
		return nil, eventstore.ErrInvalidRecord
	}
	return &batch, nil
}

func (s *Service) prepareGitHubBatchRecord(ctx context.Context, scope hubscope.NativeScope, batch *tracker.GitHubBatch) (projectstore.IntakeBatch, error) {
	record := githubBatchRecord{GitHubBatch: *batch}
	if scope.GitHubIntakeRevision != nil {
		record.RequireAdminKey = scope.GitHubIntakeRevision.RequireAdminKey
		record.Issuer = scope.GitHubIntakeRevision.Issuer
		record.FallbackRunnerID = scope.GitHubIntakeRevision.FallbackRunner
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return projectstore.IntakeBatch{}, err
	}
	payload, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, raw)
	return projectstore.IntakeBatch{ProjectID: scope.Project, ID: batch.ID, Revision: batch.Revision, Status: batch.Status, HubOwned: batch.HubOwned, Payload: payload}, err
}

func (s *Service) saveGitHubBatchRecord(ctx context.Context, scope hubscope.NativeScope, batch *tracker.GitHubBatch) error {
	expected := batch.Revision
	batch.Revision++
	row, err := s.prepareGitHubBatchRecord(ctx, scope, batch)
	if err != nil {
		return err
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Background, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		intakeBatchWriter, ok := w.(projectstore.IntakeBatchWriter)
		if !ok {
			return hubapi.ErrPostgresFeatureUnavailable
		}
		return intakeBatchWriter.SaveProjectIntakeBatch(ctx, row, expected)
	})
}

func (s *Service) commandGitHubBatchRecord(ctx context.Context, scope hubscope.NativeScope, operation string, request tracker.GitHubBatchCommand) (json.RawMessage, error) {
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return s.gitHubBatchRequestAuthority(ctx, reader, scope, scope.RequireHostedAdmin)
	}); err != nil {
		return nil, err
	}

	hash, err := hubapi.CommandHash(request)
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
	stored, err := s.loadGitHubBatchState(ctx, scope)
	if errors.Is(err, corestore.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	var batch *tracker.GitHubBatch
	if stored != nil {
		batch = &stored.GitHubBatch
	}
	origin, ok := ctx.Value(hubscope.OperatorEntryOriginKey{}).(string)
	if !ok || origin == "" {
		origin = operatortool.CurrentConnection(ctx).Origin
	}
	issuer := hubscope.OperatorChatIdentity{Identity: hubscope.OperatorIdentity(scope.Credential, string(scope.Organization)), Origin: origin}
	if !issuer.Valid() || scope.Credential.EntryKey != nil && origin == "" {
		return nil, operatortool.ErrAccessDenied
	}
	scope.GitHubIntakeRevision = &hubscope.GitHubIntakeRevision{Issuer: &issuer, RequireAdminKey: scope.RequireHostedAdmin}
	if stored != nil {
		scope.GitHubIntakeRevision.FallbackRunner = stored.FallbackRunnerID
	}
	if request.RunnerID != "" {
		scope.GitHubIntakeRevision.FallbackRunner = request.RunnerID
	}
	expected := int64(0)
	if batch != nil {
		expected = batch.Revision
	}
	if request.Revision != expected {
		return nil, hubapi.NativeConflict(tracker.Revision(expected))
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
		actor := scope.Actor()
		limit, err := s.githubBatchRecordLimit(ctx, scope)
		if err != nil {
			return nil, err
		}
		batch = &tracker.GitHubBatch{ID: hubapi.NewID("intake"), Revision: expected, HubOwned: true, Automatic: request.Action == "start", RequestedBy: &actor, Limit: limit, Discovery: tracker.GitHubDiscovery{Repository: strings.ToLower(repository), Snapshots: true, Labels: request.Labels, IncludeClosed: request.IncludeClosed, ProjectV2: request.ProjectV2}, Status: "discovering", Page: tracker.GitHubDiscoveryPage{Issues: []tracker.GitHubIssuePreview{}}, Items: []tracker.GitHubBatchItem{}}
		if s.config.ImportBackend == nil {
			if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
				return s.requireGitHubBatchRunner(ctx, reader, scope, scope.GitHubIntakeRevision.FallbackRunner, batch.Discovery.Repository, s.config.Clock())
			}); err != nil {
				return nil, err
			}
			batch.HubOwned, batch.RunnerID = false, scope.GitHubIntakeRevision.FallbackRunner
		}
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
		previews := githubBatchPreviewIndexes(batch.Page.Issues)
		selected := make(map[int]bool, len(batch.Items)+len(request.Numbers))
		for _, item := range batch.Items {
			selected[item.Number] = true
		}
		for _, number := range request.Numbers {
			if _, exists := previews[number]; !exists {
				return nil, intakeInvalid("Selected issue is absent from the preview")
			}
			if !selected[number] {
				batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: number, Status: "pending"})
				selected[number] = true
			}
		}
		batch.Status = "importing"
		actor := scope.Actor()
		batch.RequestedBy = &actor
	case "retry":
		if batch == nil {
			return nil, intakeInvalid("No intake exists")
		}
		if err := batchRetryAllowed(batch.RetryAt, s.config.Clock()); err != nil {
			return nil, err
		}
		for _, item := range batch.Items {
			if item.Status == "failed" {
				if err := batchRetryAllowed(item.RetryAt, s.config.Clock()); err != nil {
					return nil, err
				}
			}
		}
		if request.ProjectV2 != nil || request.Destination != "" || request.LabelLanes != nil || request.AllowDispatch || request.IgnoreLanes {
			if request.Destination == "" {
				request.Destination = batch.Destination
			}
			if err := configureBatchRecordDestination(project.States, batch, request); err != nil {
				return nil, err
			}
		}
		batch.Error, batch.RetryAt = "", ""
		if batch.Status == "discovery_failed" || batch.Status == "discovering" {
			batch.Status = "discovering"
		} else {
			previews := githubBatchPreviewIndexes(batch.Page.Issues)
			for i := range batch.Items {
				if batch.Items[i].Status == "failed" {
					batch.Items[i].Status, batch.Items[i].Error, batch.Items[i].RetryAt = "pending", "", ""
					index, found := previews[batch.Items[i].Number]
					if !found {
						return nil, eventstore.ErrInvalidRecord
					}
					batch.Page.Issues[index].Snapshot = nil
				}
			}
			batch.Status = "importing"
		}
		actor := scope.Actor()
		batch.RequestedBy = &actor
		if request.RunnerID != "" && !batch.HubOwned {
			if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
				return s.requireGitHubBatchRunner(ctx, reader, scope, request.RunnerID, batch.Discovery.Repository, s.config.Clock())
			}); err != nil {
				return nil, err
			}
			batch.RunnerID = request.RunnerID
		}
	case "close_source":
		if batch == nil {
			return nil, intakeInvalid("No completed intake exists")
		}
		return s.closeGitHubBatchSources(ctx, scope, operation, request, project, batch, expected)
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
		if err := s.gitHubBatchRequestAuthority(ctx, w, scope, scope.GitHubIntakeRevision.RequireAdminKey); err != nil {
			return err
		}
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "github_integration", s.config.Clock())); err != nil {
			return err
		}
		current, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if current.Settings.Revision != project.Settings.Revision {
			return corestore.ErrRevisionConflict
		}
		intakeBatchWriter, ok := w.(projectstore.IntakeBatchWriter)
		if !ok {
			return hubapi.ErrPostgresFeatureUnavailable
		}
		return intakeBatchWriter.SaveProjectIntakeBatch(ctx, row, expected)
	})
}

func configureBatchRecordDestination(states []tracker.NativeState, batch *tracker.GitHubBatch, request tracker.GitHubBatchCommand) error {
	destination := request.Destination
	if destination == "" {
		if _, ok := findBatchLane(states, "Backlog"); ok {
			destination = "Backlog"
		}
		for _, state := range states {
			if destination != "" {
				break
			}
			if !state.Dispatchable && !state.Terminal {
				destination = state.Name
				break
			}
		}
	}
	lane, exists := findBatchLane(states, destination)
	if !exists || lane.Dispatchable && !request.AllowDispatch {
		return intakeInvalid("Select a destination and explicitly confirm any dispatchable lane")
	}
	if len(request.LabelLanes) > 100 {
		return intakeInvalid("At most 100 label lane mappings are allowed")
	}
	for label, name := range request.LabelLanes {
		lane, exists := findBatchLane(states, name)
		if label == "" || len(label) > 100 || !exists || lane.Dispatchable && !request.AllowDispatch {
			return intakeInvalid("Select a destination and explicitly confirm any dispatchable lane")
		}
	}
	source := batch.Discovery.ProjectV2
	if request.ProjectV2 != nil {
		if source == nil || source.ID != request.ProjectV2.ID {
			return intakeInvalid("Select the ProjectV2 source before discovery; routing edits must retain that project ID")
		}
		source = request.ProjectV2
	}
	if err := validateBatchProjectSource(states, source); err != nil {
		return err
	}
	if source != nil {
		for _, name := range source.StatusLanes {
			lane, _ := findBatchLane(states, name)
			if lane.Dispatchable && !request.AllowDispatch {
				return intakeInvalid("Select a destination and explicitly confirm any dispatchable lane")
			}
		}
	}
	batch.Discovery.ProjectV2 = source
	batch.Destination, batch.AllowDispatch, batch.LabelLanes, batch.IgnoreLanes = destination, request.AllowDispatch, request.LabelLanes, request.IgnoreLanes
	return nil
}

func (s *Service) importGitHubSnapshotRecord(ctx context.Context, scope hubscope.NativeScope, key string, preview tracker.GitHubIssuePreview, snapshot tracker.GitHubIssueSnapshot, state string, labels []string) (tracker.NativeIssue, error) {
	canonical, repository, number, err := tracker.ParseGitHubIssueURL(preview.URL)
	if err != nil || !strings.EqualFold(repository, scope.IntegrationRepository) || number != preview.Number || snapshot.URL != canonical {
		return tracker.NativeIssue{}, hubapi.NativePublicInvalid("GitHub source does not match its repository")
	}
	scope.ScheduledRepair = s.observeImportedScheduledRepair(ctx, scope, snapshot)
	if verified := scope.ScheduledRepair; verified != nil && verified.ImportReason == "" && verified.PolicyID != "" {
		if err := hubrecords.ValidateNativeContent(snapshot.Title, snapshot.Body, nil, nil, nil); err != nil {
			return tracker.NativeIssue{}, err
		}
		if err := validateImportProvenance(&snapshot.Provenance); err != nil {
			return tracker.NativeIssue{}, err
		}
		seen := make(map[string]bool, len(snapshot.Comments))
		for _, comment := range snapshot.Comments {
			if err := validateImportProvenance(&comment.Provenance); err != nil {
				return tracker.NativeIssue{}, err
			}
			if len(comment.Body) > 64<<10 || seen[comment.Provenance.ExternalID] {
				return tracker.NativeIssue{}, hubapi.NativePublicInvalid("Intake discussion contains an oversized or duplicate comment")
			}
			seen[comment.Provenance.ExternalID] = true
		}
	}
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, GitHubIssueURL: canonical, State: state, Labels: labels}
	raw, err := s.createNativeIssueCommand(ctx, scope, request)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return issue, err
	}
	if issue.PublicationReused && (issue.LinkedSource == nil || issue.LinkedSource.URL != canonical) {
		return issue, nil
	}
	raw, err = s.applyLinkedIssueRecord(ctx, scope, issue.WorkItemID, "github.snapshot", tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: key + ":snapshot"}, Snapshot: snapshot})
	if err != nil {
		return issue, err
	}
	err = json.Unmarshal(raw, &issue)
	return issue, err
}

func githubBatchPreviewIndexes(previews []tracker.GitHubIssuePreview) map[int]int {
	indexes := make(map[int]int, len(previews))
	for index, preview := range previews {
		if _, exists := indexes[preview.Number]; !exists {
			indexes[preview.Number] = index
		}
	}
	return indexes
}

func validateGitHubBatchPage(batch *tracker.GitHubBatch, page tracker.GitHubDiscoveryPage) error {
	if page.Total < 0 || len(page.Issues) > githubBatchMaintenanceLimit || len(batch.Page.Issues)+len(page.Issues) > batchLimit(batch) {
		return hubapi.NativePublicInvalid("Invalid source discovery page or plan import limit exceeded")
	}
	seen, ids := map[int]bool{}, map[string]bool{}
	for _, preview := range batch.Page.Issues {
		seen[preview.Number], ids[preview.ID] = true, true
	}
	for _, preview := range page.Issues {
		canonical, repository, number, err := tracker.ParseGitHubIssueURL(preview.URL)
		if err != nil || repository != batch.Discovery.Repository || number != preview.Number || canonical != preview.URL || seen[number] || ids[preview.ID] || preview.ID == "" || preview.Title == "" || len(preview.Title) > 500 || len([]rune(preview.Body)) > 1000 || preview.Closed && !batch.Discovery.IncludeClosed {
			return hubapi.NativePublicInvalid("Mismatched or duplicate source preview")
		}
		seen[number], ids[preview.ID] = true, true
	}
	if page.NextCursor != "" && (page.NextCursor == batch.Discovery.Cursor || len(page.Issues) == 0) {
		return hubapi.NativePublicInvalid("Invalid source discovery cursor")
	}
	return nil
}

func githubBatchSourceFailure(err error, now time.Time, reported time.Time) (string, string) {
	message := "Source context incomplete: private access or partial page"
	var source *linkedSnapshotSourceError
	if errors.As(err, &source) && source.retryAt.After(reported) {
		reported = source.retryAt
	}
	var status *githubconnector.StatusError
	if errors.As(err, &status) && status.StatusCode == http.StatusTooManyRequests {
		message = "Source intake stopped at rate limit"
		deadline := now.Add(status.RetryAfter)
		if status.ResetAt.After(deadline) {
			deadline = status.ResetAt
		}
		if !deadline.After(now) {
			deadline = now.Add(time.Minute)
		}
		if deadline.After(reported) {
			reported = deadline
		}
	} else if errors.Is(err, githubconnector.ErrRateLimited) {
		message = "Source intake stopped at rate limit"
		if deadline := now.Add(time.Minute); deadline.After(reported) {
			reported = deadline
		}
	}

	retry := ""
	if reported.After(now) {
		retry = reported.UTC().Format(time.RFC3339Nano)
	}
	var diagnostic *hubapi.NativeError
	if errors.As(err, &diagnostic) && diagnostic.PublicMessage {
		message = diagnostic.Message
	}
	if len(message) > 1000 {
		message = "Source issue could not be imported"
	}
	return message, retry
}

func (s *Service) advanceGitHubBatchRecord(ctx context.Context, scope hubscope.NativeScope) error {
	record, err := s.loadGitHubBatchState(ctx, scope)
	if err != nil {
		return err
	}
	batch := &record.GitHubBatch
	if batch.Status != "discovering" && batch.Status != "importing" {
		return nil
	}
	scope, err = s.bindGitHubBatchIssuer(ctx, scope, record)
	if err != nil {
		return err
	}
	if !batch.HubOwned {
		return s.reconcileRunnerGitHubBatch(ctx, scope, batch)
	}
	if s.config.ImportBackend == nil {
		return hubapi.NativePublicInvalid("GitHub import transport is unavailable")
	}
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return err
	}
	if project.Settings.Intake != "manual" {
		return corestore.ErrNotFound
	}
	if batch.RequestedBy != nil {
		scope.SourceActor = batch.RequestedBy
	}
	save := func() error {
		if err := s.saveGitHubBatchRecord(ctx, scope, batch); err != nil {
			return err
		}
		scope.GitHubIntakeRevision.Revision = batch.Revision
		return nil
	}
	if batch.Status == "discovering" {
		page, fetchErr := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: batch.Discovery.Repository, Stage: "batch", Discovery: &batch.Discovery})
		if fetchErr != nil {
			var status *githubconnector.StatusError
			if !errors.Is(fetchErr, githubconnector.ErrNotFound) && !errors.Is(fetchErr, githubconnector.ErrRateLimited) && !(errors.As(fetchErr, &status) && (status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusTooManyRequests)) {
				return fetchErr
			}
			if (errors.Is(fetchErr, githubconnector.ErrNotFound) || status != nil && status.StatusCode == http.StatusNotFound) && scope.GitHubIntakeRevision.FallbackRunner != "" {
				if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
					return s.requireGitHubBatchRunner(ctx, reader, scope, scope.GitHubIntakeRevision.FallbackRunner, batch.Discovery.Repository, s.config.Clock())
				}); err != nil {
					return err
				}
				batch.HubOwned, batch.RunnerID = false, scope.GitHubIntakeRevision.FallbackRunner
				return save()
			}
			batch.Status = "discovery_failed"
			batch.Error, batch.RetryAt = githubBatchSourceFailure(fetchErr, s.config.Clock(), page.RetryAt)
			return errors.Join(fetchErr, save())
		}
		if len(page.Gaps) != 0 {
			batch.Status = "discovery_failed"
			partial := hubapi.NativePublicInvalid("Source discovery context is incomplete")
			batch.Error, batch.RetryAt = githubBatchSourceFailure(partial, s.config.Clock(), page.RetryAt)
			return errors.Join(partial, save())
		}
		if page.BatchPage == nil {
			return hubapi.NativePublicInvalid("Source discovery returned no page")
		}
		if batch.Automatic && page.BatchPage.Total > batchLimit(batch) {
			batch.Status, batch.Error, batch.Page.Total = "discovery_failed", "Repository exceeds the plan's per-job import limit; select a label filter or upgrade", page.BatchPage.Total
			return save()
		}
		if err := validateGitHubBatchPage(batch, *page.BatchPage); err != nil {
			return err
		}
		batch.Page.Issues = append(batch.Page.Issues, page.BatchPage.Issues...)
		batch.Page.Total, batch.Page.NextCursor = page.BatchPage.Total, page.BatchPage.NextCursor
		batch.Status = "preview"
		if batch.Automatic {
			for _, preview := range page.BatchPage.Issues {
				batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: preview.Number, Status: "pending"})
			}
			batch.Status = "importing"
		}
		if err := save(); err != nil {
			return err
		}
	}
	if batch.Status == "preview" {
		return nil
	}
	previews := githubBatchPreviewIndexes(batch.Page.Issues)
	processed := 0
	for i := range batch.Items {
		item := &batch.Items[i]
		if item.Status != "pending" {
			continue
		}
		if processed == githubBatchMaintenanceLimit {
			break
		}
		processed++
		index, found := previews[item.Number]
		if !found {
			return eventstore.ErrInvalidRecord
		}
		preview := batch.Page.Issues[index]
		itemErr := s.advanceGitHubBatchItem(ctx, scope, project, batch, item, preview)
		if itemErr != nil {
			var source *linkedSnapshotSourceError
			var invalid *hubapi.NativeError
			if ctx.Err() != nil || !errors.As(itemErr, &source) && !(errors.As(itemErr, &invalid) && invalid.Status == http.StatusUnprocessableEntity) {
				return itemErr
			}
			item.Status = "failed"
			item.Error, item.RetryAt = githubBatchSourceFailure(itemErr, s.config.Clock(), time.Time{})
			if item.RetryAt != "" {
				for j := range batch.Items {
					if batch.Items[j].Status == "pending" {
						batch.Items[j].Status, batch.Items[j].Error, batch.Items[j].RetryAt = "failed", "Source intake stopped at rate limit", item.RetryAt
					}
				}
			}
		} else {
			batch.Page.Issues[index].Body = ""
			batch.Page.Issues[index].Snapshot = nil
		}
	}
	finishGitHubBatchItems(batch)
	return save()
}

func (s *Service) prepareGitHubBatchItem(ctx context.Context, scope hubscope.NativeScope, project projectstore.Record, batch *tracker.GitHubBatch, item *tracker.GitHubBatchItem, preview tracker.GitHubIssuePreview) (*tracker.NativeIssue, error) {
	destination, labels, err := batchIssueDestination(project.States, batch, preview)
	if err != nil {
		return nil, err
	}
	if item.WorkItemID == "" {
		existing, owned, err := s.existingGitHubBatchItem(ctx, scope, batch, preview)
		if err == nil {
			item.WorkItemID = existing
			if !owned {
				item.Status = "skipped"
				return nil, nil
			}
		}
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return nil, err
		}
		if item.WorkItemID == "" {
			if preview.Snapshot != nil {
				scope.ScheduledRepair = s.observeImportedScheduledRepair(ctx, scope, *preview.Snapshot)
				if err := validateLinkedIssueSnapshot(*preview.Snapshot); err != nil {
					return nil, err
				}
			}
			raw, err := s.createNativeIssueCommand(ctx, scope, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: batch.ID + ":" + preview.ID}, GitHubIssueURL: preview.URL, State: destination, Labels: labels})
			if err != nil {
				return nil, err
			}
			var issue tracker.NativeIssue
			if err := json.Unmarshal(raw, &issue); err != nil {
				return nil, err
			}
			item.WorkItemID = issue.WorkItemID
		}
	}
	row, version, err := s.readWorkItemRecord(ctx, scope, string(item.WorkItemID))
	if err != nil {
		return nil, err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return nil, err
	}
	if issue.LinkedSource == nil {
		return nil, eventstore.ErrInvalidRecord
	}
	if issue.LinkedSource.Status == "complete" {
		item.Status, item.UnmappedProjectStatus = "completed", batchUnmappedProjectStatus(batch, preview)
		return nil, nil
	}
	transition := func(current tracker.NativeIssue) error {
		_, err := s.transitionNativeIssueTenant(ctx, scope, string(current.WorkItemID), tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("%s:lane:%d:%d", batch.ID, item.Number, batch.Revision)}, ExpectedRevision: current.Revision, State: destination, Reason: "user_requested"})
		return err
	}
	if issue.State != destination {
		if err := transition(issue); err != nil {
			return nil, err
		}
	}
	return &issue, nil
}

func (s *Service) advanceGitHubBatchItem(ctx context.Context, scope hubscope.NativeScope, project projectstore.Record, batch *tracker.GitHubBatch, item *tracker.GitHubBatchItem, preview tracker.GitHubIssuePreview) error {
	issue, err := s.prepareGitHubBatchItem(ctx, scope, project, batch, item, preview)
	if err != nil || issue == nil {
		return err
	}
	snapshot := preview.Snapshot
	if snapshot == nil || preview.NeedsDiscussion {
		snapshot, err = s.fetchLinkedSnapshot(ctx, scope, preview.URL)
		if err != nil {
			return err
		}
	}
	return s.completeGitHubBatchSnapshot(ctx, scope, batch, item, preview, *snapshot)
}

func (s *Service) completeGitHubBatchSnapshot(ctx context.Context, scope hubscope.NativeScope, batch *tracker.GitHubBatch, item *tracker.GitHubBatchItem, preview tracker.GitHubIssuePreview, snapshot tracker.GitHubIssueSnapshot) error {
	if len(snapshot.Comments) > 1000 {
		return hubapi.NativePublicInvalid("Linked issue discussion exceeds the intake bound")
	}
	if snapshot.Provenance.Provider != "github" || snapshot.Provenance.ExternalID != preview.ID {
		return hubapi.NativePublicInvalid("Snapshot stable source identity does not match preview")
	}
	scope.ScheduledRepair = s.observeImportedScheduledRepair(ctx, scope, snapshot)
	_, err := s.applyLinkedIssueRecord(ctx, scope, item.WorkItemID, "github.snapshot", tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: batch.ID + ":" + preview.ID + ":snapshot"}, Snapshot: snapshot})
	if err != nil {
		return err
	}
	item.Status, item.UnmappedProjectStatus = "completed", batchUnmappedProjectStatus(batch, preview)
	return nil
}

func (s *Service) existingGitHubBatchItem(ctx context.Context, scope hubscope.NativeScope, batch *tracker.GitHubBatch, preview tracker.GitHubIssuePreview) (tracker.NativeWorkItemID, bool, error) {
	var id tracker.NativeWorkItemID
	owned := false
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
			return err
		}
		find := func() error {
			source, err := reader.LinkedSourceByURL(ctx, scope.Project, preview.URL)
			if err == nil {
				id = source.WorkItemID
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			for _, key := range []string{"github:" + preview.URL, "github:" + preview.ID} {
				item, err := reader.WorkItemBySource(ctx, scope.Project, key)
				if err == nil {
					id = item.NativeID
					return nil
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			item, err := reader.WorkItemByProvenance(ctx, scope.Project, "github", preview.ID)
			if err == nil {
				id = item.NativeID
			}
			return err
		}
		if err := find(); err != nil {
			return err
		}
		operation := nativeIssueCreationOperation(scope)
		_, err := reader.Receipt(ctx, scope.Credential.ID, operation, batch.ID+":"+preview.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		owned = err == nil
		return err
	})
	return id, owned, err
}

func (s *Service) githubBatchRecordLimit(ctx context.Context, scope hubscope.NativeScope) (int, error) {
	if s.database.plansService().Plans() == nil {
		return 1000, nil
	}
	var entitlement hostedplans.HostedEntitlement
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		entitlement, err = hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, r, s.config.Clock()))
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

func (s *Service) closeGitHubBatchSources(ctx context.Context, scope hubscope.NativeScope, operation string, request tracker.GitHubBatchCommand, project projectstore.Record, batch *tracker.GitHubBatch, expected int64) (json.RawMessage, error) {
	if batch.Status != "finished" || s.config.OutboxBackend == nil || s.database.linkedSourceBase == "" {
		return nil, intakeInvalid("Finish intake and configure source publication before requesting source closure")
	}
	var organizationSlug string
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
			return err
		}
		organization, err := reader.AccessOrganization(ctx)
		organizationSlug = organization.Slug
		return err
	}); err != nil {
		return nil, err
	}
	previews := githubBatchPreviewIndexes(batch.Page.Issues)
	var outbound []projectstore.GitHubOutbound
	for index := range batch.Items {
		item := &batch.Items[index]
		if item.Status != "completed" || item.SourceCloseRequested {
			continue
		}
		previewIndex, found := previews[item.Number]
		if !found {
			return nil, eventstore.ErrInvalidRecord
		}
		preview := batch.Page.Issues[previewIndex]
		if preview.Closed {
			continue
		}
		row, _, err := s.readWorkItemRecord(ctx, scope, string(item.WorkItemID))
		if err != nil {
			return nil, err
		}
		url := strings.TrimRight(s.database.linkedSourceBase, "/") + hubrecords.IssueBrowserPath(organizationSlug, project.Core.Slug, int(row.Number))
		desired, err := json.Marshal(WorkpadDesired{Phase: "summary", Summary: true, Body: "Tracked in Detent: " + url, Marker: "<!-- detent-summary:" + string(item.WorkItemID) + ":intake -->", CloseSource: true, IssueNumber: item.Number})
		if err != nil {
			return nil, err
		}
		payload, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, desired)
		if err != nil {
			return nil, err
		}
		outbound = append(outbound, projectstore.GitHubOutbound{ProjectID: scope.Project, WorkItemID: item.WorkItemID, Key: "native:" + string(item.WorkItemID) + ":intake", Repository: batch.Discovery.Repository, IssueNumber: int64(item.Number), Kind: string(MutationWorkpad), CreatedAt: s.config.Clock(), Payload: payload})
		item.SourceCloseRequested = true
		if len(outbound) == githubBatchMaintenanceLimit {
			break
		}
	}
	batch.Revision++
	row, err := s.prepareGitHubBatchRecord(ctx, scope, batch)
	if err != nil {
		return nil, err
	}
	return s.tenantReceiptMutation(ctx, scope, operation, request.Mutation, request, projectGitHubBatchView(project.States, batch), func(writer tenantstore.Writer) error {
		if err := s.gitHubBatchRequestAuthority(ctx, writer, scope, scope.GitHubIntakeRevision.RequireAdminKey); err != nil {
			return err
		}
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, writer, "github_integration", s.config.Clock())); err != nil {
			return err
		}
		current, err := writer.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if current.Settings.Revision != project.Settings.Revision {
			return corestore.ErrRevisionConflict
		}
		owner, ok := writer.(projectstore.GitHubOutboxWriter)
		if !ok {
			return hubapi.ErrPostgresFeatureUnavailable
		}
		for _, item := range outbound {
			if _, err := writer.WorkItem(ctx, scope.Project, item.WorkItemID); err != nil {
				return err
			}
			source, err := writer.LinkedSource(ctx, scope.Project, item.WorkItemID)
			if err != nil {
				return err
			}
			canonical, repository, number, err := tracker.ParseGitHubIssueURL(source.URL)
			if err != nil || canonical != source.URL || repository != item.Repository || int64(number) != item.IssueNumber || source.SnapshotObjectKey == "" {
				return eventstore.ErrInvalidRecord
			}
			if err := owner.EnqueueGitHubOutbound(ctx, item); err != nil {
				return err
			}
		}
		batches, ok := writer.(projectstore.IntakeBatchWriter)
		if !ok {
			return hubapi.ErrPostgresFeatureUnavailable
		}
		return batches.SaveProjectIntakeBatch(ctx, row, expected)
	})
}

func (s *Service) gitHubBatchRequestAuthority(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, requireAdminKey bool) error {
	if requireAdminKey {
		return s.configurationRequestAuthority(ctx, reader, scope, s.config.Clock(), "github_intake")
	}
	if scope.Credential.Runner.RunnerID != "" {
		return hubapi.NativeNotFound()
	}
	if scope.Credential.Hosted != nil {
		if scope.Credential.HostedRole != "owner" && scope.Credential.HostedRole != "admin" {
			return hubapi.NativeNotFound()
		}
	} else if scope.Credential.Scope != hubapi.APIScopeAdmin {
		return hubapi.NativeNotFound()
	}
	scope.RequireHostedAdmin = false
	return s.authorizeWorkItemRecordScope(ctx, reader, scope)
}

func (s *Service) bindGitHubBatchIssuer(ctx context.Context, scope hubscope.NativeScope, record *githubBatchRecord) (hubscope.NativeScope, error) {
	if record.Issuer == nil || !record.Issuer.Valid() || record.Issuer.OrganizationID != string(scope.Organization) || record.RequestedBy == nil {
		return scope, &hubapi.NativeError{Code: "intake_authority_unavailable", Message: "Confirm the intake with current operator authority before continuing", Status: http.StatusConflict, PublicMessage: true}
	}
	authority, err := s.resolveOperatorChatAuthority(ctx, record.Issuer.Identity, record.Issuer.Origin)
	if err != nil {
		return scope, err
	}
	required := apikey.ScopeWrite
	if record.RequireAdminKey {
		required = apikey.ScopeAdmin
	}
	if err := authority.Check(ctx, operatortool.Requirement{Scope: required, OrganizationID: string(scope.Organization), ProjectID: string(scope.Project)}); err != nil {
		return scope, err
	}
	issuer, err := (hubProjectExecutor{s}).projectScope(authority.BindContext(ctx))
	if err != nil {
		return scope, err
	}
	issuer.Project = scope.Project
	issuer.RequireHostedAdmin = record.RequireAdminKey
	actor := issuer.Actor()
	if actor.Kind != record.RequestedBy.Kind || actor.PrincipalID != record.RequestedBy.PrincipalID {
		return scope, operatortool.ErrAccessDenied
	}
	scope.GitHubIntakeRevision = &hubscope.GitHubIntakeRevision{BatchID: record.ID, Revision: record.Revision, Issuer: record.Issuer, Authority: &issuer, FallbackRunner: record.FallbackRunnerID, RequireAdminKey: record.RequireAdminKey}
	scope.SourceActor = record.RequestedBy
	return scope, nil
}

func (s *Service) requireGitHubBatchRunner(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, runner, repository string, now time.Time) error {
	if runner == "" {
		return intakeInvalid("Select an active enrolled runner with a recent matching checkout report")
	}
	if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
		return err
	}
	identity, err := reader.RunnerIdentity(ctx, runner)
	if err != nil {
		return err
	}
	credential, err := reader.AccessCredential(ctx, identity.TokenID)
	if err != nil {
		return err
	}
	if identity.OrganizationID != scope.Organization || identity.State != "active" || identity.RemovedAt != nil || !now.Before(identity.ExpiresAt) || !slices.Contains(identity.Operations, "heartbeat") || credential.RevokedAt != nil || credential.ExpiresAt != nil && !now.Before(*credential.ExpiresAt) || credential.CreatedAt.After(now) || credential.Scope != "worker" || credential.Kind != "service" {
		return intakeInvalid("Select an active enrolled runner with a recent matching checkout report")
	}
	if scope.Credential.Runner.RunnerID != "" && (scope.Credential.Runner.RunnerID != runner || scope.Credential.ID != identity.TokenID || scope.Credential.Runner.MachineID != identity.MachineID) {
		return hubapi.NativeNotFound()
	}
	allowed, err := reader.RunnerProjectAuthorized(ctx, runner, scope.Project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return hubapi.NativeNotFound()
	}
	checkout, err := reader.RunnerCheckoutReport(ctx, runner, scope.Project)
	if err != nil {
		return err
	}
	if checkout.OrganizationID != scope.Organization || checkout.RunnerID != runner || checkout.ProjectID != scope.Project || !strings.EqualFold(checkout.Repository, repository) || !checkout.ReportedAt.After(now.Add(-5*time.Minute)) {
		return intakeInvalid("Select an active enrolled runner with a recent matching checkout report")
	}
	return nil
}

func githubBatchTask(scope hubscope.NativeScope, batch *tracker.GitHubBatch) (*tracker.GitHubBatchTask, error) {
	if batch.HubOwned {
		return nil, nil
	}
	if batch.Status == "discovering" {
		return &tracker.GitHubBatchTask{ProjectID: scope.Project, BatchID: batch.ID, Revision: batch.Revision, Discovery: &batch.Discovery}, nil
	}
	if batch.Status != "importing" {
		return nil, nil
	}
	indexes := githubBatchPreviewIndexes(batch.Page.Issues)
	var previews []tracker.GitHubIssuePreview
	for _, item := range batch.Items {
		if item.Status != "pending" {
			continue
		}
		index, ok := indexes[item.Number]
		if !ok {
			return nil, eventstore.ErrInvalidRecord
		}
		previews = append(previews, batch.Page.Issues[index])
		if len(previews) == githubBatchMaintenanceLimit {
			break
		}
	}
	if len(previews) == 0 {
		return nil, nil
	}
	return &tracker.GitHubBatchTask{ProjectID: scope.Project, BatchID: batch.ID, Revision: batch.Revision, Item: &previews[0], Items: previews}, nil
}

func (s *Service) readGitHubBatchTaskRecord(ctx context.Context, scope hubscope.NativeScope) (*tracker.GitHubBatchTask, error) {
	if scope.Project == "" {
		return nil, nil
	}
	record, err := s.loadGitHubBatchState(ctx, scope)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.HubOwned || record.RunnerID != scope.Credential.Runner.RunnerID || record.Status != "discovering" && record.Status != "importing" {
		return nil, nil
	}
	background, err := s.gitHubIntegrationScope(ctx, scope.Organization, scope.Project)
	if err != nil {
		return nil, err
	}
	background, err = s.bindGitHubBatchIssuer(ctx, background, record)
	if err != nil {
		var unavailable *hubapi.NativeError
		if errors.Is(err, operatortool.ErrAccessDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.As(err, &unavailable) && unavailable.Code == "intake_authority_unavailable" {
			return nil, nil
		}
		return nil, err
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.gitHubBatchRequestAuthority(ctx, reader, *background.GitHubIntakeRevision.Authority, background.GitHubIntakeRevision.RequireAdminKey); err != nil {
			return err
		}
		return s.requireGitHubBatchRunner(ctx, reader, scope, record.RunnerID, record.Discovery.Repository, s.config.Clock())
	}); err != nil {
		return nil, err
	}
	return githubBatchTask(scope, &record.GitHubBatch)
}

func finishGitHubBatchItems(batch *tracker.GitHubBatch) {
	if slices.ContainsFunc(batch.Items, func(item tracker.GitHubBatchItem) bool { return item.Status == "pending" }) {
		return
	}
	if batch.Automatic && batch.Page.NextCursor != "" && !slices.ContainsFunc(batch.Items, func(item tracker.GitHubBatchItem) bool { return item.Status == "failed" && item.RetryAt != "" }) {
		batch.Discovery.Cursor, batch.Status = batch.Page.NextCursor, "discovering"
	} else {
		batch.Status = "finished"
	}
}

func (s *Service) reconcileRunnerGitHubBatch(ctx context.Context, scope hubscope.NativeScope, batch *tracker.GitHubBatch) error {
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return err
	}
	if project.Settings.Intake != "manual" {
		return corestore.ErrNotFound
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Background, func(reader tenantstore.Reader) error {
		return s.requireGitHubBatchRunner(ctx, reader, scope, batch.RunnerID, batch.Discovery.Repository, s.config.Clock())
	}); err != nil {
		return err
	}
	changed := false
	if batch.Status == "importing" {
		indexes := githubBatchPreviewIndexes(batch.Page.Issues)
		processed := 0
		for index := range batch.Items {
			item := &batch.Items[index]
			if item.Status != "pending" {
				continue
			}
			if processed == githubBatchMaintenanceLimit {
				break
			}
			processed++
			previewIndex, ok := indexes[item.Number]
			if !ok {
				return eventstore.ErrInvalidRecord
			}
			original := *item
			_, err := s.prepareGitHubBatchItem(ctx, scope, project, batch, item, batch.Page.Issues[previewIndex])
			if err != nil {
				return err
			}
			changed = changed || original.WorkItemID != item.WorkItemID || original.Status != item.Status || hubapi.StringValue(original.UnmappedProjectStatus) != hubapi.StringValue(item.UnmappedProjectStatus)
		}
		previous := batch.Status
		finishGitHubBatchItems(batch)
		changed = changed || previous != batch.Status
	}
	if changed {
		return s.saveGitHubBatchRecord(ctx, scope, batch)
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Background, func(writer tenantstore.Writer) error {
		intake, ok := writer.(projectstore.GitHubIntakeWriter)
		if !ok {
			return errors.New("tenant writer does not support GitHub intake")
		}
		return intake.DeferGitHubIntake(ctx, scope.Project, s.config.Clock(), s.config.deps.Jitter())
	})
}

func (s *Service) reportGitHubBatchRecord(ctx context.Context, scope hubscope.NativeScope, operation string, request tracker.GitHubBatchResult) (json.RawMessage, error) {
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	record, err := s.loadGitHubBatchState(ctx, scope)
	if err != nil {
		return nil, err
	}
	batch := &record.GitHubBatch
	if batch.HubOwned || batch.RunnerID != scope.Credential.Runner.RunnerID || batch.ID != request.BatchID {
		return nil, hubapi.NativeNotFound()
	}
	if err := validateBatchDiagnostic(request.Error, request.RetryAt); err != nil {
		return nil, err
	}
	background, err := s.gitHubIntegrationScope(ctx, scope.Organization, scope.Project)
	if err != nil {
		return nil, err
	}
	background, err = s.bindGitHubBatchIssuer(ctx, background, record)
	if err != nil {
		return nil, err
	}
	background.GitHubIntakeRevision.Reporter = &scope
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return s.requireGitHubBatchRunner(ctx, reader, scope, batch.RunnerID, batch.Discovery.Repository, s.config.Clock())
	}); err != nil {
		return nil, err
	}
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	if batch.Revision != request.Revision {
		return nil, hubapi.NativeConflict(tracker.Revision(batch.Revision))
	}
	project, err := s.readProjectRecord(ctx, background)
	if err != nil {
		return nil, err
	}
	if project.Settings.Intake != "manual" {
		return nil, intakeInvalid("Manual intake is disabled")
	}
	expected := batch.Revision
	switch batch.Status {
	case "discovering":
		if request.Error != "" {
			batch.Status, batch.Error, batch.RetryAt = "discovery_failed", request.Error, request.RetryAt
		} else {
			if request.Page == nil {
				return nil, hubapi.NativePublicInvalid("Invalid source discovery page")
			}
			if batch.Automatic && request.Page.Total > batchLimit(batch) {
				batch.Status, batch.Error, batch.Page.Total = "discovery_failed", "Repository exceeds the plan's per-job import limit; select a label filter or upgrade", request.Page.Total
			} else {
				if err := validateGitHubBatchPage(batch, *request.Page); err != nil {
					return nil, err
				}
				batch.Page.Issues = append(batch.Page.Issues, request.Page.Issues...)
				batch.Page.Total, batch.Page.NextCursor = request.Page.Total, request.Page.NextCursor
				batch.Status = "preview"
				if batch.Automatic {
					for _, preview := range request.Page.Issues {
						batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: preview.Number, Status: "pending"})
					}
					batch.Status = "importing"
				}
			}
		}
	case "importing":
		results := request.Results
		if len(results) == 0 {
			if request.Number == 0 && request.Error != "" {
				for _, item := range batch.Items {
					if item.Status == "pending" {
						results = append(results, tracker.GitHubBatchItemResult{Number: item.Number, Error: request.Error, RetryAt: request.RetryAt})
					}
					if len(results) == githubBatchMaintenanceLimit {
						break
					}
				}
			} else {
				results = []tracker.GitHubBatchItemResult{{Number: request.Number, Snapshot: request.Snapshot, Error: request.Error, RetryAt: request.RetryAt}}
			}
		}
		if len(results) == 0 || len(results) > githubBatchMaintenanceLimit {
			return nil, hubapi.NativePublicInvalid("At most 100 intake results per page are required")
		}
		selected := make(map[int]int, len(batch.Items))
		for index, item := range batch.Items {
			if item.Status == "pending" {
				if _, exists := selected[item.Number]; !exists {
					selected[item.Number] = index
				}
			}
		}
		previews := githubBatchPreviewIndexes(batch.Page.Issues)
		seen := make(map[int]bool, len(results))
		for _, result := range results {
			index, ok := selected[result.Number]
			if !ok || seen[result.Number] {
				return nil, hubapi.NativePublicInvalid("No pending selected issue")
			}
			seen[result.Number] = true
			if err := validateBatchDiagnostic(result.Error, result.RetryAt); err != nil {
				return nil, err
			}
			item := &batch.Items[index]
			if result.Error != "" {
				item.Status, item.Error, item.RetryAt = "failed", result.Error, result.RetryAt
			} else {
				previewIndex, ok := previews[item.Number]
				if !ok {
					return nil, eventstore.ErrInvalidRecord
				}
				preview := batch.Page.Issues[previewIndex]
				if result.Snapshot == nil || result.Snapshot.Provenance.Provider != "github" || result.Snapshot.Provenance.ExternalID != preview.ID {
					return nil, hubapi.NativePublicInvalid("Snapshot stable source identity does not match preview")
				}
				issue, itemErr := s.prepareGitHubBatchItem(ctx, background, project, batch, item, preview)
				if itemErr == nil && issue != nil {
					itemErr = s.completeGitHubBatchSnapshot(ctx, background, batch, item, preview, *result.Snapshot)
				}
				if itemErr != nil {
					var invalid *hubapi.NativeError
					if !errors.As(itemErr, &invalid) || invalid.Status != http.StatusUnprocessableEntity {
						return nil, itemErr
					}
					item.Status = "failed"
					item.Error, item.RetryAt = githubBatchSourceFailure(itemErr, s.config.Clock(), time.Time{})
				} else {
					batch.Page.Issues[previewIndex].Body = ""
					batch.Page.Issues[previewIndex].Snapshot = nil
				}
			}
			if result.RetryAt != "" {
				for index := range batch.Items {
					if batch.Items[index].Status == "pending" && !seen[batch.Items[index].Number] {
						batch.Items[index].Status, batch.Items[index].Error, batch.Items[index].RetryAt = "failed", "Source intake stopped at rate limit", result.RetryAt
					}
				}
			}
		}
		finishGitHubBatchItems(batch)
	default:
		return nil, hubapi.NativePublicInvalid("Intake has no outstanding source request")
	}
	batch.Revision++
	row, err := s.prepareGitHubBatchRecord(ctx, background, batch)
	if err != nil {
		return nil, err
	}
	next, err := githubBatchTask(scope, batch)
	if err != nil {
		return nil, err
	}
	receipt := tracker.GitHubBatchReceipt{Revision: batch.Revision, Status: batch.Status, Next: next}
	return s.tenantReceiptMutation(ctx, scope, operation, request.Mutation, request, receipt, func(writer tenantstore.Writer) error {
		if err := s.requireGitHubBatchRunner(ctx, writer, scope, batch.RunnerID, batch.Discovery.Repository, s.config.Clock()); err != nil {
			return err
		}
		if err := s.gitHubBatchRequestAuthority(ctx, writer, *background.GitHubIntakeRevision.Authority, background.GitHubIntakeRevision.RequireAdminKey); err != nil {
			return err
		}
		if err := writer.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		current, err := writer.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if current.Settings.Revision != project.Settings.Revision {
			return corestore.ErrRevisionConflict
		}
		batches, ok := writer.(projectstore.IntakeBatchWriter)
		if !ok {
			return errors.New("tenant writer does not support intake batches")
		}
		return batches.SaveProjectIntakeBatch(ctx, row, expected)
	})
}
