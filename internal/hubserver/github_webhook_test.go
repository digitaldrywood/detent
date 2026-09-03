package hubserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testWebhookSecret = "webhook-secret"

func TestGitHubWebhookRejectsUnverifiableSignaturesWithoutReceipt(t *testing.T) {
	t.Parallel()

	service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), time.Now)
	payload := completeIssueWebhookPayload(t, "Issue", "2026-09-02T12:00:00Z")
	tests := []struct {
		name      string
		signature string
	}{
		{name: "missing"},
		{name: "malformed", signature: "sha256=not-hex"},
		{name: "wrong secret", signature: signWebhookPayload("wrong-secret", payload)},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := sendWebhookRequest(t, service, fmt.Sprintf("invalid-%d", i), "issues", payload, test.signature)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d body = %s, want %d", response.Code, response.Body.String(), http.StatusUnauthorized)
			}
		})
	}

	var receipts int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM github_webhook_inbox").Scan(&receipts); err != nil {
		t.Fatalf("count webhook receipts: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("webhook receipts = %d, want 0", receipts)
	}
}

func TestGitHubWebhookDeduplicatesDeliveriesAndRejectsConflicts(t *testing.T) {
	t.Parallel()

	service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), time.Now)
	payload := completeIssueWebhookPayload(t, "Original title", "2026-09-02T12:00:00Z")
	for attempt := range 2 {
		response := sendSignedWebhookRequest(t, service, "duplicate-delivery", "issues", payload)
		if response.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status = %d body = %s, want %d", attempt, response.Code, response.Body.String(), http.StatusAccepted)
		}
		var body webhookResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode attempt %d response: %v", attempt, err)
		}
		if body.Duplicate != (attempt == 1) {
			t.Fatalf("attempt %d duplicate = %t, want %t", attempt, body.Duplicate, attempt == 1)
		}
	}

	conflictingPayload := completeIssueWebhookPayload(t, "Conflicting title", "2026-09-02T12:00:00Z")
	response := sendSignedWebhookRequest(t, service, "duplicate-delivery", "issues", conflictingPayload)
	if response.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d body = %s, want %d", response.Code, response.Body.String(), http.StatusConflict)
	}

	var receipts int
	var redeliveries int
	var status string
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*), max(redelivery_count), max(status)
		FROM github_webhook_inbox
	`).Scan(&receipts, &redeliveries, &status); err != nil {
		t.Fatalf("read webhook receipt: %v", err)
	}
	if receipts != 1 || redeliveries != 1 || status != "processed" {
		t.Fatalf("receipt = count %d redeliveries %d status %q, want 1, 1, processed", receipts, redeliveries, status)
	}
	var issues int
	var title string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*), max(title) FROM issues").Scan(&issues, &title); err != nil {
		t.Fatalf("read issue projection: %v", err)
	}
	if issues != 1 || title != "Original title" {
		t.Fatalf("issue projection = count %d title %q, want one original issue", issues, title)
	}
}

func TestGitHubWebhookAcknowledgesDurableReceiptWhenProcessingFails(t *testing.T) {
	t.Parallel()

	service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), time.Now)
	for _, repository := range []struct {
		nodeID string
		owner  string
		name   string
	}{
		{nodeID: "R_conflicting_node", owner: "other", name: "repository"},
		{nodeID: "R_existing_name", owner: "digitaldrywood", name: "detent"},
	} {
		if _, err := service.database.db.ExecContext(t.Context(), `
			INSERT INTO repositories (github_node_id, github_owner, github_name, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, repository.nodeID, repository.owner, repository.name, testTimestamp, testTimestamp); err != nil {
			t.Fatalf("insert repository %s: %v", repository.nodeID, err)
		}
	}
	payload := strings.Replace(
		completeIssueWebhookPayload(t, "Issue", "2026-09-02T12:00:00Z"),
		`"node_id":"R_repo"`,
		`"node_id":"R_conflicting_node"`,
		1,
	)
	response := sendSignedWebhookRequest(t, service, "processing-failure", "issues", payload)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s, want %d", response.Code, response.Body.String(), http.StatusAccepted)
	}

	var status string
	var attempts int
	var payloadCount int
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT i.status, i.attempts, COUNT(p.inbox_id)
		FROM github_webhook_inbox i
		LEFT JOIN github_webhook_payloads p ON p.inbox_id = i.id
		WHERE i.delivery_id = ?
		GROUP BY i.id
	`, "processing-failure").Scan(&status, &attempts, &payloadCount); err != nil {
		t.Fatalf("read failed durable receipt: %v", err)
	}
	if status != "failed" || attempts != 1 || payloadCount != 1 {
		t.Fatalf("durable receipt = status %q attempts %d payloads %d, want failed, 1, 1", status, attempts, payloadCount)
	}
}

func TestGitHubWebhookSourceOrderingConverges(t *testing.T) {
	t.Parallel()

	older := completeIssueWebhookPayload(t, "Older title", "2026-09-02T11:00:00Z")
	newer := completeIssueWebhookPayload(t, "Newer title", "2026-09-02T12:00:00Z")
	tests := []struct {
		name     string
		payloads []string
	}{
		{name: "older then newer", payloads: []string{older, newer}},
		{name: "newer then older", payloads: []string{newer, older}},
	}
	var want issueProjectionSnapshot
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), func() time.Time {
				return time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
			})
			for deliveryIndex, payload := range test.payloads {
				response := sendSignedWebhookRequest(t, service, fmt.Sprintf("ordering-%d-%d", index, deliveryIndex), "issues", payload)
				if response.Code != http.StatusAccepted {
					t.Fatalf("delivery %d status = %d body = %s", deliveryIndex, response.Code, response.Body.String())
				}
			}
			got := readIssueProjectionSnapshot(t, service)
			if got.Title != "Newer title" || got.SourceUpdatedAt != "2026-09-02T12:00:00.000000000Z" {
				t.Fatalf("projection = %#v, want newer source", got)
			}
			if index == 0 {
				want = got
			} else if got != want {
				t.Fatalf("projection = %#v, want order-independent %#v", got, want)
			}
		})
	}
}

func TestGitHubWebhookEqualTimestampConflictConvergesAndHydrates(t *testing.T) {
	t.Parallel()

	first := completeIssueWebhookPayload(t, "First competing title", "2026-09-02T12:00:00Z")
	second := completeIssueWebhookPayload(t, "Second competing title", "2026-09-02T12:00:00Z")
	orders := [][]string{{first, second}, {second, first}}
	var want issueProjectionSnapshot
	for index, payloads := range orders {
		service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), func() time.Time {
			return time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
		})
		for deliveryIndex, payload := range payloads {
			response := sendSignedWebhookRequest(t, service, fmt.Sprintf("tie-%d-%d", index, deliveryIndex), "issues", payload)
			if response.Code != http.StatusAccepted {
				t.Fatalf("order %d delivery %d status = %d body = %s", index, deliveryIndex, response.Code, response.Body.String())
			}
		}
		got := readIssueProjectionSnapshot(t, service)
		if index == 0 {
			want = got
		} else if got != want {
			t.Fatalf("projection = %#v, want deterministic tie result %#v", got, want)
		}
		var hydrationCount int
		var kind string
		var key string
		var reason string
		if err := service.database.db.QueryRowContext(t.Context(), `
			SELECT COUNT(*), max(object_kind), max(object_key), max(reason)
			FROM github_hydration_requests WHERE status = 'pending'
		`).Scan(&hydrationCount, &kind, &key, &reason); err != nil {
			t.Fatalf("read conflict hydration: %v", err)
		}
		if hydrationCount != 1 || kind != "issue" || key != "2069" || reason != "source_version_conflict" {
			t.Fatalf("hydration = count %d kind %q key %q reason %q, want one issue conflict", hydrationCount, kind, key, reason)
		}
	}
}

func TestGitHubWebhookPartialPayloadRequestsOnlyTargetedHydration(t *testing.T) {
	t.Parallel()

	service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), time.Now)
	payload := `{"action":"edited","repository":{"full_name":"digitaldrywood/detent"},"issue":{"number":2069}}`
	response := sendSignedWebhookRequest(t, service, "partial-issue", "issues", payload)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s, want %d", response.Code, response.Body.String(), http.StatusAccepted)
	}

	var kind string
	var key string
	var number int
	var reason string
	var repository string
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT object_kind, object_key, github_number, reason, repository_full_name
		FROM github_hydration_requests
	`).Scan(&kind, &key, &number, &reason, &repository); err != nil {
		t.Fatalf("read targeted hydration: %v", err)
	}
	if kind != "issue" || key != "2069" || number != 2069 || reason != "partial_payload" || repository != "digitaldrywood/detent" {
		t.Fatalf("hydration = %s %s %d %s %s, want exact issue target", kind, key, number, reason, repository)
	}
	var issues int
	var repositories int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT (SELECT COUNT(*) FROM issues), (SELECT COUNT(*) FROM repositories)").Scan(&issues, &repositories); err != nil {
		t.Fatalf("count partial projections: %v", err)
	}
	if issues != 0 || repositories != 0 {
		t.Fatalf("partial projections = issues %d repositories %d, want no incomplete rows", issues, repositories)
	}
}

