package hubserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	defaultAPIPageLimit = 50
	maxAPIPageLimit     = 200
)

type workItemListResponse struct {
	Items      []tracker.WorkItem `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type workItemListQuery struct {
	Repositories  map[string]struct{}
	WorkflowState map[string]struct{}
	Readiness     map[string]struct{}
	Priorities    map[string]struct{}
	Labels        map[string]struct{}
	Assignees     map[string]struct{}
	Machines      map[string]struct{}
	Leases        map[string]struct{}
	PullRequests  map[string]struct{}
	SyncHealth    map[string]struct{}
	Sort          string
	Order         string
	Limit         int
	Cursor        workItemCursor
}

type workItemCursor struct {
	Version int      `json:"v"`
	Sort    string   `json:"sort"`
	Order   string   `json:"order"`
	Values  []string `json:"values"`
}

type workpadProjection struct {
	Phase     string    `json:"phase"`
	Body      string    `json:"body"`
	SyncState string    `json:"sync_state"`
	UpdatedAt time.Time `json:"updated_at"`
}

type timelineEvent struct {
	ID int64 `json:"id"`
	tracker.WorkEvent
	RecordedAt time.Time `json:"recorded_at"`
}

type workItemDetailResponse struct {
	tracker.WorkItem
	Body               string             `json:"body"`
	Workpad            *workpadProjection `json:"workpad,omitempty"`
	Timeline           []timelineEvent    `json:"timeline"`
	TimelineNextCursor string             `json:"timeline_next_cursor,omitempty"`
}

type timelineCursor struct {
	Version int   `json:"v"`
	ID      int64 `json:"id"`
}

func (s *Service) listWorkItems(c echo.Context) error {
	query, err := parseWorkItemListQuery(c)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_query", Message: err.Error()})
	}
	items, err := s.allWorkItems(c.Request().Context())
	if err != nil {
		return s.internalAPIError(c, "work_items_unavailable", "Work items could not be read", err)
	}
	items = filterWorkItems(items, query)
	sortWorkItems(items, query.Sort, query.Order)
	items = workItemsAfterCursor(items, query)
	response := workItemListResponse{Items: items}
	if len(response.Items) > query.Limit {
		response.Items = response.Items[:query.Limit]
		response.NextCursor, err = encodeWorkItemCursor(workItemCursor{
			Version: 1,
			Sort:    query.Sort,
			Order:   query.Order,
			Values:  workItemSortValues(response.Items[len(response.Items)-1], query.Sort),
		})
		if err != nil {
			return s.internalAPIError(c, "work_items_unavailable", "Work items could not be read", err)
		}
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Service) getWorkItem(c echo.Context) error {
	id, err := apiWorkItemID(c)
	if err != nil {
		return c.JSON(http.StatusNotFound, apiErrorResponse{Code: "work_item_not_found", Message: "Work item was not found"})
	}
	items, err := s.tracker.GetWorkItems(c.Request().Context(), []tracker.WorkItemID{id})
	if err != nil {
		return s.internalAPIError(c, "work_item_unavailable", "Work item could not be read", err)
	}
	if len(items) != 1 {
		return c.JSON(http.StatusNotFound, apiErrorResponse{Code: "work_item_not_found", Message: "Work item was not found"})
	}
	if err := s.applyRepositorySyncHealth(c.Request().Context(), items); err != nil {
		return s.internalAPIError(c, "work_item_unavailable", "Work item could not be read", err)
	}
	body, err := s.database.workItemBody(c.Request().Context(), id)
	if err != nil {
		return s.internalAPIError(c, "work_item_unavailable", "Work item could not be read", err)
	}
	timelineLimit, err := parsePageLimit(c.QueryParam("timeline_limit"))
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_query", Message: "timeline_limit must be between 1 and 200"})
	}
	cursor, err := decodeTimelineCursor(c.QueryParam("timeline_cursor"))
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_cursor", Message: "Timeline cursor is invalid"})
	}
	workpad, hasWorkpad, err := s.database.latestWorkpad(c.Request().Context(), id)
	if err != nil {
		return s.internalAPIError(c, "work_item_unavailable", "Work item could not be read", err)
	}
	timeline, err := s.database.workItemTimeline(c.Request().Context(), id, cursor.ID, timelineLimit+1)
	if err != nil {
		return s.internalAPIError(c, "work_item_unavailable", "Work item could not be read", err)
	}
	response := workItemDetailResponse{WorkItem: items[0], Body: body, Timeline: timeline}
	if hasWorkpad {
		response.Workpad = &workpad
	}
	if len(response.Timeline) > timelineLimit {
		response.Timeline = response.Timeline[:timelineLimit]
		response.TimelineNextCursor, err = encodeTimelineCursor(timelineCursor{Version: 1, ID: response.Timeline[len(response.Timeline)-1].ID})
		if err != nil {
			return s.internalAPIError(c, "work_item_unavailable", "Work item could not be read", err)
		}
	}
	return c.JSON(http.StatusOK, response)
}

func (d *database) workItemBody(ctx context.Context, id tracker.WorkItemID) (string, error) {
	var body sql.NullString
	if err := d.db.QueryRowContext(ctx, "SELECT body FROM issues WHERE id = ?", id).Scan(&body); err != nil {
		return "", fmt.Errorf("read work item body: %w", err)
	}
	return body.String, nil
}

func (s *Service) allWorkItems(ctx context.Context) ([]tracker.WorkItem, error) {
	ids, err := s.database.allWorkItemIDs(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.tracker.GetWorkItems(ctx, ids)
	if err != nil {
		return nil, err
	}
	if err := s.applyRepositorySyncHealth(ctx, items); err != nil {
		return nil, err
	}
	return items, nil
}

func (d *database) allWorkItemIDs(ctx context.Context) ([]tracker.WorkItemID, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT id FROM issues ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query hub work item IDs: %w", err)
	}
	defer rows.Close()
	var ids []tracker.WorkItemID
	for rows.Next() {
		var id tracker.WorkItemID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan hub work item ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hub work item IDs: %w", err)
	}
	return ids, nil
}

func (s *Service) applyRepositorySyncHealth(ctx context.Context, items []tracker.WorkItem) error {
	if len(items) == 0 {
		return nil
	}
	result, err := s.database.repositoryFreshness(ctx, s.config.now().UTC(), s.config.ReconcileInterval)
	if err != nil {
		return err
	}
	statuses := make(map[tracker.RepositoryID]string, len(result.Repositories))
	for _, repository := range result.Repositories {
		statuses[tracker.RepositoryID(repository.ID)] = repository.Status
	}
	for i := range items {
		if items[i].SyncStatus != tracker.SyncStatusSynced {
			continue
		}
		switch statuses[items[i].Repository.ID] {
		case "error":
			items[i].SyncStatus = tracker.SyncStatusError
			appendSyncDispatchReason(&items[i], tracker.DispatchReasonSyncError, "GitHub synchronization has an error")
		case "stale":
			items[i].SyncStatus = tracker.SyncStatusStale
			appendSyncDispatchReason(&items[i], tracker.DispatchReasonSyncStale, "GitHub projection is stale")
		}
	}
	return nil
}

func appendSyncDispatchReason(item *tracker.WorkItem, code tracker.DispatchReasonCode, message string) {
	item.Dispatchability.Dispatchable = false
	for _, reason := range item.Dispatchability.Reasons {
		if reason.Code == code {
			return
		}
	}
	item.Dispatchability.Reasons = append(item.Dispatchability.Reasons, tracker.DispatchReason{Code: code, Message: message})
}

func parseWorkItemListQuery(c echo.Context) (workItemListQuery, error) {
	query := workItemListQuery{
		Repositories:  querySet(c, "repository"),
		WorkflowState: querySet(c, "workflow_state"),
		Readiness:     querySet(c, "readiness"),
		Priorities:    querySet(c, "priority"),
		Labels:        querySet(c, "label"),
		Assignees:     querySet(c, "assignee"),
		Machines:      querySet(c, "machine"),
		Leases:        querySet(c, "lease"),
		PullRequests:  querySet(c, "pr"),
		SyncHealth:    querySet(c, "sync_health"),
		Sort:          strings.ToLower(strings.TrimSpace(c.QueryParam("sort"))),
		Order:         strings.ToLower(strings.TrimSpace(c.QueryParam("order"))),
	}
	if query.Sort == "" {
		query.Sort = "priority"
	}
	if query.Order == "" {
		query.Order = "asc"
	}
	validSort := map[string]bool{"priority": true, "created": true, "updated": true, "identifier": true, "workflow_state": true}
	if !validSort[query.Sort] {
		return workItemListQuery{}, errors.New("sort must be priority, created, updated, identifier, or workflow_state")
	}
	if query.Order != "asc" && query.Order != "desc" {
		return workItemListQuery{}, errors.New("order must be asc or desc")
	}
	var err error
	query.Limit, err = parsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return workItemListQuery{}, errors.New("limit must be between 1 and 200")
	}
	query.Cursor, err = decodeWorkItemCursor(c.QueryParam("cursor"))
	if err != nil {
		return workItemListQuery{}, errors.New("cursor is invalid")
	}
	if query.Cursor.Version != 0 && (query.Cursor.Sort != query.Sort || query.Cursor.Order != query.Order) {
		return workItemListQuery{}, errors.New("cursor does not match sort and order")
	}
	validFilters := []struct {
		values map[string]struct{}
		valid  map[string]bool
		name   string
	}{
		{query.Readiness, map[string]bool{"ready": true, "waiting": true}, "readiness"},
		{query.Priorities, map[string]bool{"urgent": true, "high": true, "normal": true, "low": true, "unset": true}, "priority"},
		{query.Leases, map[string]bool{"active": true, "unclaimed": true}, "lease"},
		{query.PullRequests, map[string]bool{"linked": true, "unlinked": true, "open": true, "closed": true, "merged": true}, "pr"},
		{query.SyncHealth, map[string]bool{"synced": true, "pending": true, "retrying": true, "error": true, "stale": true}, "sync_health"},
	}
	for _, filter := range validFilters {
		for value := range filter.values {
			if !filter.valid[value] {
				return workItemListQuery{}, fmt.Errorf("%s filter value %q is invalid", filter.name, value)
			}
		}
	}
	return query, nil
}

func parsePageLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultAPIPageLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxAPIPageLimit {
		return 0, errors.New("page limit is invalid")
	}
	return limit, nil
}

func querySet(c echo.Context, name string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, value := range c.QueryParams()[name] {
		for _, part := range strings.Split(value, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part != "" {
				result[part] = struct{}{}
			}
		}
	}
	return result
}

func filterWorkItems(items []tracker.WorkItem, query workItemListQuery) []tracker.WorkItem {
	result := make([]tracker.WorkItem, 0, len(items))
	for _, item := range items {
		if !workItemMatches(item, query) {
			continue
		}
		result = append(result, item)
	}
	return result
}

func workItemMatches(item tracker.WorkItem, query workItemListQuery) bool {
	repository := strings.ToLower(item.Repository.Owner + "/" + item.Repository.Name)
	if !matchesAny(query.Repositories, repository, strconv.FormatInt(int64(item.Repository.ID), 10)) {
		return false
	}
	workflow := ""
	if item.WorkflowState != nil {
		workflow = strings.ToLower(item.WorkflowState.Name)
	}
	workflowSource := ""
	if item.WorkflowState != nil {
		workflowSource = strings.ToLower(item.WorkflowState.SourceName)
	}
	if !matchesAny(query.WorkflowState, workflow, workflowSource) {
		return false
	}
	readiness := "waiting"
	if item.Dispatchability.Dispatchable {
		readiness = "ready"
	}
	if !matchesAny(query.Readiness, readiness) || !matchesAny(query.Priorities, workItemPriority(item)) {
		return false
	}
	if !matchesCollection(query.Labels, item.Labels) || !matchesCollection(query.Assignees, item.Assignees) {
		return false
	}
	machine := ""
	lease := "unclaimed"
	if item.ActiveLease != nil {
		machine = strings.ToLower(string(item.ActiveLease.Machine.ID))
		lease = "active"
	}
	machineHostname := ""
	machineDisplayName := ""
	if item.ActiveLease != nil {
		machineHostname = strings.ToLower(item.ActiveLease.Machine.Hostname)
		machineDisplayName = strings.ToLower(item.ActiveLease.Machine.DisplayName)
	}
	if !matchesAny(query.Machines, machine, machineHostname, machineDisplayName) || !matchesAny(query.Leases, lease) {
		return false
	}
	if !matchesPullRequest(query.PullRequests, item.PullRequests) {
		return false
	}
	return matchesAny(query.SyncHealth, strings.ToLower(string(item.SyncStatus)))
}

func matchesAny(filter map[string]struct{}, values ...string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, value := range values {
		if _, ok := filter[strings.ToLower(strings.TrimSpace(value))]; ok {
			return true
		}
	}
	return false
}

func matchesCollection(filter map[string]struct{}, values []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, value := range values {
		if _, ok := filter[strings.ToLower(strings.TrimSpace(value))]; ok {
			return true
		}
	}
	return false
}

func matchesPullRequest(filter map[string]struct{}, pullRequests []tracker.PullRequestSummary) bool {
	if len(filter) == 0 {
		return true
	}
	if len(pullRequests) == 0 {
		_, ok := filter["unlinked"]
		return ok
	}
	if _, ok := filter["linked"]; ok {
		return true
	}
	for _, pullRequest := range pullRequests {
		state := strings.ToLower(strings.TrimSpace(pullRequest.State))
		if _, ok := filter[state]; ok {
			return true
		}
	}
	return false
}

func workItemPriority(item tracker.WorkItem) string {
	if item.Queue == nil || item.Queue.PriorityRank == nil {
		return "unset"
	}
	switch *item.Queue.PriorityRank {
	case tracker.QueuePriorityUrgent:
		return "urgent"
	case tracker.QueuePriorityHigh:
		return "high"
	case tracker.QueuePriorityNormal:
		return "normal"
	case tracker.QueuePriorityLow:
		return "low"
	default:
		return "unset"
	}
}

func sortWorkItems(items []tracker.WorkItem, sortName string, order string) {
	sort.SliceStable(items, func(i int, j int) bool {
		comparison := compareStringSlices(workItemSortValues(items[i], sortName), workItemSortValues(items[j], sortName))
		if order == "desc" {
			return comparison > 0
		}
		return comparison < 0
	})
}

func workItemSortValues(item tracker.WorkItem, sortName string) []string {
	identifier := []string{
		strings.ToLower(item.Repository.Owner),
		strings.ToLower(item.Repository.Name),
		fmt.Sprintf("%020d", item.GitHub.Number),
		fmt.Sprintf("%020d", item.ID),
	}
	switch sortName {
	case "created":
		return append([]string{sortableTime(item.CreatedAt)}, identifier...)
	case "updated":
		return append([]string{sortableTime(item.UpdatedAt)}, identifier...)
	case "identifier":
		return identifier
	case "workflow_state":
		workflow := ""
		if item.WorkflowState != nil {
			workflow = strings.ToLower(item.WorkflowState.Name)
		}
		return append([]string{workflow}, identifier...)
	default:
		priority := 4
		rankMissing := 1
		rank := ""
		if item.Queue != nil {
			rank = strings.TrimSpace(item.Queue.Rank)
			if rank != "" {
				rankMissing = 0
			}
			if item.Queue.PriorityRank != nil && *item.Queue.PriorityRank >= tracker.QueuePriorityUrgent && *item.Queue.PriorityRank <= tracker.QueuePriorityLow {
				priority = *item.Queue.PriorityRank
			}
		}
		created := sortableTime(item.CreatedAt)
		return append([]string{strconv.Itoa(priority), strconv.Itoa(rankMissing), rank, created}, identifier...)
	}
}

func sortableTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func compareStringSlices(left []string, right []string) int {
	length := len(left)
	if len(right) < length {
		length = len(right)
	}
	for index := range length {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return 0
}

func workItemsAfterCursor(items []tracker.WorkItem, query workItemListQuery) []tracker.WorkItem {
	if query.Cursor.Version == 0 {
		return items
	}
	for index, item := range items {
		comparison := compareStringSlices(workItemSortValues(item, query.Sort), query.Cursor.Values)
		if query.Order == "desc" {
			comparison = -comparison
		}
		if comparison > 0 {
			return items[index:]
		}
	}
	return []tracker.WorkItem{}
}

func encodeWorkItemCursor(cursor workItemCursor) (string, error) {
	value, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func decodeWorkItemCursor(value string) (workItemCursor, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return workItemCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return workItemCursor{}, err
	}
	var cursor workItemCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Version != 1 || cursor.Sort == "" || cursor.Order == "" || len(cursor.Values) == 0 {
		return workItemCursor{}, errors.New("invalid work item cursor")
	}
	return cursor, nil
}

func encodeTimelineCursor(cursor timelineCursor) (string, error) {
	value, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func decodeTimelineCursor(value string) (timelineCursor, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return timelineCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return timelineCursor{}, err
	}
	var cursor timelineCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Version != 1 || cursor.ID <= 0 {
		return timelineCursor{}, errors.New("invalid timeline cursor")
	}
	return cursor, nil
}

func (d *database) latestWorkpad(ctx context.Context, id tracker.WorkItemID) (workpadProjection, bool, error) {
	var desiredJSON string
	var status string
	var updatedAt string
	err := d.db.QueryRowContext(ctx, `
SELECT desired_json, status, updated_at
FROM github_outbox
WHERE issue_id = ? AND mutation_kind = ?
ORDER BY id DESC
LIMIT 1`, id, MutationWorkpad).Scan(&desiredJSON, &status, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return workpadProjection{}, false, nil
	}
	if err != nil {
		return workpadProjection{}, false, fmt.Errorf("query hub workpad: %w", err)
	}
	var desired WorkpadDesired
	if err := json.Unmarshal([]byte(desiredJSON), &desired); err != nil {
		return workpadProjection{}, false, fmt.Errorf("decode hub workpad: %w", err)
	}
	parsed, err := parseTimeValue(updatedAt)
	if err != nil {
		return workpadProjection{}, false, fmt.Errorf("decode hub workpad timestamp: %w", err)
	}
	return workpadProjection{Phase: desired.Phase, Body: desired.Body, SyncState: status, UpdatedAt: parsed}, true, nil
}

func (d *database) workItemTimeline(ctx context.Context, id tracker.WorkItemID, afterID int64, limit int) ([]timelineEvent, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT id, fencing_token, machine_id, session_id, run_id, kind, payload_json, occurred_at, recorded_at
FROM work_events
WHERE issue_id = ? AND id > ?
ORDER BY id
LIMIT ?`, id, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("query hub work item timeline: %w", err)
	}
	defer rows.Close()
	events := make([]timelineEvent, 0)
	for rows.Next() {
		var event timelineEvent
		var fencingToken sql.NullInt64
		var machineID sql.NullString
		var sessionID sql.NullString
		var runID sql.NullString
		var payloadJSON string
		var occurredAt string
		var recordedAt string
		if err := rows.Scan(&event.ID, &fencingToken, &machineID, &sessionID, &runID, &event.Kind, &payloadJSON, &occurredAt, &recordedAt); err != nil {
			return nil, fmt.Errorf("scan hub work item timeline: %w", err)
		}
		event.WorkItemID = id
		if fencingToken.Valid {
			event.FencingToken = tracker.FencingToken(fencingToken.Int64)
		}
		if machineID.Valid {
			event.MachineID = tracker.MachineID(machineID.String)
		}
		event.SessionID = sessionID.String
		event.RunID = runID.String
		if err := json.Unmarshal([]byte(payloadJSON), &event.Payload); err != nil {
			return nil, fmt.Errorf("decode hub timeline event payload: %w", err)
		}
		var err error
		event.OccurredAt, err = parseTimeValue(occurredAt)
		if err != nil {
			return nil, fmt.Errorf("decode hub timeline event occurrence: %w", err)
		}
		event.RecordedAt, err = parseTimeValue(recordedAt)
		if err != nil {
			return nil, fmt.Errorf("decode hub timeline event recording: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hub work item timeline: %w", err)
	}
	return events, nil
}
