package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type githubBatchView struct {
	Progress githubBatchProgress   `json:"progress"`
	Batch    *tracker.GitHubBatch  `json:"batch"`
	Lanes    []tracker.NativeState `json:"lanes"`
}

type githubBatchProgress struct {
	Discovered int `json:"discovered"`
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	Skipped    int `json:"skipped"`
}

func projectGitHubBatchView(states []tracker.NativeState, batch *tracker.GitHubBatch) githubBatchView {
	progress := githubBatchProgress{}
	if batch != nil {
		progress.Discovered, progress.Total = len(batch.Page.Issues), batch.Page.Total
		for _, item := range batch.Items {
			switch item.Status {
			case "pending":
				progress.Pending++
			case "completed":
				progress.Completed++
			case "failed":
				progress.Failed++
			case "skipped":
				progress.Skipped++
			}
		}
	}
	if batch != nil {
		copy := *batch
		copy.Page.Issues = slices.Clone(batch.Page.Issues)
		for i := range copy.Page.Issues {
			copy.Page.Issues[i].Body = ""
			copy.Page.Issues[i].Snapshot = nil
		}
		batch = &copy
	}
	return githubBatchView{Batch: batch, Lanes: states, Progress: progress}
}

func (s *Service) getGitHubBatch(c echo.Context) error {
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	view, err := s.projectGitHubBatch(ctx, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) projectGitHubBatch(ctx context.Context, scope nativeScope) (githubBatchView, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return githubBatchView{}, ErrHostedDatabaseBinding
	}
	var project projectstore.Record
	var record projectstore.IntakeBatch
	found := false
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		project, err = r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		reader, ok := r.(projectstore.IntakeBatchReader)
		if !ok {
			return ErrHostedDatabaseBinding
		}
		record, err = reader.ProjectIntakeBatch(ctx, scope.project)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return githubBatchView{}, hubapi.ProjectRecordError(err)
	}
	if !found {
		return projectGitHubBatchView(project.States, nil), nil
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, record.Payload)
	if err != nil {
		return githubBatchView{}, err
	}
	var batch tracker.GitHubBatch
	if json.Unmarshal(raw, &batch) != nil || record.ProjectID != scope.project || record.Payload.OrganizationID != scope.organization || batch.ID != record.ID || batch.Revision != record.Revision || batch.Status != record.Status || batch.HubOwned != record.HubOwned {
		return githubBatchView{}, eventstore.ErrInvalidRecord
	}
	return projectGitHubBatchView(project.States, &batch), nil
}

func intakeInvalid(message string) error {
	return &hubapi.NativeError{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity, PublicMessage: true}
}

func batchHasPending(batch *tracker.GitHubBatch) bool {
	if batch.Status == "discovering" {
		return true
	}
	return slices.ContainsFunc(batch.Items, func(item tracker.GitHubBatchItem) bool { return item.Status == "pending" })
}

func (s *Service) commandGitHubBatch(c echo.Context) error {
	var request tracker.GitHubBatchCommand
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, nil)
}

func findBatchLane(states []tracker.NativeState, name string) (tracker.NativeState, bool) {
	for _, state := range states {
		if state.Name == name {
			return state, true
		}
	}
	return tracker.NativeState{}, false
}
func batchRetryAllowed(raw string, now time.Time) error {
	if raw == "" {
		return nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return hubapi.NativePublicInvalid("Invalid source retry deadline")
	}
	if now.Before(deadline) {
		return hubapi.NativeInvalid("Source rate limit has not reset; retry after " + raw)
	}
	return nil
}

func batchLimit(batch *tracker.GitHubBatch) int {
	if batch.Limit > 0 {
		return batch.Limit
	}
	return 1000
}

func validateBatchProjectSource(states []tracker.NativeState, source *tracker.GitHubProjectV2Source) error {
	if source == nil {
		return nil
	}
	if strings.TrimSpace(source.ID) == "" || strings.TrimSpace(source.ID) != source.ID || len(source.ID) > 256 || len(source.StatusLanes) == 0 || len(source.StatusLanes) > 100 {
		return intakeInvalid("Provide a ProjectV2 node ID and 1–100 Status lane mappings")
	}
	for status, name := range source.StatusLanes {
		if _, exists := findBatchLane(states, name); !exists || strings.TrimSpace(status) == "" || len(status) > 100 {
			return intakeInvalid("Map nonempty ProjectV2 Status values to configured native lanes")
		}
	}
	return nil
}

func batchUnmappedProjectStatus(batch *tracker.GitHubBatch, preview tracker.GitHubIssuePreview) *string {
	if source := batch.Discovery.ProjectV2; source != nil {
		if _, mapped := source.StatusLanes[preview.ProjectStatus]; !mapped {
			return &preview.ProjectStatus
		}
	}
	return nil
}

func batchIssueDestination(states []tracker.NativeState, batch *tracker.GitHubBatch, preview tracker.GitHubIssuePreview) (string, []string, error) {
	destination := batch.Destination
	labels := []string{}
	mapped := ""
	conflict := false
	for _, label := range preview.Labels {
		name, explicit := batch.LabelLanes[label]
		if batch.IgnoreLanes {
			name = ""
		}
		laneLabel := explicit && strings.HasPrefix(strings.ToLower(label), "detent:")
		if strings.HasPrefix(strings.ToLower(label), "detent:") {
			for _, state := range states {
				if strings.EqualFold(label, "detent:"+state.Name) || strings.EqualFold(label, "detent:"+strings.ReplaceAll(state.Name, " ", "-")) {
					laneLabel = true
					if !explicit && !batch.IgnoreLanes && (!state.Dispatchable || batch.AllowDispatch) {
						name = state.Name
					}
					break
				}
			}
		}
		if name != "" {
			if mapped != "" && mapped != name {
				conflict = true
			}
			mapped = name
		}
		if !laneLabel {
			labels = append(labels, label)
		}
	}
	if mapped != "" && !conflict {
		destination = mapped
	}
	if source := batch.Discovery.ProjectV2; source != nil {
		destination = batch.Destination
		if name, mapped := source.StatusLanes[preview.ProjectStatus]; mapped {
			destination = name
		}
	}
	lane, exists := findBatchLane(states, destination)
	if !exists || lane.Dispatchable && !batch.AllowDispatch {
		return "", nil, intakeInvalid("Select a destination and explicitly confirm any dispatchable lane")
	}
	return destination, labels, nil
}

func (s *Service) reportGitHubBatch(c echo.Context) error {
	var request tracker.GitHubBatchResult
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	raw, err := s.reportGitHubBatchRecord(c.Request().Context(), scope, nativeOperation(scope, http.MethodPost, "/onboarding/issue-intake/result"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func validateBatchDiagnostic(message, retry string) error {
	if len(message) > 1000 {
		return hubapi.NativePublicInvalid("Source diagnostic exceeds1000 bytes")
	}
	if retry != "" {
		if _, err := time.Parse(time.RFC3339Nano, retry); err != nil {
			return hubapi.NativePublicInvalid("Invalid source retry deadline")
		}
	}
	return nil
}
