package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestFullReconcileRepairsDroppedWebhookDrift(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-time.Minute)
	backend := &scriptedReconcileBackend{
		steps: []reconcileStep{
			{snapshot: ReconcileSnapshot{
				Repository: RepositorySource{NodeID: "R_repo", DatabaseID: int64Pointer(100), Owner: "new-owner", Name: "renamed", UpdatedAt: updatedAt},
				Issues: []IssueSource{
					{
						NodeID: "I_issue", DatabaseID: int64Pointer(200), Number: 1, Title: "Repaired title", Body: "Repaired body",
						URL: "https://example.test/1", State: "closed", Labels: []string{"Done"}, Assignees: []string{"alice"},
						CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: updatedAt,
					},
					{
						NodeID: "I_new", DatabaseID: int64Pointer(300), Number: 3, Title: "Missed issue", Body: "Created during webhook gap",
						URL: "https://example.test/3", State: "open", Labels: []string{"feature"}, CreatedAt: updatedAt, UpdatedAt: updatedAt,
					},
				},
			}},
		},
	}
	service := openTestService(t, Config{
		DatabasePath:     filepath.Join(t.TempDir(), "hub.db"),
		ReconcileBackend: backend,
		now:              func() time.Time { return now },
	})
	if err := service.stopGitHubReconciliation(); err != nil {
		t.Fatalf("stop initial reconciliation: %v", err)
	}
	repositoryID, _ := seedProjection(t, service.database.db)
	if _, err := service.database.db.ExecContext(t.Context(), `
		INSERT INTO workflow_states (repository_id, github_node_id, source_name, detent_state, terminal, created_at, updated_at)
		VALUES (?, 'WS_done', 'Done', 'Done', 1, ?, ?)
	`, repositoryID, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert repaired workflow state: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), `
		INSERT INTO issues (
			repository_id, github_node_id, github_number, title, url, github_state,
			source_version, source_updated_at, synchronized_at, created_at, updated_at
		) VALUES (?, 'I_missing', 2, 'Deleted upstream', 'https://example.test/2', 'open', 'v1', ?, ?, ?, ?)
	`, repositoryID, testTimestamp, testTimestamp, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert missing issue: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), `
		INSERT INTO github_hydration_requests (
			repository_id, repository_full_name, object_kind, object_key, github_number,
			reason, requested_source_version, status, created_at, updated_at
		) VALUES (?, 'digitaldrywood/detent', 'issue', '1', 1, 'partial_payload', 'v2', 'pending', ?, ?)
	`, repositoryID, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert hydration request: %v", err)
	}

	if _, err := service.database.db.ExecContext(t.Context(), `
		UPDATE issues SET source_version = 'z:webhook-conflict', source_updated_at = ? WHERE github_node_id = 'I_issue'
	`, formatWebhookTime(updatedAt)); err != nil {
		t.Fatalf("seed equal-timestamp webhook conflict: %v", err)
	}
	targets, err := service.database.reconcileTargets(t.Context())
	if err != nil {
		t.Fatalf("reconcileTargets() error = %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	if err := service.reconcileRepository(t.Context(), targets[0], ReconcileFullRepair); err != nil {
		t.Fatalf("reconcileRepository() error = %v", err)
	}
	requests := backend.Requests()
	if len(requests) != 1 || requests[0].Mode != ReconcileFullRepair {
		t.Fatalf("reconcile requests = %#v, want one full repair", requests)
	}

	var owner string
	var name string
	var lastReconciled string
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT github_owner, github_name, last_reconciled_at FROM repositories WHERE id = ?
	`, repositoryID).Scan(&owner, &name, &lastReconciled); err != nil {
		t.Fatalf("read repaired repository: %v", err)
	}
	if owner != "new-owner" || name != "renamed" || lastReconciled != formatWebhookTime(now) {
		t.Fatalf("repository = %s/%s synced %q, want transferred repository at %q", owner, name, lastReconciled, formatWebhookTime(now))
	}
	type issueState struct {
		number   int
		title    string
		state    string
		labels   string
		workflow sql.NullString
	}
	rows, err := service.database.db.QueryContext(t.Context(), `
		SELECT i.github_number, i.title, i.github_state, i.labels_json, ws.detent_state
		FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
		ORDER BY i.github_number
	`)
	if err != nil {
		t.Fatalf("read repaired issues: %v", err)
	}
	defer rows.Close()
	var got []issueState
	for rows.Next() {
		var issue issueState
		if err := rows.Scan(&issue.number, &issue.title, &issue.state, &issue.labels, &issue.workflow); err != nil {
			t.Fatalf("scan repaired issue: %v", err)
		}
		got = append(got, issue)
	}
	want := []issueState{
		{number: 1, title: "Repaired title", state: "closed", labels: `["Done"]`, workflow: sql.NullString{String: "Done", Valid: true}},
		{number: 2, title: "Deleted upstream", state: "deleted", labels: `[]`},
		{number: 3, title: "Missed issue", state: "open", labels: `["feature"]`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("issues = %#v, want %#v", got, want)
	}
	var pullState string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT github_state FROM pull_requests WHERE github_node_id = 'PR_one'").Scan(&pullState); err != nil {
		t.Fatalf("read missing pull request: %v", err)
	}
	if pullState != "deleted" {
		t.Fatalf("missing pull request state = %q, want deleted", pullState)
	}
	var hydrationStatus string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT status FROM github_hydration_requests").Scan(&hydrationStatus); err != nil {
		t.Fatalf("read hydration status: %v", err)
	}
	if hydrationStatus != "completed" {
		t.Fatalf("hydration status = %q, want completed", hydrationStatus)
	}
}

func TestIncrementalReconcileUsesOldestCursorAndExposesFailures(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	recovery := start.Add(time.Minute)
	clock := &leaseTestClock{value: start}
	failure := errors.New("github unavailable")
	backend := &scriptedReconcileBackend{steps: []reconcileStep{
		{err: failure},
		{snapshot: ReconcileSnapshot{
			Repository: RepositorySource{NodeID: "R_repo", Owner: "digitaldrywood", Name: "detent", UpdatedAt: recovery},
			Issues: []IssueSource{{
				NodeID: "I_issue", Number: 1, Title: "Recovered", URL: "https://example.test/1", State: "open",
				CreatedAt: recovery.Add(-time.Hour), UpdatedAt: recovery,
			}},
			PullRequests: []PullRequestSource{{
				NodeID: "PR_one", Number: 1, Title: "PR", URL: "https://example.test/pr/1", State: "open",
				HeadRef: "feature", HeadSHA: "abc", BaseRef: "main", BaseSHA: "base", CreatedAt: recovery.Add(-time.Hour), UpdatedAt: recovery,
			}},
			PullRequestDetails: []PullRequestDetailSource{{
				Number: 1, HeadSHA: "abc", MergeableState: stringPointer("clean"),
				Checks:  &tracker.CheckSummary{Status: "completed", Conclusion: "success", Total: 1, Passed: 1},
				Reviews: &tracker.ReviewSummary{State: "approved", Decision: "approved", Approvals: 1},
			}},
		}},
	}}
	service := openTestService(t, Config{
		DatabasePath:      filepath.Join(t.TempDir(), "hub.db"),
		ReconcileInterval: time.Minute,
		ReconcileBackend:  backend,
		now:               clock.Now,
	})
	if err := service.stopGitHubReconciliation(); err != nil {
		t.Fatalf("stop initial reconciliation: %v", err)
	}
	repositoryID, _ := seedProjection(t, service.database.db)
	cursor := start.Add(-10 * time.Minute)
	hydrationSince := start.Add(-20 * time.Minute)
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE repositories SET reconcile_cursor = ? WHERE id = ?", formatWebhookTime(cursor), repositoryID); err != nil {
		t.Fatalf("set reconcile cursor: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), `
		INSERT INTO github_hydration_requests (
			repository_id, repository_full_name, object_kind, object_key, github_number,
			reason, requested_source_updated_at, requested_source_version, status, created_at, updated_at
		) VALUES (?, 'digitaldrywood/detent', 'issue', '1', 1, 'partial_payload', ?, 'v2', 'pending', ?, ?)
	`, repositoryID, formatWebhookTime(hydrationSince), testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert hydration request: %v", err)
	}

	targets, err := service.database.reconcileTargets(t.Context())
	if err != nil {
		t.Fatalf("reconcileTargets() error = %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	if err := service.reconcileRepository(t.Context(), targets[0], ReconcileIncremental); !errors.Is(err, failure) {
		t.Fatalf("reconcileRepository() error = %v, want failure", err)
	}
	requests := backend.Requests()
	if len(requests) != 1 || requests[0].Since == nil || !requests[0].Since.Equal(hydrationSince) || len(requests[0].Hydrations) != 1 {
		t.Fatalf("incremental request = %#v, want since %s", requests, hydrationSince)
	}
	result, err := service.database.repositoryFreshness(t.Context(), start, time.Minute)
	if err != nil {
		t.Fatalf("repositoryFreshness() error = %v", err)
	}
	if len(result.Repositories) != 1 || result.Repositories[0].Status != "error" || result.Repositories[0].LastReconcileError == nil || result.Repositories[0].LastReconcileError.Message != failure.Error() {
		t.Fatalf("freshness after failure = %#v, want visible reconcile error", result)
	}

	clock.Advance(time.Minute)
	targets, err = service.database.reconcileTargets(t.Context())
	if err != nil {
		t.Fatalf("reconcileTargets() after failure error = %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets after failure = %d, want 1", len(targets))
	}
	if err := service.reconcileRepository(t.Context(), targets[0], ReconcileIncremental); err != nil {
		t.Fatalf("reconcileRepository() recovery error = %v", err)
	}
	result, err = service.database.repositoryFreshness(t.Context(), recovery, time.Minute)
	if err != nil {
		t.Fatalf("repositoryFreshness() after recovery error = %v", err)
	}
	freshness := result.Repositories[0]
	if freshness.Status != "fresh" || freshness.LastSuccessfulSyncAt == nil || !freshness.LastSuccessfulSyncAt.Equal(recovery) {
		t.Fatalf("freshness after recovery = %#v, want fresh at %s", freshness, recovery)
	}
	if freshness.LastReconcileError == nil || freshness.LastReconcileError.Message != failure.Error() {
		t.Fatalf("last reconcile error = %#v, want retained failure", freshness.LastReconcileError)
	}
	var storedCursor string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT reconcile_cursor FROM repositories WHERE id = ?", repositoryID).Scan(&storedCursor); err != nil {
		t.Fatalf("read advanced cursor: %v", err)
	}
	if storedCursor != formatWebhookTime(recovery) {
		t.Fatalf("reconcile cursor = %q, want %q", storedCursor, formatWebhookTime(recovery))
	}
	var checksJSON string
	var reviewsJSON string
	var mergeableState string
	var mergeReady bool
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT checks_summary_json, reviews_summary_json, mergeable_state, merge_ready
		FROM pull_requests WHERE github_node_id = 'PR_one'
	`).Scan(&checksJSON, &reviewsJSON, &mergeableState, &mergeReady); err != nil {
		t.Fatalf("read reconciled pull request details: %v", err)
	}
	if checksJSON != `{"status":"completed","conclusion":"success","total":1,"passed":1}` ||
		reviewsJSON != `{"state":"approved","decision":"approved","approvals":1}` || mergeableState != "clean" || !mergeReady {
		t.Fatalf("pull request details = checks %s reviews %s mergeable %q ready %t", checksJSON, reviewsJSON, mergeableState, mergeReady)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/repositories/freshness", nil)
	authorizeHubTestRequest(request)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("freshness endpoint status = %d body = %s", response.Code, response.Body.String())
	}
	var endpoint repositoryFreshnessResponse
	if err := json.Unmarshal(response.Body.Bytes(), &endpoint); err != nil {
		t.Fatalf("decode freshness endpoint: %v", err)
	}
	if endpoint.Summary.Fresh != 1 || endpoint.Summary.Error != 0 {
		t.Fatalf("freshness endpoint summary = %#v, want one fresh", endpoint.Summary)
	}
}