func TestGitHubWebhookCoalescesNewestHydrationSource(t *testing.T) {
	t.Parallel()

	older := `{"repository":{"full_name":"digitaldrywood/detent"},"issue":{"number":2069,"updated_at":"2026-09-02T11:00:00Z"}}`
	newer := `{"repository":{"full_name":"digitaldrywood/detent"},"issue":{"number":2069,"updated_at":"2026-09-02T12:00:00Z"}}`
	newerDigest := sha256.Sum256([]byte(newer))
	wantVersion := "1:" + hex.EncodeToString(newerDigest[:])
	orders := [][]string{{older, newer}, {newer, older}}
	for orderIndex, payloads := range orders {
		service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), time.Now)
		for deliveryIndex, payload := range payloads {
			response := sendSignedWebhookRequest(t, service, fmt.Sprintf("partial-order-%d-%d", orderIndex, deliveryIndex), "issues", payload)
			if response.Code != http.StatusAccepted {
				t.Fatalf("order %d delivery %d status = %d body = %s", orderIndex, deliveryIndex, response.Code, response.Body.String())
			}
		}
		var requestCount int
		var updatedAt string
		var version string
		if err := service.database.db.QueryRowContext(t.Context(), `
			SELECT request_count, requested_source_updated_at, requested_source_version
			FROM github_hydration_requests
		`).Scan(&requestCount, &updatedAt, &version); err != nil {
			t.Fatalf("read coalesced hydration source: %v", err)
		}
		if requestCount != 2 || updatedAt != "2026-09-02T12:00:00.000000000Z" || version != wantVersion {
			t.Fatalf("coalesced source = count %d updated %q version %q, want newest source", requestCount, updatedAt, version)
		}
	}
}

