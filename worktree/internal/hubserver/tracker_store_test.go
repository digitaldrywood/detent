package hubserver

import (
	"testing"
)

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
