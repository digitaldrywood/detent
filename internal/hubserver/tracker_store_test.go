package hubserver

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTrackerReadsNormalizedWorkItems(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, issueID := seedProjection(t, service.database.db)
	result, err := service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (repository_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, 1, 0, ?, ?)", repositoryID, "Done", "Done", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert terminal workflow state: %v", err)
	}
	terminalWorkflowID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("terminal workflow state ID: %v", err)
	}
	blockerID := insertHubTestIssue(t, service, repositoryID, 2, "I_blocker", "closed", &terminalWorkflowID)
	dependentID := insertHubTestIssue(t, service, repositoryID, 3, "I_dependent", "open", nil)
	if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO issue_dependencies (blocker_issue_id, dependent_issue_id, provenance, created_at, updated_at) VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)", blockerID, issueID, "native", testTimestamp, testTimestamp, issueID, dependentID, "native", testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert dependencies: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO queue_entries (issue_id, workflow_state_id, scope, state, rank, priority_override, created_at, updated_at) SELECT ?, workflow_state_id, ?, ?, ?, ?, ?, ? FROM issues WHERE id = ?", issueID, "fleet", "Todo", "a0", 2, testTimestamp, testTimestamp, issueID); err != nil {
		t.Fatalf("insert queue entry: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET body = ?, labels_json = ?, assignees_json = ?, github_database_id = ? WHERE id = ?", " Work item body ", `["hub"," Feature ","hub"]`, `["detent-bot"]`, 2068, issueID); err != nil {
		t.Fatalf("update issue projection: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE pull_requests SET checks_summary_json = ?, reviews_summary_json = ?, mergeable_state = ?, merge_ready = 1, merge_readiness_refreshed_at = ? WHERE issue_id = ?", `{"status":"completed","passed":3,"total":3}`, `{"decision":"approved","approvals":1}`, "clean", testTimestamp, issueID); err != nil {
		t.Fatalf("update pull request projection: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO machines (id, hostname, display_name, capacity, version, last_heartbeat_at, registered_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", "machine-a", "worker-a", "Worker A", 1, "test", testTimestamp, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO leases (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", "lease-a", issueID, "machine-a", "session-a", future, testTimestamp, testTimestamp, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert lease: %v", err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO github_outbox (idempotency_key, repository_id, issue_id, mutation_kind, desired_json, status, attempts, next_retry_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", "sync-a", repositoryID, issueID, "label", `{}`, "pending", 1, future, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert outbox: %v", err)
	}

	items, err := service.Tracker().GetWorkItems(t.Context(), []tracker.WorkItemID{tracker.WorkItemID(issueID)})
	if err != nil {
		t.Fatalf("GetWorkItems() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("GetWorkItems() count = %d, want 1", len(items))
	}
	item := items[0]
	if item.Repository.ID != tracker.RepositoryID(repositoryID) || item.Repository.Owner != "digitaldrywood" || item.GitHub.NodeID != "I_issue" || item.GitHub.Number != 1 || item.GitHub.DatabaseID == nil || *item.GitHub.DatabaseID != 2068 {
		t.Errorf("source identity = repository %#v GitHub %#v", item.Repository, item.GitHub)
	}
	if item.BodyExcerpt != "Work item body" || strings.Join(item.Labels, ",") != "hub,Feature" || strings.Join(item.Assignees, ",") != "detent-bot" {
		t.Errorf("normalized content = body %q labels %v assignees %v", item.BodyExcerpt, item.Labels, item.Assignees)
	}
	if item.Queue == nil || item.Queue.Scope != "fleet" || item.Queue.PriorityRank == nil || *item.Queue.PriorityRank != 2 {
		t.Errorf("Queue = %#v", item.Queue)
	}
	if len(item.Blockers) != 1 || item.Blockers[0].ID != tracker.WorkItemID(blockerID) || len(item.Dependents) != 1 || item.Dependents[0].ID != tracker.WorkItemID(dependentID) {
		t.Errorf("relationships = blockers %#v dependents %#v", item.Blockers, item.Dependents)
	}
	if item.ActiveLease == nil || item.ActiveLease.ID != "lease-a" || item.ActiveLease.Machine.Hostname != "worker-a" || item.ActiveLease.SessionID != "session-a" {
		t.Errorf("ActiveLease = %#v", item.ActiveLease)
	}
	if len(item.PullRequests) != 1 || item.PullRequests[0].HeadRef != "feature" || item.PullRequests[0].Checks.Passed != 3 || item.PullRequests[0].Reviews.Decision != "approved" || !item.PullRequests[0].Merge.Ready || item.PullRequests[0].Merge.State != "clean" {
		t.Errorf("PullRequests = %#v", item.PullRequests)
	}
	if item.SyncStatus != tracker.SyncStatusRetrying {
		t.Errorf("SyncStatus = %q, want retrying", item.SyncStatus)
	}
	if item.Dispatchability.Dispatchable || len(item.Dispatchability.Reasons) != 1 || item.Dispatchability.Reasons[0].Code != tracker.DispatchReasonLeaseActive {
		t.Errorf("Dispatchability = %#v, want active lease reason", item.Dispatchability)
	}

	candidates, err := service.Tracker().ListCandidates(t.Context(), tracker.CandidateQuery{RepositoryIDs: []tracker.RepositoryID{tracker.RepositoryID(repositoryID)}, WorkflowStates: []string{" todo ", "TODO"}, Scope: "fleet", Limit: 10})
	if err != nil {
		t.Fatalf("ListCandidates() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != tracker.WorkItemID(issueID) {
		t.Fatalf("ListCandidates() = %#v, want issue %d", candidates, issueID)
	}
}

func TestTrackerReadsPartialProjection(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, _ := seedProjection(t, service.database.db)
	issueID := insertHubTestIssue(t, service, repositoryID, 4, "I_partial", "mystery", nil)
	items, err := service.Tracker().GetWorkItems(t.Context(), []tracker.WorkItemID{tracker.WorkItemID(issueID), tracker.WorkItemID(issueID)})
	if err != nil {
		t.Fatalf("GetWorkItems() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("GetWorkItems() count = %d, want 1", len(items))
	}
	item := items[0]
	if item.SourceState != tracker.SourceStateUnknown || item.WorkflowState != nil || item.Labels == nil || item.Assignees == nil || item.Blockers == nil || item.Dependents == nil || item.PullRequests == nil || item.Dispatchability.Reasons == nil {
		t.Fatalf("partial WorkItem was not normalized: %#v", item)
	}
	wantReasons := []tracker.DispatchReasonCode{tracker.DispatchReasonSourceStateUnknown, tracker.DispatchReasonWorkflowStateMissing}
	if len(item.Dispatchability.Reasons) != len(wantReasons) {
		t.Fatalf("Dispatchability reasons = %#v, want %v", item.Dispatchability.Reasons, wantReasons)
	}
	for i, want := range wantReasons {
		if item.Dispatchability.Reasons[i].Code != want {
			t.Errorf("Dispatchability reason %d = %q, want %q", i, item.Dispatchability.Reasons[i].Code, want)
		}
	}
}

func TestTrackerCandidatesApplyQueueOrderBeforeLimit(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, lowPriorityID := seedProjection(t, service.database.db)
	var workflowID int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT workflow_state_id FROM issues WHERE id = ?", lowPriorityID).Scan(&workflowID); err != nil {
		t.Fatalf("read workflow state: %v", err)
	}
	highPriorityLaterID := insertHubTestIssue(t, service, repositoryID, 2, "I_high_later", "open", &workflowID)
	highPriorityEarlierID := insertHubTestIssue(t, service, repositoryID, 3, "I_high_earlier", "open", &workflowID)
	entries := []struct {
		issueID  int64
		rank     string
		priority int
	}{
		{issueID: lowPriorityID, rank: "00", priority: 2},
		{issueID: highPriorityLaterID, rank: "z0", priority: 0},
		{issueID: highPriorityEarlierID, rank: "a0", priority: 0},
	}
	for _, entry := range entries {
		if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO queue_entries (issue_id, workflow_state_id, scope, state, rank, priority_override, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", entry.issueID, workflowID, "fleet", "Todo", entry.rank, entry.priority, testTimestamp, testTimestamp); err != nil {
			t.Fatalf("insert queue entry for issue %d: %v", entry.issueID, err)
		}
	}

	candidates, err := service.Tracker().ListCandidates(t.Context(), tracker.CandidateQuery{Scope: "fleet", Limit: 2})
	if err != nil {
		t.Fatalf("ListCandidates() error = %v", err)
	}
	want := []tracker.WorkItemID{tracker.WorkItemID(highPriorityEarlierID), tracker.WorkItemID(highPriorityLaterID)}
	if len(candidates) != len(want) {
		t.Fatalf("ListCandidates() count = %d, want %d", len(candidates), len(want))
	}
	for i := range want {
		if candidates[i].ID != want[i] {
			t.Errorf("ListCandidates()[%d].ID = %d, want %d", i, candidates[i].ID, want[i])
		}
	}
}

func TestTrackerCandidatesExcludeCancelledState(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, activeID := seedProjection(t, service.database.db)
	result, err := service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (repository_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, 0, 1, ?, ?)", repositoryID, "Cancelled", "Cancelled", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert cancelled workflow state: %v", err)
	}
	cancelledWorkflowID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("cancelled workflow state ID: %v", err)
	}
	cancelledID := insertHubTestIssue(t, service, repositoryID, 2, "I_cancelled", "open", &cancelledWorkflowID)

	candidates, err := service.Tracker().ListCandidates(t.Context(), tracker.CandidateQuery{WorkflowStates: []string{"Todo", "Cancelled"}})
	if err != nil {
		t.Fatalf("ListCandidates() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != tracker.WorkItemID(activeID) {
		t.Fatalf("ListCandidates() = %#v, want active issue %d and not cancelled issue %d", candidates, activeID, cancelledID)
	}
}

func TestTrackerCandidatesRequireConfiguredCandidateState(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, activeID := seedProjection(t, service.database.db)
	result, err := service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (repository_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, 0, 0, ?, ?)", repositoryID, "Backlog", "Backlog", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert non-candidate workflow state: %v", err)
	}
	backlogWorkflowID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("non-candidate workflow state ID: %v", err)
	}
	backlogID := insertHubTestIssue(t, service, repositoryID, 2, "I_backlog", "open", &backlogWorkflowID)

	candidates, err := service.Tracker().ListCandidates(t.Context(), tracker.CandidateQuery{WorkflowStates: []string{"Todo", "Backlog"}})
	if err != nil {
		t.Fatalf("ListCandidates() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != tracker.WorkItemID(activeID) {
		t.Fatalf("ListCandidates() = %#v, want active issue %d and not non-candidate issue %d", candidates, activeID, backlogID)
	}
}

func TestTrackerReadValidation(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	tests := []struct {
		name string
		read func() error
		want error
	}{
		{name: "negative limit", read: func() error {
			_, err := service.Tracker().ListCandidates(t.Context(), tracker.CandidateQuery{Limit: -1})
			return err
		}, want: tracker.ErrInvalidCandidateQuery},
		{name: "empty workflow states", read: func() error {
			_, err := service.Tracker().ListCandidates(t.Context(), tracker.CandidateQuery{WorkflowStates: []string{" "}})
			return err
		}, want: tracker.ErrInvalidCandidateQuery},
		{name: "invalid repository ID", read: func() error {
			_, err := service.Tracker().ListCandidates(t.Context(), tracker.CandidateQuery{RepositoryIDs: []tracker.RepositoryID{0}})
			return err
		}, want: tracker.ErrInvalidCandidateQuery},
		{name: "invalid ID", read: func() error {
			_, err := service.Tracker().GetWorkItems(t.Context(), []tracker.WorkItemID{0})
			return err
		}, want: tracker.ErrInvalidWorkItemID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.read(); !errors.Is(err, test.want) {
				t.Fatalf("read error = %v, want %v", err, test.want)
			}
		})
	}
}

func insertHubTestIssue(t *testing.T, service *Service, repositoryID int64, number int, nodeID string, state string, workflowID *int64) int64 {
	t.Helper()
	result, err := service.database.db.ExecContext(t.Context(), "INSERT INTO issues (repository_id, workflow_state_id, github_node_id, github_number, title, url, github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", repositoryID, workflowID, nodeID, number, "Issue", "https://example.test/"+nodeID, state, "v1", testTimestamp, testTimestamp, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	issueID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("issue ID: %v", err)
	}
	return issueID
}