func TestGitHubWebhookTargetsChecksWithoutRepositoryRefresh(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		event    string
		payload  string
		wantKind string
		wantKey  string
	}{
		{
			name:     "pull request checks",
			event:    "check_run",
			payload:  `{"repository":{"full_name":"digitaldrywood/detent"},"check_run":{"head_sha":"abc123","pull_requests":[{"number":42}]}}`,
			wantKind: "pull_request_checks",
			wantKey:  "42",
		},
		{
			name:     "commit checks",
			event:    "check_suite",
			payload:  `{"repository":{"full_name":"digitaldrywood/detent"},"check_suite":{"head_sha":"def456","pull_requests":[]}}`,
			wantKind: "commit_checks",
			wantKey:  "def456",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), time.Now)
			response := sendSignedWebhookRequest(t, service, fmt.Sprintf("checks-%d", index), test.event, test.payload)
			if response.Code != http.StatusAccepted {
				t.Fatalf("status = %d body = %s, want %d", response.Code, response.Body.String(), http.StatusAccepted)
			}
			var kind string
			var key string
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT object_kind, object_key FROM github_hydration_requests").Scan(&kind, &key); err != nil {
				t.Fatalf("read check hydration: %v", err)
			}
			if kind != test.wantKind || key != test.wantKey {
				t.Fatalf("hydration = %s:%s, want %s:%s", kind, key, test.wantKind, test.wantKey)
			}
		})
	}
}

