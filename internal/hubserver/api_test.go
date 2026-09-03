package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAPITokensAreHashedScopedRotatableAndRedacted(t *testing.T) {
	t.Parallel()

	const (
		adminToken   = "hub-admin-token-that-is-never-persisted-in-plaintext"
		workerToken  = "hub-worker-token-that-is-never-persisted-in-plaintext"
		rotatedToken = "hub-rotated-token-that-is-never-persisted-in-plaintext"
	)
	generated := []string{workerToken, rotatedToken}
	var generatedIndex int
	var logs bytes.Buffer
	databasePath := filepath.Join(t.TempDir(), "hub.db")
	service, err := Open(t.Context(), Config{
		DatabasePath:      databasePath,
		InitialAdminToken: []byte(adminToken),
		Logger:            slog.New(slog.NewJSONHandler(&logs, nil)),
		newTokenID:        func() string { return "worker-token-id" },
		generateToken: func() (string, error) {
			token := generated[generatedIndex]
			generatedIndex++
			return token, nil
		},
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	var storedHash string
	var fingerprint string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT token_hash, token_fingerprint FROM api_tokens WHERE id = ?", bootstrapTokenID).Scan(&storedHash, &fingerprint); err != nil {
		t.Fatalf("read initial token: %v", err)
	}
	if storedHash != apikey.HashToken(adminToken) || fingerprint != tokenFingerprint(storedHash) {
		t.Fatalf("stored token metadata = hash %q fingerprint %q", storedHash, fingerprint)
	}
	if strings.Contains(storedHash, adminToken) || strings.Contains(fingerprint, adminToken) {
		t.Fatal("initial token plaintext was persisted")
	}

	unauthorized := performHubAPIRequest(t, service, http.MethodGet, "/health", "wrong-secret-token", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d body = %s", unauthorized.Code, unauthorized.Body.String())
	}
	createdResponse := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/tokens", adminToken, map[string]any{"name": "Worker", "scope": "worker"})
	if createdResponse.Code != http.StatusCreated || createdResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create token status = %d headers = %v body = %s", createdResponse.Code, createdResponse.Header(), createdResponse.Body.String())
	}
	var created tokenResponse
	decodeHubResponse(t, createdResponse, &created)
	if created.Token != workerToken || created.Scope != apiScopeWorker {
		t.Fatalf("created token = %#v", created)
	}
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT token_hash FROM api_tokens WHERE id = ?", created.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read worker token hash: %v", err)
	}
	if storedHash != apikey.HashToken(workerToken) || strings.Contains(storedHash, workerToken) {
		t.Fatalf("stored worker token hash = %q", storedHash)
	}

	if response := performHubAPIRequest(t, service, http.MethodGet, "/health", workerToken, nil); response.Code != http.StatusOK {
		t.Fatalf("worker read status = %d body = %s", response.Code, response.Body.String())
	}
	if response := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/tokens", workerToken, map[string]any{"name": "Denied", "scope": "worker"}); response.Code != http.StatusForbidden {
		t.Fatalf("worker admin status = %d body = %s", response.Code, response.Body.String())
	}
	rotatedResponse := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/tokens/"+created.ID+"/rotate", adminToken, map[string]any{})
	if rotatedResponse.Code != http.StatusOK || rotatedResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rotate token status = %d body = %s", rotatedResponse.Code, rotatedResponse.Body.String())
	}
	var rotated tokenResponse
	decodeHubResponse(t, rotatedResponse, &rotated)
	if rotated.Token != rotatedToken || rotated.RotatedAt.IsZero() {
		t.Fatalf("rotated token = %#v", rotated)
	}
	if response := performHubAPIRequest(t, service, http.MethodGet, "/health", workerToken, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("old worker token status = %d, want 401", response.Code)
	}
	if response := performHubAPIRequest(t, service, http.MethodGet, "/health", rotatedToken, nil); response.Code != http.StatusOK {
		t.Fatalf("rotated worker token status = %d body = %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{adminToken, workerToken, rotatedToken, "wrong-secret-token"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain API token %q", secret)
		}
	}
	if err := service.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	restarted, err := Open(t.Context(), Config{DatabasePath: databasePath, InitialAdminToken: []byte("different-startup-token-must-not-undo-rotation"), Logger: discardLogger()})
	if err != nil {
		t.Fatalf("restart Hub: %v", err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Fatalf("close restarted Hub: %v", err)
		}
	})
	if response := performHubAPIRequest(t, restarted, http.MethodGet, "/health", rotatedToken, nil); response.Code != http.StatusOK {
		t.Fatalf("rotated token after restart status = %d body = %s", response.Code, response.Body.String())
	}
	if err := restarted.database.db.QueryRowContext(t.Context(), "SELECT token_hash FROM api_tokens WHERE id = ?", bootstrapTokenID).Scan(&storedHash); err != nil {
		t.Fatalf("read bootstrap token after restart: %v", err)
	}
	if storedHash != apikey.HashToken(adminToken) {
		t.Fatalf("restart replaced bootstrap token hash = %q", storedHash)
	}
}