func TestReconcileDoesNotCompleteHydrationCreatedAfterCapture(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	backend := &scriptedReconcileBackend{steps: []reconcileStep{{snapshot: ReconcileSnapshot{
		Repository: RepositorySource{NodeID: "R_repo", Owner: "digitaldrywood", Name: "detent", UpdatedAt: now},
	}}}}
	service := openTestService(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"), ReconcileBackend: backend, now: func() time.Time { return now },
	})
	if err := service.stopGitHubReconciliation(); err != nil {
		t.Fatalf("stop initial reconciliation: %v", err)
	}
	repositoryID, _ := seedProjection(t, service.database.db)
	result, err := service.database.db.ExecContext(t.Context(), `
		INSERT INTO github_hydration_requests (
			repository_id, repository_full_name, object_kind, object_key, github_number,
			reason, requested_source_version, status, created_at, updated_at
		) VALUES (?, 'digitaldrywood/detent', 'issue', '1', 1, 'partial_payload', 'v1', 'pending', ?, ?)
	`, repositoryID, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert captured hydration request: %v", err)
	}
	capturedID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("captured hydration request ID: %v", err)
	}
	targets, err := service.database.reconcileTargets(t.Context())
	if err != nil || len(targets) != 1 || len(targets[0].Hydrations) != 1 {
		t.Fatalf("reconcileTargets() = %#v, %v, want one captured hydration", targets, err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE github_hydration_requests SET request_count = request_count + 1 WHERE id = ?", capturedID); err != nil {
		t.Fatalf("coalesce captured hydration request: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), `
		INSERT INTO github_hydration_requests (
			repository_id, repository_full_name, object_kind, object_key, github_number,
			reason, requested_source_version, status, created_at, updated_at
		) VALUES (?, 'digitaldrywood/detent', 'issue', '2', 2, 'partial_payload', 'v1', 'pending', ?, ?)
	`, repositoryID, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert later hydration request: %v", err)
	}
	if err := service.reconcileRepository(t.Context(), targets[0], ReconcileFullRepair); err != nil {
		t.Fatalf("reconcileRepository() error = %v", err)
	}
	rows, err := service.database.db.QueryContext(t.Context(), "SELECT object_key, status FROM github_hydration_requests ORDER BY id")
	if err != nil {
		t.Fatalf("read hydration statuses: %v", err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var key string
		var status string
		if err := rows.Scan(&key, &status); err != nil {
			t.Fatalf("scan hydration status: %v", err)
		}
		statuses = append(statuses, key+":"+status)
	}
	if !reflect.DeepEqual(statuses, []string{"1:pending", "2:pending"}) {
		t.Fatalf("hydration statuses = %#v, want both post-capture requests pending", statuses)
	}
}

type reconcileStep struct {
	snapshot ReconcileSnapshot
	err      error
}

type scriptedReconcileBackend struct {
	mu       sync.Mutex
	steps    []reconcileStep
	requests []ReconcileRequest
}

func (b *scriptedReconcileBackend) Reconcile(_ context.Context, request ReconcileRequest) (ReconcileSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, request)
	if len(b.steps) == 0 {
		return ReconcileSnapshot{}, errors.New("unexpected reconciliation request")
	}
	step := b.steps[0]
	b.steps = b.steps[1:]
	return step.snapshot, step.err
}

func (b *scriptedReconcileBackend) Requests() []ReconcileRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ReconcileRequest(nil), b.requests...)
}

func int64Pointer(value int64) *int64 {
	return &value
}

func stringPointer(value string) *string {
	return &value
}