func TestGitHubWebhookUpdatesPullRequestWithoutClearingSummaries(t *testing.T) {
	t.Parallel()

	service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), time.Now)
	initial := completePullRequestWebhookPayload(t, "Initial PR", "2026-09-02T11:00:00Z")
	response := sendSignedWebhookRequest(t, service, "pr-initial", "pull_request", initial)
	if response.Code != http.StatusAccepted {
		t.Fatalf("initial status = %d body = %s", response.Code, response.Body.String())
	}
	if _, err := service.database.db.ExecContext(t.Context(), `
		UPDATE pull_requests
		SET checks_summary_json = '{"total":2}', reviews_summary_json = '{"approvals":1}'
	`); err != nil {
		t.Fatalf("seed pull request summaries: %v", err)
	}
	updated := completePullRequestWebhookPayload(t, "Updated PR", "2026-09-02T12:00:00Z")
	response = sendSignedWebhookRequest(t, service, "pr-updated", "pull_request", updated)
	if response.Code != http.StatusAccepted {
		t.Fatalf("updated status = %d body = %s", response.Code, response.Body.String())
	}
	var title string
	var checks string
	var reviews string
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT title, checks_summary_json, reviews_summary_json FROM pull_requests
	`).Scan(&title, &checks, &reviews); err != nil {
		t.Fatalf("read pull request projection: %v", err)
	}
	if title != "Updated PR" || checks != `{"total":2}` || reviews != `{"approvals":1}` {
		t.Fatalf("pull request = title %q checks %s reviews %s, want updated base with preserved summaries", title, checks, reviews)
	}
}

func TestGitHubWebhookMapsAndClearsWorkflowStateFromLabels(t *testing.T) {
	t.Parallel()

	service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), time.Now)
	initial := completeIssueWebhookPayload(t, "Initial issue", "2026-09-02T11:00:00Z")
	response := sendSignedWebhookRequest(t, service, "workflow-initial", "issues", initial)
	if response.Code != http.StatusAccepted {
		t.Fatalf("initial status = %d body = %s", response.Code, response.Body.String())
	}
	var repositoryID int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM repositories").Scan(&repositoryID); err != nil {
		t.Fatalf("read repository ID: %v", err)
	}
	result, err := service.database.db.ExecContext(t.Context(), `
		INSERT INTO workflow_states (
			repository_id, github_node_id, source_name, detent_state,
			dispatchable, created_at, updated_at
		) VALUES (?, ?, ?, ?, 1, ?, ?)
	`, repositoryID, "WS_todo", "detent:todo", "Todo", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert workflow state: %v", err)
	}
	wantWorkflowStateID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read workflow state ID: %v", err)
	}
	withWorkflow := completeIssueWebhookPayload(t, "Mapped issue", "2026-09-02T12:00:00Z")
	response = sendSignedWebhookRequest(t, service, "workflow-mapped", "issues", withWorkflow)
	if response.Code != http.StatusAccepted {
		t.Fatalf("mapped status = %d body = %s", response.Code, response.Body.String())
	}
	var workflowStateID sql.NullInt64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT workflow_state_id FROM issues").Scan(&workflowStateID); err != nil {
		t.Fatalf("read mapped workflow state: %v", err)
	}
	if !workflowStateID.Valid || workflowStateID.Int64 != wantWorkflowStateID {
		t.Fatalf("workflow state = %#v, want %d", workflowStateID, wantWorkflowStateID)
	}

	var withoutWorkflow map[string]any
	if err := json.Unmarshal([]byte(completeIssueWebhookPayload(t, "Unmapped issue", "2026-09-02T13:00:00Z")), &withoutWorkflow); err != nil {
		t.Fatalf("decode issue without workflow label: %v", err)
	}
	issue := withoutWorkflow["issue"].(map[string]any)
	issue["labels"] = []any{map[string]any{"name": "feature"}}
	encoded, err := json.Marshal(withoutWorkflow)
	if err != nil {
		t.Fatalf("encode issue without workflow label: %v", err)
	}
	response = sendSignedWebhookRequest(t, service, "workflow-cleared", "issues", string(encoded))
	if response.Code != http.StatusAccepted {
		t.Fatalf("cleared status = %d body = %s", response.Code, response.Body.String())
	}
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT workflow_state_id FROM issues").Scan(&workflowStateID); err != nil {
		t.Fatalf("read cleared workflow state: %v", err)
	}
	if workflowStateID.Valid {
		t.Fatalf("workflow state = %#v, want NULL after label removal", workflowStateID)
	}
}

func TestGitHubWebhookDeletedIssueCannotRemainSchedulable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		deliveries         []string
		wantState          string
		wantWorkflowMapped bool
	}{
		{
			name: "newer deletion",
			deliveries: []string{
				completeIssueWebhookPayload(t, "Mapped issue", "2026-09-02T12:00:00Z"),
				issueWebhookPayloadWithAction(t, "deleted", "Mapped issue", "2026-09-02T13:00:00Z"),
			},
			wantState: "closed",
		},
		{
			name: "stale deletion",
			deliveries: []string{
				completeIssueWebhookPayload(t, "Mapped issue", "2026-09-02T13:00:00Z"),
				issueWebhookPayloadWithAction(t, "deleted", "Mapped issue", "2026-09-02T12:00:00Z"),
			},
			wantState:          "open",
			wantWorkflowMapped: true,
		},
		{
			name: "equal timestamp deletion last",
			deliveries: []string{
				completeIssueWebhookPayload(t, "Mapped issue", "2026-09-02T12:00:00Z"),
				issueWebhookPayloadWithAction(t, "deleted", "Mapped issue", "2026-09-02T12:00:00Z"),
			},
			wantState: "closed",
		},
		{
			name: "equal timestamp deletion first",
			deliveries: []string{
				issueWebhookPayloadWithAction(t, "deleted", "Mapped issue", "2026-09-02T12:00:00Z"),
				completeIssueWebhookPayload(t, "Mapped issue", "2026-09-02T12:00:00Z"),
			},
			wantState: "closed",
		},
	}
	for testIndex, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := openWebhookTestService(t, filepath.Join(t.TempDir(), "hub.db"), time.Now)
			response := sendSignedWebhookRequest(t, service, fmt.Sprintf("deleted-seed-%d", testIndex), "issues", completeIssueWebhookPayload(t, "Seed issue", "2026-09-02T11:00:00Z"))
			if response.Code != http.StatusAccepted {
				t.Fatalf("seed status = %d body = %s", response.Code, response.Body.String())
			}
			var repositoryID int64
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM repositories").Scan(&repositoryID); err != nil {
				t.Fatalf("read repository ID: %v", err)
			}
			result, err := service.database.db.ExecContext(t.Context(), `
				INSERT INTO workflow_states (
					repository_id, github_node_id, source_name, detent_state,
					dispatchable, created_at, updated_at
				) VALUES (?, ?, ?, ?, 1, ?, ?)
			`, repositoryID, "WS_todo", "detent:todo", "Todo", testTimestamp, testTimestamp)
			if err != nil {
				t.Fatalf("insert workflow state: %v", err)
			}
			wantWorkflowStateID, err := result.LastInsertId()
			if err != nil {
				t.Fatalf("read workflow state ID: %v", err)
			}
			for deliveryIndex, payload := range test.deliveries {
				response = sendSignedWebhookRequest(t, service, fmt.Sprintf("deleted-%d-%d", testIndex, deliveryIndex), "issues", payload)
				if response.Code != http.StatusAccepted {
					t.Fatalf("delivery %d status = %d body = %s", deliveryIndex, response.Code, response.Body.String())
				}
			}
			var state string
			var workflowStateID sql.NullInt64
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT github_state, workflow_state_id FROM issues").Scan(&state, &workflowStateID); err != nil {
				t.Fatalf("read issue scheduling state: %v", err)
			}
			if state != test.wantState {
				t.Fatalf("GitHub state = %q, want %q", state, test.wantState)
			}
			if workflowStateID.Valid != test.wantWorkflowMapped {
				t.Fatalf("workflow state = %#v, want mapped %t", workflowStateID, test.wantWorkflowMapped)
			}
			if test.wantWorkflowMapped && workflowStateID.Int64 != wantWorkflowStateID {
				t.Fatalf("workflow state ID = %d, want %d", workflowStateID.Int64, wantWorkflowStateID)
			}
		})
	}
}

func TestGitHubWebhookPayloadRetentionPreservesAuditAndProjection(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{
		DatabasePath:               filepath.Join(t.TempDir(), "hub.db"),
		GitHubWebhookSecret:        []byte(testWebhookSecret),
		WebhookPayloadRetention:    time.Hour,
		WebhookMaintenanceInterval: time.Hour,
		now:                        func() time.Time { return now },
	})
	payload := completeIssueWebhookPayload(t, "Retained issue", "2026-09-02T12:00:00Z")
	response := sendSignedWebhookRequest(t, service, "retention", "issues", payload)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	pendingPayload := []byte(`{}`)
	pendingDigest := sha256.Sum256(pendingPayload)
	if _, err := service.database.recordWebhook(t.Context(), webhookReceipt{
		DeliveryID:       "pending-retention",
		EventType:        "issues",
		HeadersJSON:      "{}",
		Payload:          pendingPayload,
		PayloadSHA256:    hex.EncodeToString(pendingDigest[:]),
		ReceivedAt:       now.Add(-2 * time.Hour),
		PayloadExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record pending retention receipt: %v", err)
	}
	deleted, err := service.database.purgeWebhookPayloads(t.Context(), now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("purgeWebhookPayloads() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("purged payloads = %d, want 1", deleted)
	}

	var payloads int
	var receipts int
	var issues int
	var payloadHash string
	var payloadBytes int
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM github_webhook_payloads),
			(SELECT COUNT(*) FROM github_webhook_inbox),
			(SELECT COUNT(*) FROM issues),
			(SELECT payload_sha256 FROM github_webhook_inbox WHERE delivery_id = 'retention'),
			(SELECT payload_bytes FROM github_webhook_inbox WHERE delivery_id = 'retention')
	`).Scan(&payloads, &receipts, &issues, &payloadHash, &payloadBytes); err != nil {
		t.Fatalf("read retained webhook state: %v", err)
	}
	if payloads != 1 || receipts != 2 || issues != 1 || payloadHash == "" || payloadBytes != len(payload) {
		t.Fatalf("retained state = payloads %d receipts %d issues %d hash %q bytes %d", payloads, receipts, issues, payloadHash, payloadBytes)
	}
}