func TestWorkItemAPIUsesStableFilteredCursorsAndDetailTimeline(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	repositoryID, firstID := seedProjection(t, service.database.db)
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE repositories SET last_reconciled_at = ? WHERE id = ?", formatHubTime(now), repositoryID); err != nil {
		t.Fatalf("mark repository fresh: %v", err)
	}
	var workflowID int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT workflow_state_id FROM issues WHERE id = ?", firstID).Scan(&workflowID); err != nil {
		t.Fatalf("read workflow state: %v", err)
	}
	secondID := insertHubTestIssue(t, service, repositoryID, 2, "I_second", "open", &workflowID)
	thirdID := insertHubTestIssue(t, service, repositoryID, 3, "I_third", "open", &workflowID)
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET body = ? WHERE id = ?", "Complete issue body", thirdID); err != nil {
		t.Fatalf("set complete issue body: %v", err)
	}
	entries := []struct {
		id       int64
		rank     string
		priority int
	}{
		{firstID, "m", tracker.QueuePriorityLow},
		{secondID, "z", tracker.QueuePriorityUrgent},
		{thirdID, "a", tracker.QueuePriorityUrgent},
	}
	for _, entry := range entries {
		if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO queue_entries (issue_id, workflow_state_id, scope, state, rank, priority_override, created_at, updated_at) VALUES (?, ?, 'fleet', 'Todo', ?, ?, ?, ?)", entry.id, workflowID, entry.rank, entry.priority, testTimestamp, testTimestamp); err != nil {
			t.Fatalf("insert queue entry: %v", err)
		}
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET labels_json = '[\"api\"]', assignees_json = '[\"worker-a\"]' WHERE id = ?", thirdID); err != nil {
		t.Fatalf("update filters: %v", err)
	}

	firstPage := performHubAPIRequest(t, service, http.MethodGet, "/api/v1/work-items?repository=digitaldrywood%2Fdetent&readiness=ready&priority=urgent&limit=1", testHubAdminToken, nil)
	if firstPage.Code != http.StatusOK {
		t.Fatalf("first page status = %d body = %s", firstPage.Code, firstPage.Body.String())
	}
	var page workItemListResponse
	decodeHubResponse(t, firstPage, &page)
	if len(page.Items) != 1 || page.Items[0].ID != tracker.WorkItemID(thirdID) || page.NextCursor == "" {
		t.Fatalf("first page = %#v", page)
	}
	secondPagePath := "/api/v1/work-items?repository=digitaldrywood%2Fdetent&readiness=ready&priority=urgent&limit=1&cursor=" + url.QueryEscape(page.NextCursor)
	secondPage := performHubAPIRequest(t, service, http.MethodGet, secondPagePath, testHubAdminToken, nil)
	page = workItemListResponse{}
	decodeHubResponse(t, secondPage, &page)
	if len(page.Items) != 1 || page.Items[0].ID != tracker.WorkItemID(secondID) || page.NextCursor != "" {
		t.Fatalf("second page = %#v", page)
	}
	filtered := performHubAPIRequest(t, service, http.MethodGet, "/api/v1/work-items?label=api&assignee=worker-a&sort=identifier", testHubAdminToken, nil)
	page = workItemListResponse{}
	decodeHubResponse(t, filtered, &page)
	if len(page.Items) != 1 || page.Items[0].ID != tracker.WorkItemID(thirdID) {
		t.Fatalf("filtered page = %#v", page)
	}
	invalidCursor := performHubAPIRequest(t, service, http.MethodGet, secondPagePath+"&sort=updated", testHubAdminToken, nil)
	if invalidCursor.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched cursor status = %d body = %s", invalidCursor.Code, invalidCursor.Body.String())
	}

	for index := 1; index <= 2; index++ {
		if _, err := service.database.db.ExecContext(t.Context(), `
INSERT INTO work_events (issue_id, kind, payload_json, occurred_at, recorded_at)
VALUES (?, ?, '{}', ?, ?)`, thirdID, fmt.Sprintf("event_%d", index), formatHubTime(now.Add(time.Duration(index)*time.Minute)), formatHubTime(now)); err != nil {
			t.Fatalf("insert timeline event: %v", err)
		}
	}
	desired, err := json.Marshal(WorkpadDesired{Phase: "implementation", Body: "Hub API workpad", Marker: workpadMarker})
	if err != nil {
		t.Fatalf("marshal workpad: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), `
INSERT INTO github_outbox (idempotency_key, repository_id, issue_id, mutation_kind, desired_json, status, created_at, updated_at)
VALUES ('workpad-api', ?, ?, ?, ?, 'pending', ?, ?)`, repositoryID, thirdID, MutationWorkpad, string(desired), formatHubTime(now), formatHubTime(now)); err != nil {
		t.Fatalf("insert workpad: %v", err)
	}
	detailResponse := performHubAPIRequest(t, service, http.MethodGet, fmt.Sprintf("/api/v1/work-items/%d?timeline_limit=1", thirdID), testHubAdminToken, nil)
	var detail workItemDetailResponse
	decodeHubResponse(t, detailResponse, &detail)
	if detail.ID != tracker.WorkItemID(thirdID) || detail.Body != "Complete issue body" || detail.Workpad == nil || detail.Workpad.Body != "Hub API workpad" || len(detail.Timeline) != 1 || detail.TimelineNextCursor == "" {
		t.Fatalf("detail response = %#v", detail)
	}
	nextTimeline := performHubAPIRequest(t, service, http.MethodGet, fmt.Sprintf("/api/v1/work-items/%d?timeline_limit=1&timeline_cursor=%s", thirdID, url.QueryEscape(detail.TimelineNextCursor)), testHubAdminToken, nil)
	detail = workItemDetailResponse{}
	decodeHubResponse(t, nextTimeline, &detail)
	if len(detail.Timeline) != 1 || detail.Timeline[0].Kind != "event_2" || detail.TimelineNextCursor != "" {
		t.Fatalf("next timeline = %#v", detail.Timeline)
	}
}

func TestClaimNextAPIIsAtomicAndFenced(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	repositoryID, issueID := seedProjection(t, service.database.db)
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE repositories SET last_reconciled_at = ? WHERE id = ?", formatHubTime(now), repositoryID); err != nil {
		t.Fatalf("mark repository fresh: %v", err)
	}
	registered := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/machines/register", testHubAdminToken, map[string]any{
		"id": "machine-a", "hostname": "worker-a", "display_name": "Worker A", "capacity": 10, "version": "v1", "capabilities": map[string]any{"go": true},
	})
	if registered.Code != http.StatusOK {
		t.Fatalf("register machine status = %d body = %s", registered.Code, registered.Body.String())
	}

	const attempts = 10
	statuses := make(chan int, attempts)
	bodies := make(chan []byte, attempts)
	var wait sync.WaitGroup
	for index := range attempts {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			payload, _ := json.Marshal(map[string]any{"machine_id": "machine-a", "session_id": fmt.Sprintf("session-%d", index), "ttl_seconds": 90})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/claims", bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, request)
			statuses <- response.Code
			bodies <- append([]byte(nil), response.Body.Bytes()...)
		}(index)
	}
	wait.Wait()
	close(statuses)
	close(bodies)
	successes := 0
	var lease tracker.Lease
	for status := range statuses {
		if status == http.StatusCreated {
			successes++
		}
	}
	for body := range bodies {
		var candidate tracker.Lease
		if json.Unmarshal(body, &candidate) == nil && candidate.ID != "" {
			lease = candidate
		}
	}
	if successes != 1 || lease.WorkItemID != tracker.WorkItemID(issueID) || lease.FencingToken <= 0 {
		t.Fatalf("claim results = %d successes lease %#v", successes, lease)
	}
	var active int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM leases WHERE issue_id = ? AND released_at IS NULL", issueID).Scan(&active); err != nil {
		t.Fatalf("count active leases: %v", err)
	}
	if active != 1 {
		t.Fatalf("active leases = %d, want 1", active)
	}

	staleEvent := performHubAPIRequest(t, service, http.MethodPost, fmt.Sprintf("/api/v1/work-items/%d/events", issueID), testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken + 1, "kind": "progress"})
	if staleEvent.Code != http.StatusConflict {
		t.Fatalf("stale event status = %d body = %s", staleEvent.Code, staleEvent.Body.String())
	}
	validEvent := performHubAPIRequest(t, service, http.MethodPost, fmt.Sprintf("/api/v1/work-items/%d/events", issueID), testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken, "kind": "progress", "payload": map[string]any{"step": "test"}})
	if validEvent.Code != http.StatusCreated {
		t.Fatalf("valid event status = %d body = %s", validEvent.Code, validEvent.Body.String())
	}
	renewed := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/leases/"+string(lease.ID)+"/renew", testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken, "ttl_seconds": 120})
	if renewed.Code != http.StatusOK {
		t.Fatalf("renew status = %d body = %s", renewed.Code, renewed.Body.String())
	}
	released := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/leases/"+string(lease.ID)+"/release", testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken, "reason": "complete"})
	if released.Code != http.StatusNoContent {
		t.Fatalf("release status = %d body = %s", released.Code, released.Body.String())
	}
	otherRepository, err := service.database.db.ExecContext(t.Context(), "INSERT INTO repositories (github_node_id, github_owner, github_name, last_reconciled_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)", "R_other", "Acme", "Other", formatHubTime(now), testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert other repository: %v", err)
	}
	otherRepositoryID, err := otherRepository.LastInsertId()
	if err != nil {
		t.Fatalf("other repository ID: %v", err)
	}
	otherWorkflow, err := service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (repository_id, github_node_id, source_name, detent_state, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)", otherRepositoryID, "WS_other_todo", "Todo", "Todo", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert other workflow state: %v", err)
	}
	otherWorkflowID, err := otherWorkflow.LastInsertId()
	if err != nil {
		t.Fatalf("other workflow state ID: %v", err)
	}
	otherRepositoryIssueID := insertHubTestIssue(t, service, otherRepositoryID, 1, "I_other_repository", "open", &otherWorkflowID)
	repositoryScoped := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/claims", testHubAdminToken, map[string]any{
		"machine_id": "machine-a", "session_id": "repository-session", "ttl_seconds": 90, "repositories": []string{"acme/other"},
	})
	if repositoryScoped.Code != http.StatusCreated {
		t.Fatalf("repository-scoped claim status = %d body = %s", repositoryScoped.Code, repositoryScoped.Body.String())
	}
	var repositoryLease tracker.Lease
	decodeHubResponse(t, repositoryScoped, &repositoryLease)
	if repositoryLease.WorkItemID != tracker.WorkItemID(otherRepositoryIssueID) {
		t.Fatalf("repository-scoped work item = %d, want %d", repositoryLease.WorkItemID, otherRepositoryIssueID)
	}
	if response := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/leases/"+string(repositoryLease.ID)+"/release", testHubAdminToken, map[string]any{"fencing_token": repositoryLease.FencingToken}); response.Code != http.StatusNoContent {
		t.Fatalf("release repository-scoped claim status = %d body = %s", response.Code, response.Body.String())
	}
	heartbeat := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/machines/machine-a/heartbeat", testHubAdminToken, map[string]any{"capacity": 4, "version": "v2"})
	if heartbeat.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d body = %s", heartbeat.Code, heartbeat.Body.String())
	}
	var machine machineResponse
	decodeHubResponse(t, heartbeat, &machine)
	if machine.Capacity != 4 || machine.Version != "v2" || machine.Capabilities["go"] != true {
		t.Fatalf("heartbeat machine = %#v", machine)
	}
	specific := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/claims", testHubAdminToken, map[string]any{
		"work_item_id": issueID, "machine_id": "machine-a", "session_id": "specific-session", "ttl_seconds": 90,
	})
	if specific.Code != http.StatusCreated {
		t.Fatalf("specific claim status = %d body = %s", specific.Code, specific.Body.String())
	}
	var workflowID int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT workflow_state_id FROM issues WHERE id = ?", issueID).Scan(&workflowID); err != nil {
		t.Fatalf("read workflow state: %v", err)
	}
	otherID := insertHubTestIssue(t, service, repositoryID, 2, "I_other_claim", "open", &workflowID)
	mismatched := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/claims", testHubAdminToken, map[string]any{
		"work_item_id": otherID, "machine_id": "machine-a", "session_id": "specific-session", "ttl_seconds": 90,
	})
	if mismatched.Code != http.StatusConflict {
		t.Fatalf("mismatched specific retry status = %d body = %s", mismatched.Code, mismatched.Body.String())
	}
}

func TestClaimNextAPIAppliesAuthorizationFiltersBeforeClaim(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	repositoryID, rejectedID := seedProjection(t, service.database.db)
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE repositories SET last_reconciled_at = ? WHERE id = ?", formatHubTime(now), repositoryID); err != nil {
		t.Fatalf("mark repository fresh: %v", err)
	}
	var workflowID int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT workflow_state_id FROM issues WHERE id = ?", rejectedID).Scan(&workflowID); err != nil {
		t.Fatalf("read workflow state: %v", err)
	}
	acceptedID := insertHubTestIssue(t, service, repositoryID, 2, "I_authorized", "open", &workflowID)
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET author_login = 'mallory', labels_json = '[\"detent:todo\",\"hold\"]', assignees_json = '[\"worker-b\"]' WHERE id = ?", rejectedID); err != nil {
		t.Fatalf("update rejected issue: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET author_login = 'Alice', labels_json = '[\"detent:todo\",\"backend\"]', assignees_json = '[\"worker-a\"]' WHERE id = ?", acceptedID); err != nil {
		t.Fatalf("update accepted issue: %v", err)
	}
	registered := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/machines/register", testHubAdminToken, map[string]any{
		"id": "machine-a", "hostname": "worker-a", "capacity": 1, "version": "v1",
	})
	if registered.Code != http.StatusOK {
		t.Fatalf("register machine status = %d body = %s", registered.Code, registered.Body.String())
	}
	response := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/claims", testHubAdminToken, map[string]any{
		"machine_id": "machine-a", "session_id": "filtered-session", "ttl_seconds": 90,
		"authors": []string{"alice"}, "assignees": []string{"worker-a"},
		"label_include": []string{"detent:todo", "backend"}, "label_exclude": []string{"hold"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("filtered claim status = %d body = %s", response.Code, response.Body.String())
	}
	var lease tracker.Lease
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != tracker.WorkItemID(acceptedID) {
		t.Fatalf("filtered claim work item = %d, want %d", lease.WorkItemID, acceptedID)
	}
}

func TestClaimNextAPIRejectsUnsafeRepositorySyncHealth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		setup func(*testing.T, *Service, int64)
	}{
		{
			name: "stale",
			setup: func(t *testing.T, service *Service, repositoryID int64) {
				t.Helper()
				if _, err := service.database.db.ExecContext(t.Context(), "UPDATE repositories SET last_reconciled_at = NULL WHERE id = ?", repositoryID); err != nil {
					t.Fatalf("mark repository stale: %v", err)
				}
			},
		},
		{
			name: "error",
			setup: func(t *testing.T, service *Service, repositoryID int64) {
				t.Helper()
				if _, err := service.database.db.ExecContext(t.Context(), "UPDATE repositories SET last_reconciled_at = ? WHERE id = ?", formatHubTime(now), repositoryID); err != nil {
					t.Fatalf("mark repository reconciled: %v", err)
				}
				if err := service.database.recordCheckpointFailure(t.Context(), repositoryID, checkpointIncremental, now, errors.New("sync failed")); err != nil {
					t.Fatalf("record sync failure: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
			repositoryID, _ := seedProjection(t, service.database.db)
			test.setup(t, service, repositoryID)
			registered := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/machines/register", testHubAdminToken, map[string]any{
				"id": "machine-a", "hostname": "worker-a", "capacity": 1, "version": "v1",
			})
			if registered.Code != http.StatusOK {
				t.Fatalf("register machine status = %d body = %s", registered.Code, registered.Body.String())
			}
			claimed := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/claims", testHubAdminToken, map[string]any{
				"machine_id": "machine-a", "session_id": "unsafe-sync-session", "ttl_seconds": 90,
			})
			if claimed.Code != http.StatusConflict {
				t.Fatalf("claim status = %d body = %s", claimed.Code, claimed.Body.String())
			}
		})
	}
}

func TestOperatorMutationsAreTypedAuditedAndScoped(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, issueID := seedProjection(t, service.database.db)
	var workflowID int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT workflow_state_id FROM issues WHERE id = ?", issueID).Scan(&workflowID); err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	blockerID := insertHubTestIssue(t, service, repositoryID, 2, "I_blocker_api", "open", &workflowID)
	result, err := service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (repository_id, source_name, detent_state, dispatchable, created_at, updated_at) VALUES (?, 'In Progress', 'In Progress', 0, ?, ?)", repositoryID, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	inProgressID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("workflow ID: %v", err)
	}
	seedHubAPIToken(t, service, "operator-token", "operator-secret-token", apiScopeOperator)
	seedHubAPIToken(t, service, "worker-token", "worker-secret-token", apiScopeWorker)

	denied := performHubAPIRequest(t, service, http.MethodPost, fmt.Sprintf("/api/v1/work-items/%d/order", issueID), "worker-secret-token", map[string]any{"scope": "fleet", "state": "Todo", "rank": "a"})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("worker operator mutation status = %d body = %s", denied.Code, denied.Body.String())
	}
	requests := []struct {
		path    string
		payload map[string]any
		status  int
	}{
		{fmt.Sprintf("/api/v1/work-items/%d/workflow", issueID), map[string]any{"workflow_state_id": inProgressID, "label": "detent:in-progress", "idempotency_key": "workflow-api"}, http.StatusAccepted},
		{fmt.Sprintf("/api/v1/work-items/%d/dependencies", issueID), map[string]any{"action": "add", "blocker_work_item_id": blockerID, "provenance": "operator"}, http.StatusOK},
		{fmt.Sprintf("/api/v1/work-items/%d/order", issueID), map[string]any{"scope": "fleet", "state": "In Progress", "rank": "rank-a"}, http.StatusOK},
		{fmt.Sprintf("/api/v1/work-items/%d/priority", issueID), map[string]any{"scope": "fleet", "state": "In Progress", "priority": "high", "idempotency_key": "priority-api"}, http.StatusAccepted},
	}
	for _, request := range requests {
		response := performHubAPIRequest(t, service, http.MethodPost, request.path, "operator-secret-token", request.payload)
		if response.Code != request.status {
			t.Fatalf("POST %s status = %d body = %s", request.path, response.Code, response.Body.String())
		}
	}
	var gotWorkflowID int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT workflow_state_id FROM issues WHERE id = ?", issueID).Scan(&gotWorkflowID); err != nil || gotWorkflowID != inProgressID {
		t.Fatalf("workflow state = %d error = %v, want %d", gotWorkflowID, err, inProgressID)
	}
	var dependencies int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM issue_dependencies WHERE blocker_issue_id = ? AND dependent_issue_id = ?", blockerID, issueID).Scan(&dependencies); err != nil || dependencies != 1 {
		t.Fatalf("dependency count = %d error = %v", dependencies, err)
	}
	var rank string
	var priority int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT rank, priority_override FROM queue_entries WHERE issue_id = ? AND scope = 'fleet'", issueID).Scan(&rank, &priority); err != nil {
		t.Fatalf("read queue entry: %v", err)
	}
	if rank != "rank-a" || priority != tracker.QueuePriorityHigh {
		t.Fatalf("queue entry = rank %q priority %d", rank, priority)
	}
	var outboxCount int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM github_outbox WHERE idempotency_key IN ('workflow-api', 'priority-api')").Scan(&outboxCount); err != nil || outboxCount != 2 {
		t.Fatalf("outbox count = %d error = %v", outboxCount, err)
	}
	var auditCount int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM work_events WHERE issue_id = ? AND kind IN ('dependency_add', 'queue_order_changed')", issueID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("operator audit count = %d error = %v", auditCount, err)
	}
	cycle := performHubAPIRequest(t, service, http.MethodPost, fmt.Sprintf("/api/v1/work-items/%d/dependencies", blockerID), "operator-secret-token", map[string]any{"action": "add", "blocker_work_item_id": issueID})
	if cycle.Code != http.StatusUnprocessableEntity {
		t.Fatalf("dependency cycle status = %d body = %s", cycle.Code, cycle.Body.String())
	}
}

func TestListenerSecurityRequiresLoopbackTLSOrTrustedProxy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{name: "IPv4 loopback", config: Config{ListenAddress: "127.0.0.1:7777"}},
		{name: "IPv6 loopback", config: Config{ListenAddress: "[::1]:7777"}},
		{name: "localhost", config: Config{ListenAddress: "localhost:7777"}},
		{name: "wildcard rejected", config: Config{ListenAddress: "0.0.0.0:7777"}, wantErr: true},
		{name: "public TLS", config: Config{ListenAddress: "0.0.0.0:7777", TLSCertFile: "server.crt", TLSKeyFile: "server.key"}},
		{name: "trusted proxy", config: Config{ListenAddress: "0.0.0.0:7777", TrustedProxy: true}},
		{name: "incomplete TLS", config: Config{ListenAddress: "127.0.0.1:7777", TLSCertFile: "server.crt"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateListenerSecurity(test.config)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateListenerSecurity() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestControlPlaneRoutesRequireAuthentication(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/api/v1/work-items"},
		{http.MethodGet, "/api/v1/work-items/1"},
		{http.MethodPost, "/api/v1/claims"},
		{http.MethodPost, "/api/v1/leases/lease/renew"},
		{http.MethodPost, "/api/v1/leases/lease/release"},
		{http.MethodPost, "/api/v1/work-items/1/events"},
		{http.MethodPost, "/api/v1/work-items/1/workflow"},
		{http.MethodPost, "/api/v1/work-items/1/dependencies"},
		{http.MethodPost, "/api/v1/work-items/1/priority"},
		{http.MethodPost, "/api/v1/work-items/1/order"},
		{http.MethodPost, "/api/v1/machines/register"},
		{http.MethodPost, "/api/v1/machines/machine/heartbeat"},
		{http.MethodGet, "/api/v1/repositories/freshness"},
		{http.MethodGet, "/api/v1/outbox/health"},
		{http.MethodPost, "/api/v1/tokens"},
		{http.MethodPost, "/api/v1/tokens/token/rotate"},
		{http.MethodDelete, "/api/v1/tokens/token"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			response := performHubAPIRequest(t, service, route.method, route.path, "", nil)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d body = %s, want 401", response.Code, response.Body.String())
			}
		})
	}
}

func TestHealthListsUseStableCursors(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, issueID := seedProjection(t, service.database.db)
	if _, err := service.database.db.ExecContext(t.Context(), `
INSERT INTO repositories (github_node_id, github_owner, github_name, created_at, updated_at)
VALUES ('R_second', 'example', 'second', ?, ?)`, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert second repository: %v", err)
	}
	first := performHubAPIRequest(t, service, http.MethodGet, "/api/v1/repositories/freshness?limit=1", testHubAdminToken, nil)
	var repositories repositoryFreshnessResponse
	decodeHubResponse(t, first, &repositories)
	if len(repositories.Repositories) != 1 || repositories.NextCursor == "" || repositories.Summary.Total != 2 {
		t.Fatalf("repository first page = %#v", repositories)
	}
	second := performHubAPIRequest(t, service, http.MethodGet, "/api/v1/repositories/freshness?limit=1&cursor="+url.QueryEscape(repositories.NextCursor), testHubAdminToken, nil)
	repositories = repositoryFreshnessResponse{}
	decodeHubResponse(t, second, &repositories)
	if len(repositories.Repositories) != 1 || repositories.NextCursor != "" || repositories.Summary.Total != 2 {
		t.Fatalf("repository second page = %#v", repositories)
	}

	for index := 1; index <= 2; index++ {
		if _, err := service.database.db.ExecContext(t.Context(), `
INSERT INTO github_outbox (
  idempotency_key, repository_id, issue_id, mutation_kind, desired_json,
  status, terminal_error, operator_action, created_at, updated_at
) VALUES (?, ?, ?, ?, '{}', 'dead_letter', 'failed', ?, ?, ?)`,
			fmt.Sprintf("dead-letter-%d", index), repositoryID, issueID, MutationWorkpad,
			fmt.Sprintf("retry action %d", index), testTimestamp, testTimestamp); err != nil {
			t.Fatalf("insert dead letter: %v", err)
		}
	}
	outboxFirst := performHubAPIRequest(t, service, http.MethodGet, "/api/v1/outbox/health?limit=1", testHubAdminToken, nil)
	var outbox outboxHealthResponse
	decodeHubResponse(t, outboxFirst, &outbox)
	if len(outbox.OperatorActions) != 1 || outbox.NextCursor == "" || outbox.DeadLetters != 2 {
		t.Fatalf("outbox first page = %#v", outbox)
	}
	outboxSecond := performHubAPIRequest(t, service, http.MethodGet, "/api/v1/outbox/health?limit=1&cursor="+url.QueryEscape(outbox.NextCursor), testHubAdminToken, nil)
	outbox = outboxHealthResponse{}
	decodeHubResponse(t, outboxSecond, &outbox)
	if len(outbox.OperatorActions) != 1 || outbox.NextCursor != "" || outbox.DeadLetters != 2 {
		t.Fatalf("outbox second page = %#v", outbox)
	}
}

func performHubAPIRequest(t *testing.T, service *Service, method string, path string, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response status %d body %s: %v", response.Code, response.Body.String(), err)
	}
}

func seedHubAPIToken(t *testing.T, service *Service, id string, token string, scope apiScope) {
	t.Helper()
	hash := apikey.HashToken(token)
	if _, err := service.database.db.ExecContext(t.Context(), `
INSERT INTO api_tokens (id, name, token_hash, token_fingerprint, scope, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, id, id, hash, tokenFingerprint(hash), scope, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert Hub API token: %v", err)
	}
}