func TestGitHubWebhookStartupReplaysPendingReceipt(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "hub.db")
	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
	service := openWebhookTestService(t, databasePath, func() time.Time { return now })
	payload := []byte(completeIssueWebhookPayload(t, "Replayed issue", "2026-09-02T12:00:00Z"))
	digest := sha256.Sum256(payload)
	if _, err := service.database.recordWebhook(t.Context(), webhookReceipt{
		DeliveryID:       "startup-replay",
		EventType:        "issues",
		HeadersJSON:      "{}",
		Payload:          payload,
		PayloadSHA256:    hex.EncodeToString(digest[:]),
		ReceivedAt:       now,
		PayloadExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("recordWebhook() error = %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened := openWebhookTestService(t, databasePath, func() time.Time { return now.Add(time.Minute) })
	var status string
	var title string
	if err := reopened.database.db.QueryRowContext(t.Context(), `
		SELECT i.status, issue.title
		FROM github_webhook_inbox i
		JOIN issues issue ON issue.github_node_id = 'I_issue'
		WHERE i.delivery_id = 'startup-replay'
	`).Scan(&status, &title); err != nil {
		t.Fatalf("read replayed webhook: %v", err)
	}
	if status != "processed" || title != "Replayed issue" {
		t.Fatalf("replayed state = status %q title %q, want processed replayed issue", status, title)
	}
}

type issueProjectionSnapshot struct {
	Title           string
	Body            string
	State           string
	LabelsJSON      string
	AssigneesJSON   string
	SourceVersion   string
	SourceUpdatedAt string
}

func readIssueProjectionSnapshot(t *testing.T, service *Service) issueProjectionSnapshot {
	t.Helper()
	var snapshot issueProjectionSnapshot
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT title, body, github_state, labels_json, assignees_json, source_version, source_updated_at
		FROM issues WHERE github_node_id = 'I_issue'
	`).Scan(
		&snapshot.Title,
		&snapshot.Body,
		&snapshot.State,
		&snapshot.LabelsJSON,
		&snapshot.AssigneesJSON,
		&snapshot.SourceVersion,
		&snapshot.SourceUpdatedAt,
	); err != nil {
		t.Fatalf("read issue projection: %v", err)
	}
	return snapshot
}

func openWebhookTestService(t *testing.T, databasePath string, now func() time.Time) *Service {
	t.Helper()
	return openTestService(t, Config{
		DatabasePath:               databasePath,
		GitHubWebhookSecret:        []byte(testWebhookSecret),
		WebhookMaintenanceInterval: time.Hour,
		now:                        now,
	})
}

func sendSignedWebhookRequest(t *testing.T, service *Service, deliveryID string, event string, payload string) *httptest.ResponseRecorder {
	t.Helper()
	return sendWebhookRequest(t, service, deliveryID, event, payload, signWebhookPayload(testWebhookSecret, payload))
}

func sendWebhookRequest(t *testing.T, service *Service, deliveryID string, event string, payload string, signature string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-Hub-Signature-256", signature)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func signWebhookPayload(secret string, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func completeIssueWebhookPayload(t *testing.T, title string, updatedAt string) string {
	t.Helper()
	payload := map[string]any{
		"action": "edited",
		"repository": map[string]any{
			"id":         100,
			"node_id":    "R_repo",
			"name":       "detent",
			"full_name":  "digitaldrywood/detent",
			"owner":      map[string]any{"login": "digitaldrywood"},
			"updated_at": updatedAt,
		},
		"issue": map[string]any{
			"id":         200,
			"node_id":    "I_issue",
			"number":     2069,
			"title":      title,
			"body":       "Issue body",
			"html_url":   "https://github.com/digitaldrywood/detent/issues/2069",
			"state":      "open",
			"labels":     []any{map[string]any{"name": "feature"}, map[string]any{"name": "detent:todo"}},
			"assignees":  []any{map[string]any{"login": "corylanou"}},
			"created_at": "2026-09-01T12:00:00Z",
			"updated_at": updatedAt,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode issue webhook payload: %v", err)
	}
	return string(encoded)
}

func issueWebhookPayloadWithAction(t *testing.T, action string, title string, updatedAt string) string {
	t.Helper()
	payload := make(map[string]any)
	if err := json.Unmarshal([]byte(completeIssueWebhookPayload(t, title, updatedAt)), &payload); err != nil {
		t.Fatalf("decode issue webhook payload: %v", err)
	}
	payload["action"] = action
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode issue webhook payload: %v", err)
	}
	return string(encoded)
}

func completePullRequestWebhookPayload(t *testing.T, title string, updatedAt string) string {
	t.Helper()
	payload := map[string]any{
		"action": "edited",
		"repository": map[string]any{
			"id":         100,
			"node_id":    "R_repo",
			"name":       "detent",
			"full_name":  "digitaldrywood/detent",
			"owner":      map[string]any{"login": "digitaldrywood"},
			"updated_at": updatedAt,
		},
		"pull_request": map[string]any{
			"id":         300,
			"node_id":    "PR_node",
			"number":     2070,
			"title":      title,
			"html_url":   "https://github.com/digitaldrywood/detent/pull/2070",
			"state":      "open",
			"draft":      false,
			"head":       map[string]any{"ref": "feature", "sha": "abc123"},
			"base":       map[string]any{"ref": "main", "sha": "def456"},
			"created_at": "2026-09-01T12:00:00Z",
			"updated_at": updatedAt,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode pull request webhook payload: %v", err)
	}
	return string(encoded)
}
